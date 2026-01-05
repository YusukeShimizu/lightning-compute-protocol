package provider

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bruwbird/lcp/go-lcpd/internal/computebackend"
	"github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"
	"github.com/bruwbird/lcp/go-lcpd/internal/envconfig"
	"github.com/bruwbird/lcp/go-lcpd/internal/jobstore"
	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
	"github.com/bruwbird/lcp/go-lcpd/internal/llm"
	"github.com/bruwbird/lcp/go-lcpd/internal/lndpeermsg"
	"github.com/bruwbird/lcp/go-lcpd/internal/peerdirectory"
	"github.com/bruwbird/lcp/go-lcpd/internal/protocolcompat"
	"github.com/bruwbird/lcp/go-lcpd/internal/replaystore"
	"go.uber.org/zap"
)

const (
	defaultQuoteTTLSeconds = uint64(300)

	defaultInvoiceExpirySlackSeconds      = uint64(5)
	defaultMaxEnvelopeExpiryWindowSeconds = uint64(600)

	defaultMaxStreamBytes = uint64(4_194_304)
	defaultMaxJobBytes    = uint64(8_388_608)

	contentTypeTextPlain       = "text/plain; charset=utf-8"
	contentTypeApplicationJSON = "application/json; charset=utf-8"
	contentTypeTextEventStream = "text/event-stream; charset=utf-8"
	contentEncodingIdentity    = "identity"
)

type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

type Messenger interface {
	SendCustomMessage(
		ctx context.Context,
		peerPubKey string,
		msgType lcpwire.MessageType,
		payload []byte,
	) error
}

type InvoiceRequest struct {
	DescriptionHash lcp.Hash32
	PriceMsat       uint64
	ExpirySeconds   uint64
}

type InvoiceResult struct {
	PaymentRequest string
	PaymentHash    lcp.Hash32
	AddIndex       uint64
}

type InvoiceCreator interface {
	CreateInvoice(ctx context.Context, req InvoiceRequest) (InvoiceResult, error)
	WaitForSettlement(ctx context.Context, paymentHash lcp.Hash32, addIndex uint64) error
}

type Handler struct {
	cfg       Config
	validator QuoteRequestValidator
	messenger Messenger
	invoices  InvoiceCreator
	backend   computebackend.Backend
	policy    llm.ExecutionPolicyProvider
	estimator llm.UsageEstimator
	jobs      *jobstore.Store
	replay    *replaystore.Store
	peers     *peerdirectory.Directory
	logger    *zap.SugaredLogger
	clock     Clock

	newMsgIDFn func() (lcpwire.MsgID, error)

	jobMu      sync.Mutex
	jobCancels map[jobstore.Key]context.CancelFunc
	inFlight   uint64
}

func NewHandler(
	cfg Config,
	validator QuoteRequestValidator,
	messenger Messenger,
	invoices InvoiceCreator,
	backend computebackend.Backend,
	policy llm.ExecutionPolicyProvider,
	estimator llm.UsageEstimator,
	jobs *jobstore.Store,
	replay *replaystore.Store,
	peers *peerdirectory.Directory,
	logger *zap.SugaredLogger,
) *Handler {
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	if backend == nil {
		backend = computebackend.NewDisabled()
	}
	if policy == nil {
		policy = llm.MustFixedExecutionPolicy(llm.DefaultMaxOutputTokens)
	}
	if estimator == nil {
		estimator = llm.NewApproxUsageEstimator()
	}

	return &Handler{
		cfg:        cfg,
		validator:  validator,
		messenger:  messenger,
		invoices:   invoices,
		backend:    backend,
		policy:     policy,
		estimator:  estimator,
		jobs:       jobs,
		replay:     replay,
		peers:      peers,
		logger:     logger.With("component", "provider"),
		clock:      realClock{},
		newMsgIDFn: newMsgID,
		jobCancels: make(map[jobstore.Key]context.CancelFunc),
	}
}

func (h *Handler) HandleInboundCustomMessage(
	ctx context.Context,
	msg lndpeermsg.InboundCustomMessage,
) {
	switch lcpwire.MessageType(msg.MsgType) {
	case lcpwire.MessageTypeQuoteRequest:
		h.handleQuoteRequest(ctx, msg)
	case lcpwire.MessageTypeStreamBegin:
		h.handleStreamBegin(ctx, msg)
	case lcpwire.MessageTypeStreamChunk:
		h.handleStreamChunk(ctx, msg)
	case lcpwire.MessageTypeStreamEnd:
		h.handleStreamEnd(ctx, msg)
	case lcpwire.MessageTypeCancel:
		h.handleCancel(ctx, msg)
	case lcpwire.MessageTypeManifest,
		lcpwire.MessageTypeQuoteResponse,
		lcpwire.MessageTypeResult,
		lcpwire.MessageTypeError:
		return
	default:
		return
	}
}

func (h *Handler) handleQuoteRequest(ctx context.Context, msg lndpeermsg.InboundCustomMessage) {
	req, err := lcpwire.DecodeQuoteRequest(msg.Payload)
	if err != nil {
		h.logger.Warnw(
			"decode lcp_quote_request failed",
			"peer_pub_key", msg.PeerPubKey,
			"err", err,
		)
		return
	}

	if h.rejectQuoteRequestIfProviderUnavailable(ctx, msg.PeerPubKey, req.Envelope) {
		return
	}

	remoteManifest := h.remoteManifestFor(msg.PeerPubKey)
	remoteMaxPayload := remoteMaxPayloadBytes(remoteManifest)

	if vErr := h.validator.ValidateQuoteRequest(req, remoteManifest); vErr != nil {
		h.sendError(ctx, msg.PeerPubKey, req.Envelope, vErr.Code, vErr.Message)
		return
	}

	if h.rejectQuoteRequestIfUnsupportedModel(ctx, msg.PeerPubKey, req) {
		return
	}

	now, ok := h.nowUnix()
	if !ok {
		return
	}
	if h.dropIfExpired(msg.PeerPubKey, req.Envelope, now, "drop expired lcp_quote_request") {
		return
	}

	if !h.checkReplay(msg.PeerPubKey, req.Envelope, now, "lcp_quote_request") {
		return
	}

	key := jobstore.Key{PeerPubKey: msg.PeerPubKey, JobID: req.Envelope.JobID}

	if h.jobs == nil {
		h.logger.Warnw("drop lcp_quote_request: job store is not configured")
		return
	}

	// Manifest exchange is mandatory in v0.2. If we don't have the peer's
	// manifest yet, reject with invalid_state.
	if remoteManifest == nil {
		h.sendError(
			ctx,
			msg.PeerPubKey,
			req.Envelope,
			lcpwire.ErrorCodeInvalidState,
			"manifest not exchanged",
		)
		return
	}

	if existing, found := h.jobs.Get(key); found {
		h.handleExistingQuoteRequest(ctx, msg.PeerPubKey, key, req, now, remoteMaxPayload, existing)
		return
	}

	reqCopy := req
	h.jobs.Upsert(jobstore.Job{
		PeerPubKey:    msg.PeerPubKey,
		JobID:         req.Envelope.JobID,
		State:         jobstore.StateAwaitingInput,
		QuoteRequest:  &reqCopy,
		InputStream:   nil,
		QuoteExpiry:   0,
		TermsHash:     nil,
		PaymentHash:   nil,
		QuoteResponse: nil,
	})
}

func (h *Handler) handleExistingQuoteRequest(
	ctx context.Context,
	peerPubKey string,
	key jobstore.Key,
	req lcpwire.QuoteRequest,
	now uint64,
	remoteMaxPayload uint32,
	existing jobstore.Job,
) {
	// If the requester retries the quote_request for the same job_id, we should
	// be idempotent and return the same quote_response if still valid.
	if existing.QuoteExpiry != 0 && existing.QuoteExpiry < now {
		h.sendError(ctx, peerPubKey, req.Envelope, lcpwire.ErrorCodeQuoteExpired, "quote expired")
		return
	}

	if existing.QuoteRequest != nil && !quoteRequestEqual(*existing.QuoteRequest, req) {
		h.sendError(
			ctx,
			peerPubKey,
			req.Envelope,
			lcpwire.ErrorCodePaymentInvalid,
			"quote_request mismatch for job",
		)
		return
	}

	// Update stored quote_request (first writer wins semantics are fine here;
	// streams are validated independently).
	existingReq := req
	existing.QuoteRequest = &existingReq
	if existing.State == "" {
		existing.State = jobstore.StateAwaitingInput
	}
	h.jobs.Upsert(existing)

	if existing.QuoteResponse == nil {
		return
	}

	h.logger.Debugw(
		"resend stored lcp_quote_response",
		"peer_pub_key", peerPubKey,
		"job_id", req.Envelope.JobID.String(),
	)

	if !h.sendQuoteResponse(ctx, peerPubKey, *existing.QuoteResponse, remoteMaxPayload) {
		return
	}
	if existing.InputStream == nil || !existing.InputStream.Validated {
		return
	}
	if existing.QuoteRequest == nil {
		return
	}

	invoice := invoiceFromJob(existing)
	h.startJobRunner(
		ctx,
		key,
		*existing.QuoteRequest,
		*existing.InputStream,
		*existing.QuoteResponse,
		invoice,
		remoteMaxPayload,
	)
}

func (h *Handler) handleStreamBegin(ctx context.Context, msg lndpeermsg.InboundCustomMessage) {
	begin, err := lcpwire.DecodeStreamBegin(msg.Payload)
	if err != nil {
		h.logger.Warnw(
			"decode lcp_stream_begin failed",
			"peer_pub_key", msg.PeerPubKey,
			"err", err,
		)
		return
	}

	// Provider only accepts the input stream.
	if begin.Kind != lcpwire.StreamKindInput {
		return
	}

	if h.rejectQuoteRequestIfProviderUnavailable(ctx, msg.PeerPubKey, begin.Envelope) {
		return
	}

	remoteManifest := h.remoteManifestFor(msg.PeerPubKey)
	if remoteManifest == nil {
		h.sendError(
			ctx,
			msg.PeerPubKey,
			begin.Envelope,
			lcpwire.ErrorCodeInvalidState,
			"manifest not exchanged",
		)
		return
	}

	now, ok := h.nowUnix()
	if !ok {
		return
	}
	if h.dropIfExpired(msg.PeerPubKey, begin.Envelope, now, "drop expired lcp_stream_begin") {
		return
	}
	if !h.checkReplay(msg.PeerPubKey, begin.Envelope, now, "lcp_stream_begin") {
		return
	}

	totalLen, sha, vErr := validateInputStreamBegin(begin)
	if vErr != nil {
		h.sendError(ctx, msg.PeerPubKey, begin.Envelope, vErr.Code, vErr.Message)
		return
	}

	if h.jobs == nil {
		return
	}

	key := jobstore.Key{PeerPubKey: msg.PeerPubKey, JobID: begin.Envelope.JobID}
	job, ok := h.jobs.Get(key)
	if !ok || job.QuoteRequest == nil {
		h.sendError(
			ctx,
			msg.PeerPubKey,
			begin.Envelope,
			lcpwire.ErrorCodeInvalidState,
			"no quote_request for job",
		)
		return
	}

	if job.State != jobstore.StateAwaitingInput {
		h.sendError(
			ctx,
			msg.PeerPubKey,
			begin.Envelope,
			lcpwire.ErrorCodeInvalidState,
			"job not awaiting input",
		)
		return
	}

	expectedContentType := inputContentTypeForTaskKind(job.QuoteRequest.TaskKind)
	if expectedContentType != "" && begin.ContentType != expectedContentType {
		h.sendError(
			ctx,
			msg.PeerPubKey,
			begin.Envelope,
			lcpwire.ErrorCodeUnsupportedEncoding,
			"unsupported content_type",
		)
		return
	}

	job.InputStream = &jobstore.InputStreamState{
		StreamID:        begin.StreamID,
		ContentType:     begin.ContentType,
		ContentEncoding: begin.ContentEncoding,
		ExpectedSeq:     0,
		Buf:             nil,
		TotalLen:        totalLen,
		SHA256:          sha,
		Validated:       false,
	}

	h.jobs.Upsert(job)
}

func (h *Handler) handleStreamChunk(ctx context.Context, msg lndpeermsg.InboundCustomMessage) {
	chunk, err := lcpwire.DecodeStreamChunk(msg.Payload)
	if err != nil {
		h.logger.Warnw(
			"decode lcp_stream_chunk failed",
			"peer_pub_key", msg.PeerPubKey,
			"err", err,
		)
		return
	}

	if h.jobs == nil {
		return
	}

	now, ok := h.nowUnix()
	if !ok {
		return
	}
	if h.dropIfExpired(msg.PeerPubKey, chunk.Envelope, now, "drop expired lcp_stream_chunk") {
		return
	}

	key := jobstore.Key{PeerPubKey: msg.PeerPubKey, JobID: chunk.Envelope.JobID}
	job, ok := h.jobs.Get(key)
	if !ok || job.InputStream == nil || job.QuoteRequest == nil {
		return
	}
	if job.State != jobstore.StateAwaitingInput || job.InputStream.Validated {
		return
	}
	if job.InputStream.StreamID != chunk.StreamID {
		return
	}

	if chunk.Seq < job.InputStream.ExpectedSeq {
		// Duplicate chunk.
		return
	}
	if chunk.Seq > job.InputStream.ExpectedSeq {
		h.sendError(
			ctx,
			msg.PeerPubKey,
			chunk.Envelope,
			lcpwire.ErrorCodeChunkOutOfOrder,
			"chunk out of order",
		)
		return
	}

	nextLen := uint64(len(job.InputStream.Buf)) + uint64(len(chunk.Data))
	if nextLen > job.InputStream.TotalLen || nextLen > defaultMaxStreamBytes ||
		nextLen > defaultMaxJobBytes {
		h.sendError(
			ctx,
			msg.PeerPubKey,
			chunk.Envelope,
			lcpwire.ErrorCodePayloadTooLarge,
			"input stream exceeds local limits",
		)
		return
	}

	stream := *job.InputStream
	stream.Buf = append(stream.Buf, chunk.Data...)
	stream.ExpectedSeq++
	job.InputStream = &stream
	h.jobs.Upsert(job)
}

func (h *Handler) handleStreamEnd(ctx context.Context, msg lndpeermsg.InboundCustomMessage) {
	end, err := lcpwire.DecodeStreamEnd(msg.Payload)
	if err != nil {
		h.logger.Warnw(
			"decode lcp_stream_end failed",
			"peer_pub_key", msg.PeerPubKey,
			"err", err,
		)
		return
	}

	if h.jobs == nil {
		return
	}

	now, ok := h.nowUnix()
	if !ok {
		return
	}
	if h.dropIfExpired(msg.PeerPubKey, end.Envelope, now, "drop expired lcp_stream_end") {
		return
	}
	if !h.checkReplay(msg.PeerPubKey, end.Envelope, now, "lcp_stream_end") {
		return
	}

	key := jobstore.Key{PeerPubKey: msg.PeerPubKey, JobID: end.Envelope.JobID}
	job, ok := h.jobs.Get(key)
	if !ok || job.InputStream == nil || job.QuoteRequest == nil {
		h.sendError(
			ctx,
			msg.PeerPubKey,
			end.Envelope,
			lcpwire.ErrorCodeInvalidState,
			"no input stream for job",
		)
		return
	}
	if job.State != jobstore.StateAwaitingInput || job.InputStream.Validated {
		return
	}
	if job.InputStream.StreamID != end.StreamID {
		return
	}

	if end.TotalLen != job.InputStream.TotalLen {
		h.sendError(
			ctx,
			msg.PeerPubKey,
			end.Envelope,
			lcpwire.ErrorCodeChecksumMismatch,
			"stream total_len mismatch",
		)
		return
	}
	if end.SHA256 != job.InputStream.SHA256 {
		h.sendError(
			ctx,
			msg.PeerPubKey,
			end.Envelope,
			lcpwire.ErrorCodeChecksumMismatch,
			"stream sha256 mismatch",
		)
		return
	}
	if uint64(len(job.InputStream.Buf)) != end.TotalLen {
		h.sendError(
			ctx,
			msg.PeerPubKey,
			end.Envelope,
			lcpwire.ErrorCodeChecksumMismatch,
			"stream length mismatch",
		)
		return
	}

	sum := sha256.Sum256(job.InputStream.Buf)
	if lcp.Hash32(sum) != end.SHA256 {
		h.sendError(
			ctx,
			msg.PeerPubKey,
			end.Envelope,
			lcpwire.ErrorCodeChecksumMismatch,
			"stream sha256 mismatch",
		)
		return
	}

	stream := *job.InputStream
	stream.Validated = true
	job.InputStream = &stream

	h.handleValidatedInputStreamEnd(ctx, msg.PeerPubKey, key, job, stream, now, end.Envelope)
}

func (h *Handler) handleValidatedInputStreamEnd(
	ctx context.Context,
	peerPubKey string,
	key jobstore.Key,
	job jobstore.Job,
	stream jobstore.InputStreamState,
	now uint64,
	env lcpwire.JobEnvelope,
) {
	if job.QuoteResponse != nil {
		h.jobs.Upsert(job)
		return
	}

	if vErr := validateInputStream(*job.QuoteRequest, stream); vErr != nil {
		h.sendError(ctx, peerPubKey, env, vErr.Code, vErr.Message)
		return
	}

	quoteTTL := h.quoteTTLSeconds()
	quoteExpiry := now + quoteTTL

	price, err := h.quotePrice(*job.QuoteRequest, stream.Buf)
	if err != nil {
		h.sendError(ctx, peerPubKey, env, quoteErrorCode(err), err.Error())
		return
	}

	termsHash, err := h.computeTermsHash(*job.QuoteRequest, stream, quoteExpiry, price.PriceMsat)
	if err != nil {
		h.sendError(
			ctx,
			peerPubKey,
			env,
			lcpwire.ErrorCodeUnsupportedTask,
			"failed to compute terms_hash",
		)
		return
	}

	remoteManifest := h.remoteManifestFor(peerPubKey)
	remoteMaxPayload := remoteMaxPayloadBytes(remoteManifest)
	if remoteManifest == nil {
		h.sendError(ctx, peerPubKey, env, lcpwire.ErrorCodeInvalidState, "manifest not exchanged")
		return
	}

	h.createInvoiceAndStartQuoteJob(
		ctx,
		peerPubKey,
		key,
		*job.QuoteRequest,
		stream,
		quoteExpiry,
		quoteTTL,
		price.PriceMsat,
		termsHash,
		remoteMaxPayload,
	)
}

func (h *Handler) computeTermsHash(
	req lcpwire.QuoteRequest,
	input jobstore.InputStreamState,
	quoteExpiry uint64,
	priceMsat uint64,
) (lcp.Hash32, error) {
	terms := lcp.Terms{
		ProtocolVersion: req.Envelope.ProtocolVersion,
		JobID:           req.Envelope.JobID,
		PriceMsat:       priceMsat,
		QuoteExpiry:     quoteExpiry,
	}

	var paramsBytes []byte
	if req.ParamsBytes != nil {
		paramsBytes = *req.ParamsBytes
	}

	return protocolcompat.ComputeTermsHash(terms, protocolcompat.TermsCommit{
		TaskKind:             req.TaskKind,
		Input:                input.Buf,
		InputContentType:     input.ContentType,
		InputContentEncoding: input.ContentEncoding,
		Params:               paramsBytes,
	})
}

func (h *Handler) createInvoiceAndStartQuoteJob(
	ctx context.Context,
	peerPubKey string,
	key jobstore.Key,
	req lcpwire.QuoteRequest,
	input jobstore.InputStreamState,
	quoteExpiry uint64,
	quoteTTL uint64,
	priceMsat uint64,
	termsHash lcp.Hash32,
	remoteMaxPayload uint32,
) {
	invoiceExpirySeconds := quoteTTL
	defaultSlackSeconds := envconfig.Uint64(
		"LCP_ALLOWED_CLOCK_SKEW_SECONDS",
		defaultInvoiceExpirySlackSeconds,
	)
	slackSeconds := envconfig.Uint64(
		"LCP_INVOICE_EXPIRY_SLACK_SECONDS",
		defaultSlackSeconds,
	)
	if invoiceExpirySeconds > slackSeconds {
		invoiceExpirySeconds -= slackSeconds
	}
	if invoiceExpirySeconds == 0 {
		invoiceExpirySeconds = 1
	}

	invoice, err := h.invoices.CreateInvoice(ctx, InvoiceRequest{
		DescriptionHash: termsHash,
		PriceMsat:       priceMsat,
		ExpirySeconds:   invoiceExpirySeconds,
	})
	if err != nil {
		h.logger.Warnw(
			"create invoice failed",
			"peer_pub_key", peerPubKey,
			"job_id", req.Envelope.JobID.String(),
			"err", err,
		)
		h.sendError(
			ctx,
			peerPubKey,
			req.Envelope,
			lcpwire.ErrorCodePaymentRequired,
			"failed to create invoice",
		)
		return
	}

	quoteResp := h.newQuoteResponse(
		req.Envelope,
		quoteExpiry,
		priceMsat,
		termsHash,
		invoice.PaymentRequest,
	)

	if !h.sendQuoteResponse(ctx, peerPubKey, quoteResp, remoteMaxPayload) {
		return
	}

	reqCopy := req
	inputCopy := input
	if len(inputCopy.Buf) > 0 {
		inputCopy.Buf = append([]byte(nil), inputCopy.Buf...)
	}

	quoteRespCopy := quoteResp
	termsCopy := quoteResp.TermsHash
	paymentHash := invoice.PaymentHash
	addIndex := invoice.AddIndex

	h.jobs.Upsert(jobstore.Job{
		PeerPubKey:      key.PeerPubKey,
		JobID:           key.JobID,
		State:           jobstore.StateWaitingPayment,
		QuoteRequest:    &reqCopy,
		InputStream:     &inputCopy,
		QuoteExpiry:     quoteExpiry,
		TermsHash:       &termsCopy,
		PaymentHash:     &paymentHash,
		InvoiceAddIndex: &addIndex,
		QuoteResponse:   &quoteRespCopy,
	})

	h.startJobRunner(ctx, key, reqCopy, inputCopy, quoteResp, invoice, remoteMaxPayload)
}

func (h *Handler) rejectQuoteRequestIfProviderUnavailable(
	ctx context.Context,
	peerPubKey string,
	env lcpwire.JobEnvelope,
) bool {
	if !h.cfg.Enabled {
		h.sendError(ctx, peerPubKey, env, lcpwire.ErrorCodeUnsupportedTask, "provider disabled")
		return true
	}

	if _, ok := h.backend.(*computebackend.Disabled); ok {
		h.sendError(
			ctx,
			peerPubKey,
			env,
			lcpwire.ErrorCodeUnsupportedTask,
			"compute backend disabled",
		)
		return true
	}

	return false
}

func (h *Handler) rejectQuoteRequestIfUnsupportedModel(
	ctx context.Context,
	peerPubKey string,
	req lcpwire.QuoteRequest,
) bool {
	switch req.TaskKind {
	case taskKindOpenAIChatCompletionsV1, taskKindOpenAIResponsesV1:
	default:
		return false
	}
	if len(h.cfg.Models) == 0 {
		return false
	}
	if req.ParamsBytes == nil {
		h.sendError(ctx, peerPubKey, req.Envelope, lcpwire.ErrorCodeUnsupportedParams, "params are required")
		return true
	}

	var model string
	switch req.TaskKind {
	case taskKindOpenAIChatCompletionsV1:
		params, err := lcpwire.DecodeOpenAIChatCompletionsV1Params(*req.ParamsBytes)
		if err != nil {
			h.sendError(
				ctx,
				peerPubKey,
				req.Envelope,
				lcpwire.ErrorCodeUnsupportedParams,
				"openai.chat_completions.v1 params must be a valid TLV stream",
			)
			return true
		}
		model = params.Model
	case taskKindOpenAIResponsesV1:
		params, err := lcpwire.DecodeOpenAIResponsesV1Params(*req.ParamsBytes)
		if err != nil {
			h.sendError(
				ctx,
				peerPubKey,
				req.Envelope,
				lcpwire.ErrorCodeUnsupportedParams,
				"openai.responses.v1 params must be a valid TLV stream",
			)
			return true
		}
		model = params.Model
	}

	if _, ok := h.cfg.Models[model]; ok {
		return false
	}

	msg := fmt.Sprintf("unsupported model: %q", model)
	supported := make([]string, 0, len(h.cfg.Models))
	for model := range h.cfg.Models {
		supported = append(supported, model)
	}
	slices.Sort(supported)
	msg = fmt.Sprintf("%s (supported: %s)", msg, strings.Join(supported, ", "))
	h.sendError(ctx, peerPubKey, req.Envelope, lcpwire.ErrorCodeUnsupportedTask, msg)
	return true
}

func (h *Handler) handleCancel(_ context.Context, msg lndpeermsg.InboundCustomMessage) {
	cancelMsg, err := lcpwire.DecodeCancel(msg.Payload)
	if err != nil {
		h.logger.Warnw(
			"decode lcp_cancel failed",
			"peer_pub_key", msg.PeerPubKey,
			"err", err,
		)
		return
	}

	now, ok := h.nowUnix()
	if !ok {
		return
	}
	if h.dropIfExpired(msg.PeerPubKey, cancelMsg.Envelope, now, "drop expired lcp_cancel") {
		return
	}

	key := jobstore.Key{
		PeerPubKey: msg.PeerPubKey,
		JobID:      cancelMsg.Envelope.JobID,
	}

	if !h.checkReplay(msg.PeerPubKey, cancelMsg.Envelope, now, "lcp_cancel") {
		return
	}

	if canceled := h.cancelJob(key); canceled {
		h.logger.Infow(
			"job canceled by requester",
			"peer_pub_key", msg.PeerPubKey,
			"job_id", cancelMsg.Envelope.JobID.String(),
		)
	}
	h.updateState(key, jobstore.StateCanceled)
}

func (h *Handler) dropIfExpired(
	peerPubKey string,
	env lcpwire.JobEnvelope,
	now uint64,
	logMessage string,
) bool {
	if env.Expiry >= now {
		return false
	}

	h.logger.Debugw(
		logMessage,
		"peer_pub_key", peerPubKey,
		"expiry", env.Expiry,
		"now", now,
	)
	return true
}

func (h *Handler) remoteManifestFor(peerPubKey string) *lcpwire.Manifest {
	if h.peers == nil {
		return nil
	}
	manifest, ok := h.peers.RemoteManifestFor(peerPubKey)
	if !ok {
		return nil
	}
	return manifest
}

func (h *Handler) newQuoteResponse(
	reqEnvelope lcpwire.JobEnvelope,
	quoteExpiry uint64,
	priceMsat uint64,
	termsHash lcp.Hash32,
	paymentRequest string,
) lcpwire.QuoteResponse {
	respEnvelope := lcpwire.JobEnvelope{
		ProtocolVersion: reqEnvelope.ProtocolVersion,
		JobID:           reqEnvelope.JobID,
		MsgID:           h.mustNewMsgID(),
		Expiry:          quoteExpiry,
	}

	return lcpwire.QuoteResponse{
		Envelope:       respEnvelope,
		PriceMsat:      priceMsat,
		QuoteExpiry:    quoteExpiry,
		TermsHash:      termsHash,
		PaymentRequest: paymentRequest,
	}
}

func invoiceFromJob(job jobstore.Job) InvoiceResult {
	invoice := InvoiceResult{
		PaymentRequest: job.QuoteResponse.PaymentRequest,
	}
	if job.PaymentHash != nil {
		invoice.PaymentHash = *job.PaymentHash
	}
	if job.InvoiceAddIndex != nil {
		invoice.AddIndex = *job.InvoiceAddIndex
	}
	return invoice
}

func (h *Handler) nowUnix() (uint64, bool) {
	nowUnix := h.clock.Now().Unix()
	if nowUnix < 0 {
		h.logger.Warnw("clock returned negative unix time", "now_unix", nowUnix)
		return 0, false
	}
	return uint64(nowUnix), true
}

func (h *Handler) checkReplay(
	peerPubKey string,
	env lcpwire.JobEnvelope,
	now uint64,
	msgName string,
) bool {
	if h.replay == nil {
		return true
	}

	windowSeconds := envconfig.Uint64(
		"LCP_MAX_ENVELOPE_EXPIRY_WINDOW_SECONDS",
		defaultMaxEnvelopeExpiryWindowSeconds,
	)

	effectiveExpiry := env.Expiry
	if now <= ^uint64(0)-windowSeconds {
		maxExpiry := now + windowSeconds
		if effectiveExpiry > maxExpiry {
			effectiveExpiry = maxExpiry
		}
	}

	result := h.replay.CheckAndRemember(
		replaystore.Key{
			PeerPubKey: peerPubKey,
			JobID:      env.JobID,
			MsgID:      env.MsgID,
		},
		effectiveExpiry,
		now,
	)

	switch result {
	case replaystore.CheckResultOK:
		return true
	case replaystore.CheckResultExpired:
		h.logger.Debugw(
			"drop expired message (replay window)",
			"peer_pub_key", peerPubKey,
			"msg", msgName,
		)
		return false
	case replaystore.CheckResultDuplicate:
		h.logger.Debugw(
			"drop duplicate message",
			"peer_pub_key", peerPubKey,
			"msg", msgName,
		)
		return false
	}
	return false
}

func (h *Handler) sendQuoteResponse(
	ctx context.Context,
	peerPubKey string,
	resp lcpwire.QuoteResponse,
	remoteMaxPayload uint32,
) bool {
	payload, err := lcpwire.EncodeQuoteResponse(resp)
	if err != nil {
		h.logger.Warnw(
			"encode lcp_quote_response failed",
			"peer_pub_key", peerPubKey,
			"job_id", resp.Envelope.JobID.String(),
			"err", err,
		)
		return false
	}

	if remoteMaxPayload != 0 && uint64(len(payload)) > uint64(remoteMaxPayload) {
		h.sendError(
			ctx,
			peerPubKey,
			resp.Envelope,
			lcpwire.ErrorCodePayloadTooLarge,
			"quote_response exceeds remote max_payload_bytes",
		)
		return false
	}

	if sendErr := h.messenger.SendCustomMessage(ctx, peerPubKey, lcpwire.MessageTypeQuoteResponse, payload); sendErr != nil {
		h.logger.Warnw(
			"send lcp_quote_response failed",
			"peer_pub_key", peerPubKey,
			"job_id", resp.Envelope.JobID.String(),
			"err", sendErr,
		)
		return false
	}
	return true
}

func (h *Handler) startJobRunner(
	_ context.Context,
	key jobstore.Key,
	req lcpwire.QuoteRequest,
	input jobstore.InputStreamState,
	quoteResp lcpwire.QuoteResponse,
	invoice InvoiceResult,
	remoteMaxPayload uint32,
) {
	if h.invoices == nil || h.messenger == nil {
		return
	}

	reqCopy := req
	if req.ParamsBytes != nil {
		bytesCopy := append([]byte(nil), (*req.ParamsBytes)...)
		reqCopy.ParamsBytes = &bytesCopy
	}

	inputCopy := input
	if len(input.Buf) > 0 {
		inputCopy.Buf = append([]byte(nil), input.Buf...)
	}

	var (
		jobCtx context.Context
		cancel context.CancelFunc
	)
	nowUnix, ok := h.nowUnix()
	if ok && quoteResp.QuoteExpiry > nowUnix {
		ttlSeconds := quoteResp.QuoteExpiry - nowUnix
		if ttlSeconds <= uint64(maxInt64)/uint64(time.Second) {
			jobCtx, cancel = context.WithTimeout(
				context.Background(),
				time.Duration(ttlSeconds)*time.Second,
			)
		} else {
			jobCtx, cancel = context.WithCancel(context.Background())
		}
	} else {
		jobCtx, cancel = context.WithCancel(context.Background())
	}

	h.jobMu.Lock()
	if _, exists := h.jobCancels[key]; exists {
		h.jobMu.Unlock()
		cancel()
		return
	}
	h.jobCancels[key] = cancel
	h.inFlight++
	h.jobMu.Unlock()

	go h.runJob(jobCtx, key, reqCopy, inputCopy, quoteResp, invoice, remoteMaxPayload)
}

func (h *Handler) runJob(
	ctx context.Context,
	key jobstore.Key,
	req lcpwire.QuoteRequest,
	input jobstore.InputStreamState,
	quoteResp lcpwire.QuoteResponse,
	invoice InvoiceResult,
	remoteMaxPayload uint32,
) {
	defer h.forgetJob(key)

	jobID := quoteResp.Envelope.JobID.String()
	model := modelFromRequest(req)
	inputBytes := len(input.Buf)
	meta := jobLogMeta{
		peerPubKey: key.PeerPubKey,
		jobID:      jobID,
		taskKind:   req.TaskKind,
		model:      model,
		inputBytes: inputBytes,
		priceMsat:  quoteResp.PriceMsat,
		termsHash:  quoteResp.TermsHash.String(),
	}

	settleStart := time.Now()
	if err := h.invoices.WaitForSettlement(ctx, invoice.PaymentHash, invoice.AddIndex); err != nil {
		if errors.Is(err, context.Canceled) {
			h.logger.Debugw(
				"job canceled before settlement",
				"peer_pub_key", key.PeerPubKey,
				"job_id", jobID,
			)
			return
		}
		h.logger.Warnw(
			"wait invoice settlement failed",
			"peer_pub_key", key.PeerPubKey,
			"job_id", jobID,
			"err", err,
		)
		h.updateState(key, jobstore.StateFailed)
		return
	}
	settleDuration := time.Since(settleStart)

	h.updateState(key, jobstore.StatePaid)

	if ctx.Err() != nil {
		return
	}

	h.updateState(key, jobstore.StateExecuting)

	execStart := time.Now()
	planned, _, plannedModel, wantStreaming, err := h.planTask(req, input.Buf)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		code := computeErrorCode(err)
		execDuration := time.Since(execStart)
		h.logJobExecutionFailed(meta, settleDuration, execDuration, code, err)
		h.sendError(ctx, key.PeerPubKey, quoteResp.Envelope, code, err.Error())
		h.updateState(key, jobstore.StateFailed)
		return
	}
	meta.model = plannedModel

	if wantStreaming {
		if streamingBackend, ok := h.backend.(computebackend.StreamingBackend); ok {
			result, err := streamingBackend.ExecuteStreaming(ctx, planned)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				code := computeErrorCode(err)
				execDuration := time.Since(execStart)
				h.logJobExecutionFailed(meta, settleDuration, execDuration, code, err)
				h.sendError(ctx, key.PeerPubKey, quoteResp.Envelope, code, err.Error())
				h.updateState(key, jobstore.StateFailed)
				return
			}
			if result.Output == nil {
				if ctx.Err() != nil {
					return
				}
				err := fmt.Errorf("%w: streaming output is nil", computebackend.ErrBackendUnavailable)
				code := computeErrorCode(err)
				execDuration := time.Since(execStart)
				h.logJobExecutionFailed(meta, settleDuration, execDuration, code, err)
				h.sendError(ctx, key.PeerPubKey, quoteResp.Envelope, code, err.Error())
				h.updateState(key, jobstore.StateFailed)
				return
			}
			defer func() { _ = result.Output.Close() }()

			h.updateState(key, jobstore.StateStreamingResult)

			inputLen := uint64(len(input.Buf))
			outputLen, ok, streamErr := h.sendResultStreaming(
				ctx,
				key.PeerPubKey,
				quoteResp.Envelope,
				req.TaskKind,
				inputLen,
				result.Output,
				remoteMaxPayload,
			)
			execDuration := time.Since(execStart)
			totalDuration := time.Since(settleStart)
			if streamErr != nil {
				if ctx.Err() != nil {
					return
				}
				code := computeErrorCode(streamErr)
				h.logJobExecutionFailed(meta, settleDuration, execDuration, code, streamErr)
				h.sendError(ctx, key.PeerPubKey, quoteResp.Envelope, code, streamErr.Error())
				h.updateState(key, jobstore.StateFailed)
				return
			}
			if !ok {
				h.updateState(key, jobstore.StateFailed)
				return
			}

			h.logJobCompleted(meta, settleDuration, execDuration, totalDuration, int(outputLen), result.Usage)
			h.updateState(key, jobstore.StateDone)
			return
		}
	}

	result, err := h.backend.Execute(ctx, planned)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		code := computeErrorCode(err)
		execDuration := time.Since(execStart)
		h.logJobExecutionFailed(meta, settleDuration, execDuration, code, err)
		h.sendError(ctx, key.PeerPubKey, quoteResp.Envelope, code, err.Error())
		h.updateState(key, jobstore.StateFailed)
		return
	}
	if len(result.OutputBytes) == 0 {
		if ctx.Err() != nil {
			return
		}
		err := fmt.Errorf("%w: execute backend returned empty output", computebackend.ErrBackendUnavailable)
		code := computeErrorCode(err)
		execDuration := time.Since(execStart)
		h.logJobExecutionFailed(meta, settleDuration, execDuration, code, err)
		h.sendError(ctx, key.PeerPubKey, quoteResp.Envelope, code, err.Error())
		h.updateState(key, jobstore.StateFailed)
		return
	}

	execDuration := time.Since(execStart)
	outputBytes := len(result.OutputBytes)
	totalDuration := time.Since(settleStart)
	inputLen := uint64(len(input.Buf))

	if ok := h.sendResult(
		ctx,
		key.PeerPubKey,
		quoteResp.Envelope,
		req.TaskKind,
		wantStreaming,
		inputLen,
		result.OutputBytes,
		remoteMaxPayload,
	); !ok {
		h.updateState(key, jobstore.StateFailed)
		return
	}

	h.logJobCompleted(meta, settleDuration, execDuration, totalDuration, outputBytes, result.Usage)
	h.updateState(key, jobstore.StateDone)
}

func (h *Handler) execute(
	ctx context.Context,
	req lcpwire.QuoteRequest,
	input jobstore.InputStreamState,
) (computebackend.ExecutionResult, error) {
	planned, _, _, _, err := h.planTask(req, input.Buf)
	if err != nil {
		return computebackend.ExecutionResult{}, err
	}

	result, err := h.backend.Execute(ctx, planned)
	if err != nil {
		return computebackend.ExecutionResult{}, err
	}
	if len(result.OutputBytes) == 0 {
		return computebackend.ExecutionResult{}, errors.New("execute backend returned empty output")
	}
	return result, nil
}

func (h *Handler) sendResult(
	ctx context.Context,
	peerPubKey string,
	env lcpwire.JobEnvelope,
	taskKind string,
	streaming bool,
	inputLen uint64,
	output []byte,
	remoteMaxPayload uint32,
) bool {
	if h.messenger == nil {
		return false
	}

	remoteManifest, remoteMaxPayload, ok := h.resolveRemoteManifestAndMaxPayload(
		peerPubKey,
		env.JobID,
		remoteMaxPayload,
	)
	if !ok {
		return false
	}

	totalLen := uint64(len(output))
	if totalLen > remoteManifest.MaxStreamBytes || inputLen+totalLen > remoteManifest.MaxJobBytes {
		msg := "result exceeds remote stream limits"
		return h.sendTerminalResultStatus(
			ctx,
			peerPubKey,
			env,
			lcpwire.ResultStatusFailed,
			&msg,
			remoteMaxPayload,
		)
	}

	streamID, ok := h.newStreamID()
	if !ok {
		return false
	}

	sum := sha256.Sum256(output)
	resultHash := lcp.Hash32(sum)

	expiry := h.resultExpiry()

	contentType := resultContentTypeForTaskKind(taskKind, streaming)
	contentEncoding := contentEncodingIdentity

	if !h.sendResultStream(
		ctx,
		peerPubKey,
		env,
		expiry,
		streamID,
		output,
		totalLen,
		resultHash,
		contentType,
		contentEncoding,
		remoteMaxPayload,
	) {
		return false
	}
	return h.sendTerminalOKResult(
		ctx,
		peerPubKey,
		env,
		expiry,
		streamID,
		totalLen,
		resultHash,
		contentType,
		contentEncoding,
		remoteMaxPayload,
	)
}

func (h *Handler) sendResultStreaming(
	ctx context.Context,
	peerPubKey string,
	env lcpwire.JobEnvelope,
	taskKind string,
	inputLen uint64,
	output io.Reader,
	remoteMaxPayload uint32,
) (uint64, bool, error) {
	if h.messenger == nil {
		return 0, false, nil
	}

	remoteManifest, remoteMaxPayload, ok := h.resolveRemoteManifestAndMaxPayload(
		peerPubKey,
		env.JobID,
		remoteMaxPayload,
	)
	if !ok {
		return 0, false, nil
	}

	maxStreamBytes := remoteManifest.MaxStreamBytes
	if maxStreamBytes == 0 {
		maxStreamBytes = defaultMaxStreamBytes
	}
	maxJobBytes := remoteManifest.MaxJobBytes
	if maxJobBytes == 0 {
		maxJobBytes = defaultMaxJobBytes
	}

	streamID, ok := h.newStreamID()
	if !ok {
		return 0, false, nil
	}

	expiry := h.resultExpiry()

	contentType := resultContentTypeForTaskKind(taskKind, true)
	contentEncoding := contentEncodingIdentity

	if !h.sendResultStreamBeginUnknown(
		ctx,
		peerPubKey,
		env,
		expiry,
		streamID,
		contentType,
		contentEncoding,
		remoteMaxPayload,
	) {
		return 0, false, nil
	}

	hash := sha256.New()
	totalLen := uint64(0)
	seq := uint32(0)

	readBufSize := int(remoteMaxPayload)
	if readBufSize < 1 {
		readBufSize = 1
	}
	readBuf := make([]byte, readBufSize)

	var pending []byte

	for {
		if len(pending) == 0 {
			n, readErr := output.Read(readBuf)
			if n == 0 && readErr == nil {
				return totalLen, false, fmt.Errorf(
					"%w: stream read returned 0 bytes",
					computebackend.ErrBackendUnavailable,
				)
			}
			if n > 0 {
				pending = append(pending[:0], readBuf[:n]...)
			}

			if readErr != nil {
				if errors.Is(readErr, io.EOF) && len(pending) == 0 {
					break
				}
				if !errors.Is(readErr, io.EOF) {
					return totalLen, false, fmt.Errorf(
						"%w: stream read: %s",
						computebackend.ErrBackendUnavailable,
						readErr.Error(),
					)
				}
			}
			if len(pending) == 0 {
				continue
			}
		}

		chunkPayload, chunkLen, err := encodeFittingResultStreamChunk(
			env,
			expiry,
			streamID,
			seq,
			pending,
			remoteMaxPayload,
		)
		if err != nil {
			return totalLen, false, fmt.Errorf(
				"%w: encode lcp_stream_chunk: %s",
				computebackend.ErrBackendUnavailable,
				err.Error(),
			)
		}

		nextTotalLen := totalLen + uint64(chunkLen)
		if nextTotalLen > maxStreamBytes || inputLen+nextTotalLen > maxJobBytes {
			msg := "result exceeds remote stream limits"
			ok := h.sendTerminalResultStatus(
				ctx,
				peerPubKey,
				env,
				lcpwire.ResultStatusFailed,
				&msg,
				remoteMaxPayload,
			)
			return totalLen, ok, nil
		}

		if sendErr := h.messenger.SendCustomMessage(ctx, peerPubKey, lcpwire.MessageTypeStreamChunk, chunkPayload); sendErr != nil {
			h.logger.Warnw(
				"send lcp_stream_chunk failed",
				"peer_pub_key", peerPubKey,
				"seq", seq,
				"err", sendErr,
			)
			return totalLen, false, nil
		}

		if _, err := hash.Write(pending[:chunkLen]); err != nil {
			return totalLen, false, fmt.Errorf(
				"%w: sha256 write: %s",
				computebackend.ErrBackendUnavailable,
				err.Error(),
			)
		}
		totalLen = nextTotalLen
		pending = pending[chunkLen:]
		seq++
	}

	if totalLen == 0 {
		return 0, false, fmt.Errorf("%w: empty response", computebackend.ErrBackendUnavailable)
	}

	var resultHash lcp.Hash32
	sum := hash.Sum(nil)
	copy(resultHash[:], sum)

	if !h.sendResultStreamEnd(
		ctx,
		peerPubKey,
		env,
		expiry,
		streamID,
		totalLen,
		resultHash,
		remoteMaxPayload,
	) {
		return totalLen, false, nil
	}
	ok = h.sendTerminalOKResult(
		ctx,
		peerPubKey,
		env,
		expiry,
		streamID,
		totalLen,
		resultHash,
		contentType,
		contentEncoding,
		remoteMaxPayload,
	)
	return totalLen, ok, nil
}

func (h *Handler) resolveRemoteManifestAndMaxPayload(
	peerPubKey string,
	jobID lcp.JobID,
	remoteMaxPayload uint32,
) (*lcpwire.Manifest, uint32, bool) {
	remoteManifest := h.remoteManifestFor(peerPubKey)
	if remoteManifest == nil {
		h.logger.Warnw(
			"cannot send result: missing remote manifest",
			"peer_pub_key", peerPubKey,
			"job_id", jobID.String(),
		)
		return nil, 0, false
	}
	if remoteMaxPayload == 0 {
		remoteMaxPayload = remoteManifest.MaxPayloadBytes
	}
	if remoteMaxPayload == 0 {
		h.logger.Warnw(
			"cannot send result: remote max_payload_bytes is zero",
			"peer_pub_key", peerPubKey,
			"job_id", jobID.String(),
		)
		return nil, 0, false
	}
	return remoteManifest, remoteMaxPayload, true
}

func (h *Handler) newStreamID() (lcp.Hash32, bool) {
	var streamID lcp.Hash32
	if _, err := rand.Read(streamID[:]); err != nil {
		h.logger.Warnw("generate stream_id failed", "err", err)
		return lcp.Hash32{}, false
	}
	return streamID, true
}

func (h *Handler) sendResultStream(
	ctx context.Context,
	peerPubKey string,
	env lcpwire.JobEnvelope,
	expiry uint64,
	streamID lcp.Hash32,
	output []byte,
	totalLen uint64,
	resultHash lcp.Hash32,
	contentType string,
	contentEncoding string,
	remoteMaxPayload uint32,
) bool {
	if !h.sendResultStreamBegin(
		ctx,
		peerPubKey,
		env,
		expiry,
		streamID,
		totalLen,
		resultHash,
		contentType,
		contentEncoding,
		remoteMaxPayload,
	) {
		return false
	}
	if !h.sendResultStreamChunks(ctx, peerPubKey, env, expiry, streamID, output, remoteMaxPayload) {
		return false
	}
	return h.sendResultStreamEnd(
		ctx,
		peerPubKey,
		env,
		expiry,
		streamID,
		totalLen,
		resultHash,
		remoteMaxPayload,
	)
}

func (h *Handler) sendResultStreamBegin(
	ctx context.Context,
	peerPubKey string,
	env lcpwire.JobEnvelope,
	expiry uint64,
	streamID lcp.Hash32,
	totalLen uint64,
	resultHash lcp.Hash32,
	contentType string,
	contentEncoding string,
	remoteMaxPayload uint32,
) bool {
	begin := lcpwire.StreamBegin{
		Envelope: lcpwire.JobEnvelope{
			ProtocolVersion: env.ProtocolVersion,
			JobID:           env.JobID,
			MsgID:           h.mustNewMsgID(),
			Expiry:          expiry,
		},
		StreamID:        streamID,
		Kind:            lcpwire.StreamKindResult,
		TotalLen:        &totalLen,
		SHA256:          &resultHash,
		ContentType:     contentType,
		ContentEncoding: contentEncoding,
	}
	beginPayload, err := lcpwire.EncodeStreamBegin(begin)
	if err != nil {
		h.logger.Warnw(
			"encode lcp_stream_begin failed",
			"peer_pub_key", peerPubKey,
			"err", err,
		)
		return false
	}
	if uint64(len(beginPayload)) > uint64(remoteMaxPayload) {
		h.logger.Warnw(
			"lcp_stream_begin exceeds remote max_payload_bytes",
			"peer_pub_key", peerPubKey,
			"payload_len", len(beginPayload),
			"max_payload_bytes", remoteMaxPayload,
		)
		return false
	}
	if sendErr := h.messenger.SendCustomMessage(ctx, peerPubKey, lcpwire.MessageTypeStreamBegin, beginPayload); sendErr != nil {
		h.logger.Warnw(
			"send lcp_stream_begin failed",
			"peer_pub_key", peerPubKey,
			"err", sendErr,
		)
		return false
	}
	return true
}

func (h *Handler) sendResultStreamBeginUnknown(
	ctx context.Context,
	peerPubKey string,
	env lcpwire.JobEnvelope,
	expiry uint64,
	streamID lcp.Hash32,
	contentType string,
	contentEncoding string,
	remoteMaxPayload uint32,
) bool {
	begin := lcpwire.StreamBegin{
		Envelope: lcpwire.JobEnvelope{
			ProtocolVersion: env.ProtocolVersion,
			JobID:           env.JobID,
			MsgID:           h.mustNewMsgID(),
			Expiry:          expiry,
		},
		StreamID:        streamID,
		Kind:            lcpwire.StreamKindResult,
		ContentType:     contentType,
		ContentEncoding: contentEncoding,
	}
	beginPayload, err := lcpwire.EncodeStreamBegin(begin)
	if err != nil {
		h.logger.Warnw(
			"encode lcp_stream_begin failed",
			"peer_pub_key", peerPubKey,
			"err", err,
		)
		return false
	}
	if uint64(len(beginPayload)) > uint64(remoteMaxPayload) {
		h.logger.Warnw(
			"lcp_stream_begin exceeds remote max_payload_bytes",
			"peer_pub_key", peerPubKey,
			"payload_len", len(beginPayload),
			"max_payload_bytes", remoteMaxPayload,
		)
		return false
	}
	if sendErr := h.messenger.SendCustomMessage(ctx, peerPubKey, lcpwire.MessageTypeStreamBegin, beginPayload); sendErr != nil {
		h.logger.Warnw(
			"send lcp_stream_begin failed",
			"peer_pub_key", peerPubKey,
			"err", sendErr,
		)
		return false
	}
	return true
}

func (h *Handler) sendResultStreamChunks(
	ctx context.Context,
	peerPubKey string,
	env lcpwire.JobEnvelope,
	expiry uint64,
	streamID lcp.Hash32,
	output []byte,
	remoteMaxPayload uint32,
) bool {
	seq := uint32(0)
	for offset := 0; offset < len(output); {
		chunkPayload, chunkLen, err := encodeFittingResultStreamChunk(
			env,
			expiry,
			streamID,
			seq,
			output[offset:],
			remoteMaxPayload,
		)
		if err != nil {
			h.logger.Warnw(
				"encode lcp_stream_chunk failed",
				"peer_pub_key", peerPubKey,
				"seq", seq,
				"err", err,
			)
			return false
		}

		if sendErr := h.messenger.SendCustomMessage(ctx, peerPubKey, lcpwire.MessageTypeStreamChunk, chunkPayload); sendErr != nil {
			h.logger.Warnw(
				"send lcp_stream_chunk failed",
				"peer_pub_key", peerPubKey,
				"seq", seq,
				"err", sendErr,
			)
			return false
		}

		offset += chunkLen
		seq++
	}

	return true
}

func encodeFittingResultStreamChunk(
	env lcpwire.JobEnvelope,
	expiry uint64,
	streamID lcp.Hash32,
	seq uint32,
	data []byte,
	remoteMaxPayload uint32,
) ([]byte, int, error) {
	chunkLen := len(data)
	if maxPayload := int(remoteMaxPayload); chunkLen > maxPayload {
		chunkLen = maxPayload
	}

	for {
		chunk := lcpwire.StreamChunk{
			Envelope: lcpwire.JobEnvelope{
				ProtocolVersion: env.ProtocolVersion,
				JobID:           env.JobID,
				Expiry:          expiry,
			},
			StreamID: streamID,
			Seq:      seq,
			Data:     data[:chunkLen],
		}
		payload, _, encErr := lcpwire.EncodeStreamChunk(chunk)
		if encErr != nil {
			return nil, 0, encErr
		}

		if uint64(len(payload)) <= uint64(remoteMaxPayload) {
			return payload, chunkLen, nil
		}

		if chunkLen <= 1 {
			return nil, 0, errors.New("cannot fit lcp_stream_chunk under remote max_payload_bytes")
		}
		chunkLen /= 2
	}
}

func (h *Handler) sendResultStreamEnd(
	ctx context.Context,
	peerPubKey string,
	env lcpwire.JobEnvelope,
	expiry uint64,
	streamID lcp.Hash32,
	totalLen uint64,
	resultHash lcp.Hash32,
	remoteMaxPayload uint32,
) bool {
	end := lcpwire.StreamEnd{
		Envelope: lcpwire.JobEnvelope{
			ProtocolVersion: env.ProtocolVersion,
			JobID:           env.JobID,
			MsgID:           h.mustNewMsgID(),
			Expiry:          expiry,
		},
		StreamID: streamID,
		TotalLen: totalLen,
		SHA256:   resultHash,
	}
	endPayload, err := lcpwire.EncodeStreamEnd(end)
	if err != nil {
		h.logger.Warnw(
			"encode lcp_stream_end failed",
			"peer_pub_key", peerPubKey,
			"err", err,
		)
		return false
	}
	if uint64(len(endPayload)) > uint64(remoteMaxPayload) {
		h.logger.Warnw(
			"lcp_stream_end exceeds remote max_payload_bytes",
			"peer_pub_key", peerPubKey,
			"payload_len", len(endPayload),
			"max_payload_bytes", remoteMaxPayload,
		)
		return false
	}
	if sendErr := h.messenger.SendCustomMessage(ctx, peerPubKey, lcpwire.MessageTypeStreamEnd, endPayload); sendErr != nil {
		h.logger.Warnw(
			"send lcp_stream_end failed",
			"peer_pub_key", peerPubKey,
			"err", sendErr,
		)
		return false
	}
	return true
}

func (h *Handler) sendTerminalOKResult(
	ctx context.Context,
	peerPubKey string,
	env lcpwire.JobEnvelope,
	expiry uint64,
	streamID lcp.Hash32,
	totalLen uint64,
	resultHash lcp.Hash32,
	resultContentType string,
	resultContentEncoding string,
	remoteMaxPayload uint32,
) bool {
	terminal := lcpwire.Result{
		Envelope: lcpwire.JobEnvelope{
			ProtocolVersion: env.ProtocolVersion,
			JobID:           env.JobID,
			MsgID:           h.mustNewMsgID(),
			Expiry:          expiry,
		},
		Status: lcpwire.ResultStatusOK,
		OK: &lcpwire.ResultOK{
			ResultStreamID:        streamID,
			ResultHash:            resultHash,
			ResultLen:             totalLen,
			ResultContentType:     resultContentType,
			ResultContentEncoding: resultContentEncoding,
		},
	}
	terminalPayload, err := lcpwire.EncodeResult(terminal)
	if err != nil {
		h.logger.Warnw(
			"encode lcp_result failed",
			"peer_pub_key", peerPubKey,
			"job_id", env.JobID.String(),
			"err", err,
		)
		return false
	}
	if uint64(len(terminalPayload)) > uint64(remoteMaxPayload) {
		h.logger.Warnw(
			"lcp_result exceeds remote max_payload_bytes",
			"peer_pub_key", peerPubKey,
			"payload_len", len(terminalPayload),
			"max_payload_bytes", remoteMaxPayload,
		)
		return false
	}
	if sendErr := h.messenger.SendCustomMessage(ctx, peerPubKey, lcpwire.MessageTypeResult, terminalPayload); sendErr != nil {
		h.logger.Warnw(
			"send lcp_result failed",
			"peer_pub_key", peerPubKey,
			"err", sendErr,
		)
		return false
	}
	return true
}

func (h *Handler) sendTerminalResultStatus(
	ctx context.Context,
	peerPubKey string,
	env lcpwire.JobEnvelope,
	status lcpwire.ResultStatus,
	message *string,
	remoteMaxPayload uint32,
) bool {
	if h.messenger == nil {
		return false
	}

	res := lcpwire.Result{
		Envelope: lcpwire.JobEnvelope{
			ProtocolVersion: env.ProtocolVersion,
			JobID:           env.JobID,
			MsgID:           h.mustNewMsgID(),
			Expiry:          h.resultExpiry(),
		},
		Status:  status,
		Message: message,
	}

	payload, err := lcpwire.EncodeResult(res)
	if err != nil {
		h.logger.Warnw(
			"encode lcp_result failed",
			"peer_pub_key", peerPubKey,
			"err", err,
		)
		return false
	}
	if remoteMaxPayload != 0 && uint64(len(payload)) > uint64(remoteMaxPayload) {
		h.logger.Warnw(
			"lcp_result exceeds remote max_payload_bytes",
			"peer_pub_key", peerPubKey,
			"payload_len", len(payload),
			"max_payload_bytes", remoteMaxPayload,
		)
		return false
	}

	if sendErr := h.messenger.SendCustomMessage(ctx, peerPubKey, lcpwire.MessageTypeResult, payload); sendErr != nil {
		h.logger.Warnw(
			"send lcp_result failed",
			"peer_pub_key", peerPubKey,
			"job_id", env.JobID.String(),
			"err", sendErr,
		)
		return false
	}

	return true
}

func (h *Handler) sendError(
	ctx context.Context,
	peerPubKey string,
	env lcpwire.JobEnvelope,
	code lcpwire.ErrorCode,
	message string,
) {
	msgID := h.mustNewMsgID()
	errMsg := &message

	expiry := env.Expiry
	if expiry == 0 {
		expiry = h.resultExpiry()
	}

	errPayload, err := lcpwire.EncodeError(lcpwire.Error{
		Envelope: lcpwire.JobEnvelope{
			ProtocolVersion: env.ProtocolVersion,
			JobID:           env.JobID,
			MsgID:           msgID,
			Expiry:          expiry,
		},
		Code:    code,
		Message: errMsg,
	})
	if err != nil {
		h.logger.Warnw(
			"encode lcp_error failed",
			"peer_pub_key", peerPubKey,
			"job_id", env.JobID.String(),
			"code", code,
			"err", err,
		)
		return
	}
	if sendErr := h.messenger.SendCustomMessage(ctx, peerPubKey, lcpwire.MessageTypeError, errPayload); sendErr != nil {
		h.logger.Warnw(
			"send lcp_error failed",
			"peer_pub_key", peerPubKey,
			"job_id", env.JobID.String(),
			"code", code,
			"err", sendErr,
		)
	}
}

func (h *Handler) mustNewMsgID() lcpwire.MsgID {
	id, err := h.newMsgIDFn()
	if err != nil {
		h.logger.Warnw("generate msg_id failed", "err", err)
	}
	return id
}

func (h *Handler) quoteTTLSeconds() uint64 {
	if h.cfg.QuoteTTLSeconds == 0 {
		return defaultQuoteTTLSeconds
	}
	return h.cfg.QuoteTTLSeconds
}

func (h *Handler) resultExpiry() uint64 {
	now, ok := h.nowUnix()
	if !ok {
		return h.quoteTTLSeconds()
	}
	return now + h.quoteTTLSeconds()
}

func (h *Handler) cancelJob(key jobstore.Key) bool {
	h.jobMu.Lock()
	cancel, ok := h.jobCancels[key]
	if ok {
		delete(h.jobCancels, key)
		if h.inFlight > 0 {
			h.inFlight--
		}
	}
	h.jobMu.Unlock()

	if ok {
		cancel()
	}
	return ok
}

func (h *Handler) forgetJob(key jobstore.Key) {
	h.jobMu.Lock()
	if _, ok := h.jobCancels[key]; ok {
		delete(h.jobCancels, key)
		if h.inFlight > 0 {
			h.inFlight--
		}
	}
	h.jobMu.Unlock()
}

func (h *Handler) updateState(key jobstore.Key, state jobstore.State) {
	if h.jobs == nil {
		return
	}
	h.jobs.UpdateState(key, state)
}

func newMsgID() (lcpwire.MsgID, error) {
	var id lcpwire.MsgID
	if _, err := rand.Read(id[:]); err != nil {
		return lcpwire.MsgID{}, fmt.Errorf("rand read: %w", err)
	}
	return id, nil
}

func remoteMaxPayloadBytes(manifest *lcpwire.Manifest) uint32 {
	if manifest == nil {
		return 0
	}
	return manifest.MaxPayloadBytes
}

func quoteRequestEqual(a lcpwire.QuoteRequest, b lcpwire.QuoteRequest) bool {
	if a.TaskKind != b.TaskKind {
		return false
	}

	if a.ParamsBytes == nil && b.ParamsBytes == nil {
		return true
	}
	if a.ParamsBytes == nil || b.ParamsBytes == nil {
		return false
	}
	return bytes.Equal(*a.ParamsBytes, *b.ParamsBytes)
}

type openAIChatCompletionsRequest struct {
	Model    string            `json:"model"`
	Messages []json.RawMessage `json:"messages"`

	Stream *bool `json:"stream,omitempty"`

	MaxTokens           *uint32 `json:"max_tokens,omitempty"`
	MaxCompletionTokens *uint32 `json:"max_completion_tokens,omitempty"`
	MaxOutputTokens     *uint32 `json:"max_output_tokens,omitempty"`
}

type openAIResponsesRequest struct {
	Model string          `json:"model"`
	Input json.RawMessage `json:"input"`

	Stream *bool `json:"stream,omitempty"`
}

func parseOpenAIChatCompletionsRequest(body []byte) (openAIChatCompletionsRequest, error) {
	var parsed openAIChatCompletionsRequest
	if err := json.Unmarshal(body, &parsed); err != nil {
		return openAIChatCompletionsRequest{}, err
	}
	return parsed, nil
}

func parseOpenAIResponsesRequest(body []byte) (openAIResponsesRequest, error) {
	var parsed openAIResponsesRequest
	if err := json.Unmarshal(body, &parsed); err != nil {
		return openAIResponsesRequest{}, err
	}
	return parsed, nil
}

func (r openAIChatCompletionsRequest) maxOutputTokens() (uint32, bool) {
	if r.MaxCompletionTokens != nil {
		return *r.MaxCompletionTokens, true
	}
	if r.MaxTokens != nil {
		return *r.MaxTokens, true
	}
	if r.MaxOutputTokens != nil {
		return *r.MaxOutputTokens, true
	}
	return 0, false
}

func validateInputStreamBegin(begin lcpwire.StreamBegin) (uint64, lcp.Hash32, *ValidationError) {
	if begin.ContentEncoding != contentEncodingIdentity {
		return 0, lcp.Hash32{}, &ValidationError{
			Code:    lcpwire.ErrorCodeUnsupportedEncoding,
			Message: "unsupported content_encoding",
		}
	}

	if begin.TotalLen == nil || begin.SHA256 == nil {
		return 0, lcp.Hash32{}, &ValidationError{
			Code:    lcpwire.ErrorCodeInvalidState,
			Message: "input stream begin missing total_len/sha256",
		}
	}

	totalLen := *begin.TotalLen
	if totalLen > defaultMaxStreamBytes || totalLen > defaultMaxJobBytes {
		return 0, lcp.Hash32{}, &ValidationError{
			Code:    lcpwire.ErrorCodePayloadTooLarge,
			Message: "input stream exceeds local limits",
		}
	}

	return totalLen, *begin.SHA256, nil
}

func validateInputStream(
	req lcpwire.QuoteRequest,
	input jobstore.InputStreamState,
) *ValidationError {
	switch req.TaskKind {
	case taskKindOpenAIChatCompletionsV1:
		if req.ParamsBytes == nil {
			return &ValidationError{
				Code:    lcpwire.ErrorCodeUnsupportedParams,
				Message: "openai.chat_completions.v1 params are required",
			}
		}

		params, err := lcpwire.DecodeOpenAIChatCompletionsV1Params(*req.ParamsBytes)
		if err != nil {
			return &ValidationError{
				Code:    lcpwire.ErrorCodeUnsupportedParams,
				Message: "openai.chat_completions.v1 params must be a valid TLV stream",
			}
		}

		parsed, err := parseOpenAIChatCompletionsRequest(input.Buf)
		if err != nil {
			return &ValidationError{
				Code:    lcpwire.ErrorCodeUnsupportedParams,
				Message: "openai.chat_completions.v1 input must be valid json",
			}
		}
		if parsed.Model == "" {
			return &ValidationError{
				Code:    lcpwire.ErrorCodeUnsupportedParams,
				Message: "request_json.model is required",
			}
		}
		if len(parsed.Messages) == 0 {
			return &ValidationError{
				Code:    lcpwire.ErrorCodeUnsupportedParams,
				Message: "request_json.messages is required",
			}
		}
		if parsed.Model != params.Model {
			return &ValidationError{
				Code:    lcpwire.ErrorCodeUnsupportedParams,
				Message: "params.model must match request_json.model",
			}
		}

		return nil
	case taskKindOpenAIResponsesV1:
		if req.ParamsBytes == nil {
			return &ValidationError{
				Code:    lcpwire.ErrorCodeUnsupportedParams,
				Message: "openai.responses.v1 params are required",
			}
		}

		params, err := lcpwire.DecodeOpenAIResponsesV1Params(*req.ParamsBytes)
		if err != nil {
			return &ValidationError{
				Code:    lcpwire.ErrorCodeUnsupportedParams,
				Message: "openai.responses.v1 params must be a valid TLV stream",
			}
		}

		parsed, err := parseOpenAIResponsesRequest(input.Buf)
		if err != nil {
			return &ValidationError{
				Code:    lcpwire.ErrorCodeUnsupportedParams,
				Message: "openai.responses.v1 input must be valid json",
			}
		}
		if parsed.Model == "" {
			return &ValidationError{
				Code:    lcpwire.ErrorCodeUnsupportedParams,
				Message: "request_json.model is required",
			}
		}
		if len(bytes.TrimSpace(parsed.Input)) == 0 || bytes.Equal(bytes.TrimSpace(parsed.Input), []byte("null")) {
			return &ValidationError{
				Code:    lcpwire.ErrorCodeUnsupportedParams,
				Message: "request_json.input is required",
			}
		}
		if parsed.Model != params.Model {
			return &ValidationError{
				Code:    lcpwire.ErrorCodeUnsupportedParams,
				Message: "params.model must match request_json.model",
			}
		}

		return nil
	default:
		return nil
	}
}

func inputContentTypeForTaskKind(taskKind string) string {
	switch taskKind {
	case taskKindOpenAIChatCompletionsV1:
		return contentTypeApplicationJSON
	case taskKindOpenAIResponsesV1:
		return contentTypeApplicationJSON
	default:
		return ""
	}
}

func resultContentTypeForTaskKind(taskKind string, streaming bool) string {
	switch taskKind {
	case taskKindOpenAIChatCompletionsV1:
		if streaming {
			return contentTypeTextEventStream
		}
		return contentTypeApplicationJSON
	case taskKindOpenAIResponsesV1:
		if streaming {
			return contentTypeTextEventStream
		}
		return contentTypeApplicationJSON
	default:
		return contentTypeTextPlain
	}
}

func modelFromRequest(req lcpwire.QuoteRequest) string {
	if req.ParamsBytes == nil {
		return ""
	}

	switch req.TaskKind {
	case taskKindOpenAIChatCompletionsV1:
		params, err := lcpwire.DecodeOpenAIChatCompletionsV1Params(*req.ParamsBytes)
		if err != nil {
			return ""
		}
		return params.Model
	case taskKindOpenAIResponsesV1:
		params, err := lcpwire.DecodeOpenAIResponsesV1Params(*req.ParamsBytes)
		if err != nil {
			return ""
		}
		return params.Model
	default:
		return ""
	}
}

func computeErrorCode(err error) lcpwire.ErrorCode {
	switch {
	case errors.Is(err, computebackend.ErrUnsupportedTaskKind):
		return lcpwire.ErrorCodeUnsupportedTask
	case errors.Is(err, computebackend.ErrInvalidTask):
		return lcpwire.ErrorCodeUnsupportedParams
	case errors.Is(err, computebackend.ErrUnauthenticated):
		return lcpwire.ErrorCodePaymentInvalid
	case errors.Is(err, computebackend.ErrBackendUnavailable):
		return lcpwire.ErrorCodeRateLimited
	default:
		return lcpwire.ErrorCodePaymentInvalid
	}
}

func quoteErrorCode(err error) lcpwire.ErrorCode {
	switch {
	case errors.Is(err, llm.ErrUnknownModel):
		return lcpwire.ErrorCodeUnsupportedTask
	case errors.Is(err, llm.ErrPriceOverflow):
		return lcpwire.ErrorCodeRateLimited
	default:
		return computeErrorCode(err)
	}
}

func errorKind(err error) string {
	switch {
	case errors.Is(err, computebackend.ErrUnsupportedTaskKind):
		return "unsupported_task_kind"
	case errors.Is(err, computebackend.ErrInvalidTask):
		return "invalid_task"
	case errors.Is(err, computebackend.ErrUnauthenticated):
		return "unauthenticated"
	case errors.Is(err, computebackend.ErrBackendUnavailable):
		return "backend_unavailable"
	default:
		return "unknown"
	}
}

func safeErrMessage(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	msg = strings.ReplaceAll(msg, "\n", "\\n")
	msg = strings.ReplaceAll(msg, "\r", "\\r")
	const maxLen = 200
	if len(msg) > maxLen {
		msg = msg[:maxLen] + "...(truncated)"
	}
	return msg
}

type jobLogMeta struct {
	peerPubKey string
	jobID      string
	taskKind   string
	model      string
	inputBytes int
	priceMsat  uint64
	termsHash  string
}

func (h *Handler) logJobExecutionFailed(
	meta jobLogMeta,
	settleDuration time.Duration,
	execDuration time.Duration,
	code lcpwire.ErrorCode,
	err error,
) {
	h.logger.Warnw(
		"job execution failed",
		"peer_pub_key", meta.peerPubKey,
		"job_id", meta.jobID,
		"task_kind", meta.taskKind,
		"model", meta.model,
		"input_bytes", meta.inputBytes,
		"price_msat", meta.priceMsat,
		"terms_hash", meta.termsHash,
		"settle_ms", settleDuration.Milliseconds(),
		"execute_ms", execDuration.Milliseconds(),
		"error_code", code,
		"err_kind", errorKind(err),
	)
	h.logger.Debugw(
		"job execution failed details",
		"peer_pub_key", meta.peerPubKey,
		"job_id", meta.jobID,
		"err", safeErrMessage(err),
	)
}

func (h *Handler) logJobCompleted(
	meta jobLogMeta,
	settleDuration time.Duration,
	execDuration time.Duration,
	totalDuration time.Duration,
	outputBytes int,
	usage computebackend.Usage,
) {
	h.logger.Infow(
		"job completed",
		"peer_pub_key", meta.peerPubKey,
		"job_id", meta.jobID,
		"task_kind", meta.taskKind,
		"model", meta.model,
		"input_bytes", meta.inputBytes,
		"output_bytes", outputBytes,
		"price_msat", meta.priceMsat,
		"terms_hash", meta.termsHash,
		"settle_ms", settleDuration.Milliseconds(),
		"execute_ms", execDuration.Milliseconds(),
		"total_ms", totalDuration.Milliseconds(),
		"usage_input_units", usage.InputUnits,
		"usage_output_units", usage.OutputUnits,
		"usage_total_units", usage.TotalUnits,
	)
}

func (h *Handler) maxOutputTokensForModel(model string) (uint32, error) {
	maxOutputTokens := h.policy.Policy().MaxOutputTokens

	if modelCfg, ok := h.cfg.Models[model]; ok && modelCfg.MaxOutputTokens != nil {
		if *modelCfg.MaxOutputTokens == 0 {
			return 0, errors.New("model max_output_tokens must be > 0")
		}
		maxOutputTokens = *modelCfg.MaxOutputTokens
	}

	if maxOutputTokens == 0 {
		return 0, errors.New("max_output_tokens must be > 0")
	}
	return maxOutputTokens, nil
}

func (h *Handler) planTask(
	req lcpwire.QuoteRequest,
	inputBytes []byte,
) (computebackend.Task, llm.ExecutionPolicy, string, bool, error) {
	switch req.TaskKind {
	case taskKindOpenAIChatCompletionsV1:
		return h.planOpenAIChatCompletionsV1Task(req, inputBytes)
	case taskKindOpenAIResponsesV1:
		return h.planOpenAIResponsesV1Task(req, inputBytes)
	default:
		return computebackend.Task{}, llm.ExecutionPolicy{}, "", false, fmt.Errorf(
			"%w: %q",
			computebackend.ErrUnsupportedTaskKind,
			req.TaskKind,
		)
	}
}

func (h *Handler) planOpenAIChatCompletionsV1Task(
	req lcpwire.QuoteRequest,
	inputBytes []byte,
) (computebackend.Task, llm.ExecutionPolicy, string, bool, error) {
	if req.ParamsBytes == nil {
		return computebackend.Task{}, llm.ExecutionPolicy{}, "", false, fmt.Errorf(
			"%w: openai.chat_completions.v1 params are required",
			computebackend.ErrInvalidTask,
		)
	}
	params, err := lcpwire.DecodeOpenAIChatCompletionsV1Params(*req.ParamsBytes)
	if err != nil {
		return computebackend.Task{}, llm.ExecutionPolicy{}, "", false, fmt.Errorf(
			"%w: invalid openai.chat_completions.v1 params: %s",
			computebackend.ErrInvalidTask,
			err.Error(),
		)
	}

	parsed, err := parseOpenAIChatCompletionsRequest(inputBytes)
	if err != nil {
		return computebackend.Task{}, llm.ExecutionPolicy{}, "", false, fmt.Errorf(
			"%w: invalid openai.chat_completions.v1 input_json: %s",
			computebackend.ErrInvalidTask,
			err.Error(),
		)
	}

	if parsed.Model == "" {
		return computebackend.Task{}, llm.ExecutionPolicy{}, "", false, fmt.Errorf(
			"%w: request_json.model is required",
			computebackend.ErrInvalidTask,
		)
	}
	if len(parsed.Messages) == 0 {
		return computebackend.Task{}, llm.ExecutionPolicy{}, "", false, fmt.Errorf(
			"%w: request_json.messages is required",
			computebackend.ErrInvalidTask,
		)
	}
	if parsed.Model != params.Model {
		return computebackend.Task{}, llm.ExecutionPolicy{}, "", false, fmt.Errorf(
			"%w: params.model must match request_json.model",
			computebackend.ErrInvalidTask,
		)
	}

	providerMaxOutputTokens, err := h.maxOutputTokensForModel(params.Model)
	if err != nil {
		return computebackend.Task{}, llm.ExecutionPolicy{}, "", false, fmt.Errorf(
			"%w: provider max_output_tokens invalid: %s",
			computebackend.ErrInvalidTask,
			err.Error(),
		)
	}

	maxOutputTokens := providerMaxOutputTokens
	if parsedMax, ok := parsed.maxOutputTokens(); ok {
		if parsedMax == 0 {
			return computebackend.Task{}, llm.ExecutionPolicy{}, "", false, fmt.Errorf(
				"%w: max_tokens must be > 0",
				computebackend.ErrInvalidTask,
			)
		}
		if parsedMax > providerMaxOutputTokens {
			return computebackend.Task{}, llm.ExecutionPolicy{}, "", false, fmt.Errorf(
				"%w: max_tokens exceeds provider max_output_tokens",
				computebackend.ErrInvalidTask,
			)
		}
		maxOutputTokens = parsedMax
	}

	streaming := parsed.Stream != nil && *parsed.Stream

	task := computebackend.Task{
		TaskKind:   req.TaskKind,
		Model:      params.Model,
		InputBytes: append([]byte(nil), inputBytes...),
	}
	policy := llm.ExecutionPolicy{MaxOutputTokens: maxOutputTokens}
	return task, policy, params.Model, streaming, nil
}

func (h *Handler) planOpenAIResponsesV1Task(
	req lcpwire.QuoteRequest,
	inputBytes []byte,
) (computebackend.Task, llm.ExecutionPolicy, string, bool, error) {
	if req.ParamsBytes == nil {
		return computebackend.Task{}, llm.ExecutionPolicy{}, "", false, fmt.Errorf(
			"%w: openai.responses.v1 params are required",
			computebackend.ErrInvalidTask,
		)
	}
	params, err := lcpwire.DecodeOpenAIResponsesV1Params(*req.ParamsBytes)
	if err != nil {
		return computebackend.Task{}, llm.ExecutionPolicy{}, "", false, fmt.Errorf(
			"%w: invalid openai.responses.v1 params: %s",
			computebackend.ErrInvalidTask,
			err.Error(),
		)
	}

	parsed, err := parseOpenAIResponsesRequest(inputBytes)
	if err != nil {
		return computebackend.Task{}, llm.ExecutionPolicy{}, "", false, fmt.Errorf(
			"%w: invalid openai.responses.v1 input_json: %s",
			computebackend.ErrInvalidTask,
			err.Error(),
		)
	}

	if parsed.Model == "" {
		return computebackend.Task{}, llm.ExecutionPolicy{}, "", false, fmt.Errorf(
			"%w: request_json.model is required",
			computebackend.ErrInvalidTask,
		)
	}
	if len(bytes.TrimSpace(parsed.Input)) == 0 || bytes.Equal(bytes.TrimSpace(parsed.Input), []byte("null")) {
		return computebackend.Task{}, llm.ExecutionPolicy{}, "", false, fmt.Errorf(
			"%w: request_json.input is required",
			computebackend.ErrInvalidTask,
		)
	}
	if parsed.Model != params.Model {
		return computebackend.Task{}, llm.ExecutionPolicy{}, "", false, fmt.Errorf(
			"%w: params.model must match request_json.model",
			computebackend.ErrInvalidTask,
		)
	}

	providerMaxOutputTokens, err := h.maxOutputTokensForModel(params.Model)
	if err != nil {
		return computebackend.Task{}, llm.ExecutionPolicy{}, "", false, fmt.Errorf(
			"%w: provider max_output_tokens invalid: %s",
			computebackend.ErrInvalidTask,
			err.Error(),
		)
	}

	streaming := parsed.Stream != nil && *parsed.Stream

	task := computebackend.Task{
		TaskKind:   req.TaskKind,
		Model:      params.Model,
		InputBytes: append([]byte(nil), inputBytes...),
	}
	policy := llm.ExecutionPolicy{MaxOutputTokens: providerMaxOutputTokens}
	return task, policy, params.Model, streaming, nil
}

func (h *Handler) priceTable() llm.PriceTable {
	if len(h.cfg.Models) == 0 {
		return llm.DefaultPriceTable()
	}

	table := make(llm.PriceTable, len(h.cfg.Models))
	for model, cfg := range h.cfg.Models {
		entry := cfg.Price
		entry.Model = model
		table[model] = entry
	}
	return table
}

func (h *Handler) quotePrice(
	req lcpwire.QuoteRequest,
	inputBytes []byte,
) (llm.PriceBreakdown, error) {
	planned, policy, model, _, err := h.planTask(req, inputBytes)
	if err != nil {
		return llm.PriceBreakdown{}, err
	}

	estimation, err := h.estimator.Estimate(planned, policy)
	if err != nil {
		return llm.PriceBreakdown{}, err
	}

	base, err := llm.QuotePrice(
		model,
		estimation.Usage,
		0, // cachedInputTokens
		h.priceTable(),
	)
	if err != nil {
		return llm.PriceBreakdown{}, err
	}

	inFlight := h.inFlightJobs()
	multiplierBps := inFlightMultiplierBps(h.cfg.Pricing.InFlightSurge, inFlight)
	priceMsat, err := applyMultiplierCeil(base.PriceMsat, multiplierBps)
	if err != nil {
		return llm.PriceBreakdown{}, err
	}
	if priceMsat != base.PriceMsat && h.logger != nil {
		h.logger.Debugw(
			"applied surge pricing",
			"strategy", "in_flight_jobs",
			"in_flight_jobs", inFlight,
			"multiplier_bps", multiplierBps,
			"base_price_msat", base.PriceMsat,
			"price_msat", priceMsat,
		)
	}

	base.PriceMsat = priceMsat
	return base, nil
}

func (h *Handler) inFlightJobs() uint64 {
	h.jobMu.Lock()
	defer h.jobMu.Unlock()
	return h.inFlight
}
