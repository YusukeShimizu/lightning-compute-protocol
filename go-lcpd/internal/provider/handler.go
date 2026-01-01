package provider

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
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

	contentTypeTextPlain = "text/plain; charset=utf-8"
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

	if h.rejectQuoteRequestIfUnsupportedProfile(ctx, msg.PeerPubKey, req) {
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
		h.sendError(ctx, msg.PeerPubKey, req.Envelope, lcpwire.ErrorCodeInvalidState, "manifest not exchanged")
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
		h.sendError(ctx, peerPubKey, req.Envelope, lcpwire.ErrorCodePaymentInvalid, "quote_request mismatch for job")
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
		h.sendError(ctx, msg.PeerPubKey, begin.Envelope, lcpwire.ErrorCodeInvalidState, "manifest not exchanged")
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

	if begin.ContentEncoding != "identity" {
		h.sendError(
			ctx,
			msg.PeerPubKey,
			begin.Envelope,
			lcpwire.ErrorCodeUnsupportedEncoding,
			"unsupported content_encoding",
		)
		return
	}

	if begin.TotalLen == nil || begin.SHA256 == nil {
		h.sendError(
			ctx,
			msg.PeerPubKey,
			begin.Envelope,
			lcpwire.ErrorCodeInvalidState,
			"input stream begin missing total_len/sha256",
		)
		return
	}

	totalLen := *begin.TotalLen
	if totalLen > defaultMaxStreamBytes || totalLen > defaultMaxJobBytes {
		h.sendError(
			ctx,
			msg.PeerPubKey,
			begin.Envelope,
			lcpwire.ErrorCodePayloadTooLarge,
			"input stream exceeds local limits",
		)
		return
	}

	if h.jobs == nil {
		return
	}

	key := jobstore.Key{PeerPubKey: msg.PeerPubKey, JobID: begin.Envelope.JobID}
	job, ok := h.jobs.Get(key)
	if !ok || job.QuoteRequest == nil {
		h.sendError(ctx, msg.PeerPubKey, begin.Envelope, lcpwire.ErrorCodeInvalidState, "no quote_request for job")
		return
	}

	if job.State != jobstore.StateAwaitingInput {
		h.sendError(ctx, msg.PeerPubKey, begin.Envelope, lcpwire.ErrorCodeInvalidState, "job not awaiting input")
		return
	}

	sha := *begin.SHA256
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
		h.sendError(ctx, msg.PeerPubKey, chunk.Envelope, lcpwire.ErrorCodeChunkOutOfOrder, "chunk out of order")
		return
	}

	nextLen := uint64(len(job.InputStream.Buf)) + uint64(len(chunk.Data))
	if nextLen > job.InputStream.TotalLen || nextLen > defaultMaxStreamBytes || nextLen > defaultMaxJobBytes {
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
		h.sendError(ctx, msg.PeerPubKey, end.Envelope, lcpwire.ErrorCodeInvalidState, "no input stream for job")
		return
	}
	if job.State != jobstore.StateAwaitingInput || job.InputStream.Validated {
		return
	}
	if job.InputStream.StreamID != end.StreamID {
		return
	}

	if end.TotalLen != job.InputStream.TotalLen {
		h.sendError(ctx, msg.PeerPubKey, end.Envelope, lcpwire.ErrorCodeChecksumMismatch, "stream total_len mismatch")
		return
	}
	if end.SHA256 != job.InputStream.SHA256 {
		h.sendError(ctx, msg.PeerPubKey, end.Envelope, lcpwire.ErrorCodeChecksumMismatch, "stream sha256 mismatch")
		return
	}
	if uint64(len(job.InputStream.Buf)) != end.TotalLen {
		h.sendError(ctx, msg.PeerPubKey, end.Envelope, lcpwire.ErrorCodeChecksumMismatch, "stream length mismatch")
		return
	}

	sum := sha256.Sum256(job.InputStream.Buf)
	if lcp.Hash32(sum) != end.SHA256 {
		h.sendError(ctx, msg.PeerPubKey, end.Envelope, lcpwire.ErrorCodeChecksumMismatch, "stream sha256 mismatch")
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

	quoteTTL := h.quoteTTLSeconds()
	quoteExpiry := now + quoteTTL

	price, err := h.quotePrice(*job.QuoteRequest, stream.Buf)
	if err != nil {
		h.sendError(ctx, peerPubKey, env, quoteErrorCode(err), err.Error())
		return
	}

	termsHash, err := h.computeTermsHash(*job.QuoteRequest, stream, quoteExpiry, price.PriceMsat)
	if err != nil {
		h.sendError(ctx, peerPubKey, env, lcpwire.ErrorCodeUnsupportedTask, "failed to compute terms_hash")
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

func (h *Handler) rejectQuoteRequestIfUnsupportedProfile(
	ctx context.Context,
	peerPubKey string,
	req lcpwire.QuoteRequest,
) bool {
	if req.TaskKind != taskKindLLMChat {
		return false
	}
	if len(h.cfg.LLMChatProfiles) == 0 {
		return false
	}
	if req.LLMChatParams == nil {
		h.sendError(
			ctx,
			peerPubKey,
			req.Envelope,
			lcpwire.ErrorCodeUnsupportedParams,
			"llm.chat params are required",
		)
		return true
	}

	if _, ok := h.cfg.LLMChatProfiles[req.LLMChatParams.Profile]; ok {
		return false
	}

	msg := fmt.Sprintf("unsupported profile: %q", req.LLMChatParams.Profile)
	supported := make([]string, 0, len(h.cfg.LLMChatProfiles))
	for profile := range h.cfg.LLMChatProfiles {
		supported = append(supported, profile)
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

	if err := h.invoices.WaitForSettlement(ctx, invoice.PaymentHash, invoice.AddIndex); err != nil {
		if errors.Is(err, context.Canceled) {
			h.logger.Debugw(
				"job canceled before settlement",
				"peer_pub_key", key.PeerPubKey,
				"job_id", quoteResp.Envelope.JobID.String(),
			)
			return
		}
		h.logger.Warnw(
			"wait invoice settlement failed",
			"peer_pub_key", key.PeerPubKey,
			"job_id", quoteResp.Envelope.JobID.String(),
			"err", err,
		)
		h.updateState(key, jobstore.StateFailed)
		return
	}

	h.updateState(key, jobstore.StatePaid)

	if ctx.Err() != nil {
		return
	}

	h.updateState(key, jobstore.StateExecuting)

	result, err := h.execute(ctx, req, input)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		code := computeErrorCode(err)
		h.sendError(ctx, key.PeerPubKey, quoteResp.Envelope, code, err.Error())
		h.updateState(key, jobstore.StateFailed)
		return
	}

	if ok := h.sendResult(ctx, key.PeerPubKey, quoteResp.Envelope, result.OutputBytes, remoteMaxPayload); !ok {
		h.updateState(key, jobstore.StateFailed)
		return
	}

	h.updateState(key, jobstore.StateDone)
}

func (h *Handler) execute(
	ctx context.Context,
	req lcpwire.QuoteRequest,
	input jobstore.InputStreamState,
) (computebackend.ExecutionResult, error) {
	planned, _, _, err := h.planTask(req, input.Buf)
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
	if totalLen > remoteManifest.MaxStreamBytes || totalLen > remoteManifest.MaxJobBytes {
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

	if !h.sendResultStream(ctx, peerPubKey, env, expiry, streamID, output, totalLen, resultHash, remoteMaxPayload) {
		return false
	}
	return h.sendTerminalOKResult(ctx, peerPubKey, env, expiry, streamID, totalLen, resultHash, remoteMaxPayload)
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
	remoteMaxPayload uint32,
) bool {
	if !h.sendResultStreamBegin(ctx, peerPubKey, env, expiry, streamID, totalLen, resultHash, remoteMaxPayload) {
		return false
	}
	if !h.sendResultStreamChunks(ctx, peerPubKey, env, expiry, streamID, output, remoteMaxPayload) {
		return false
	}
	return h.sendResultStreamEnd(ctx, peerPubKey, env, expiry, streamID, totalLen, resultHash, remoteMaxPayload)
}

func (h *Handler) sendResultStreamBegin(
	ctx context.Context,
	peerPubKey string,
	env lcpwire.JobEnvelope,
	expiry uint64,
	streamID lcp.Hash32,
	totalLen uint64,
	resultHash lcp.Hash32,
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
		ContentType:     contentTypeTextPlain,
		ContentEncoding: "identity",
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
			ResultContentType:     contentTypeTextPlain,
			ResultContentEncoding: "identity",
		},
	}
	terminalPayload, err := lcpwire.EncodeResult(terminal)
	if err != nil {
		h.logger.Warnw(
			"encode lcp_result failed",
			"peer_pub_key", peerPubKey,
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
			"code", code,
			"err", err,
		)
		return
	}
	if sendErr := h.messenger.SendCustomMessage(ctx, peerPubKey, lcpwire.MessageTypeError, errPayload); sendErr != nil {
		h.logger.Warnw(
			"send lcp_error failed",
			"peer_pub_key", peerPubKey,
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
	}
	h.jobMu.Unlock()

	if ok {
		cancel()
	}
	return ok
}

func (h *Handler) forgetJob(key jobstore.Key) {
	h.jobMu.Lock()
	delete(h.jobCancels, key)
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

func modelFromRequest(req lcpwire.QuoteRequest) string {
	if req.LLMChatParams != nil {
		return req.LLMChatParams.Profile
	}
	return ""
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

func (h *Handler) backendModelForProfile(profile string) string {
	if profile == "" {
		return ""
	}

	profileCfg, ok := h.cfg.LLMChatProfiles[profile]
	if !ok {
		return profile
	}

	backendModel := strings.TrimSpace(profileCfg.BackendModel)
	if backendModel == "" {
		return profile
	}
	return backendModel
}

func (h *Handler) maxOutputTokensForProfile(profile string) (uint32, error) {
	maxOutputTokens := h.policy.Policy().MaxOutputTokens

	if profileCfg, ok := h.cfg.LLMChatProfiles[profile]; ok && profileCfg.MaxOutputTokens != nil {
		if *profileCfg.MaxOutputTokens == 0 {
			return 0, errors.New("profile max_output_tokens must be > 0")
		}
		maxOutputTokens = *profileCfg.MaxOutputTokens
	}

	if maxOutputTokens == 0 {
		return 0, errors.New("max_output_tokens must be > 0")
	}
	return maxOutputTokens, nil
}

func (h *Handler) planTask(
	req lcpwire.QuoteRequest,
	inputBytes []byte,
) (computebackend.Task, llm.ExecutionPolicy, string, error) {
	profile := modelFromRequest(req)
	if strings.TrimSpace(profile) == "" {
		return computebackend.Task{}, llm.ExecutionPolicy{}, "", fmt.Errorf(
			"%w: llm.chat profile is required",
			computebackend.ErrInvalidTask,
		)
	}

	backendModel := h.backendModelForProfile(profile)
	if strings.TrimSpace(backendModel) == "" {
		return computebackend.Task{}, llm.ExecutionPolicy{}, "", fmt.Errorf(
			"%w: backend model is required",
			computebackend.ErrInvalidTask,
		)
	}

	maxOutputTokens, err := h.maxOutputTokensForProfile(profile)
	if err != nil {
		return computebackend.Task{}, llm.ExecutionPolicy{}, "", fmt.Errorf(
			"%w: invalid max_output_tokens for profile %q: %s",
			computebackend.ErrInvalidTask,
			profile,
			err.Error(),
		)
	}

	policy, err := llm.NewFixedExecutionPolicy(maxOutputTokens)
	if err != nil {
		return computebackend.Task{}, llm.ExecutionPolicy{}, "", fmt.Errorf(
			"%w: create execution policy failed: %s",
			computebackend.ErrInvalidTask,
			err.Error(),
		)
	}

	task := computebackend.Task{
		TaskKind:   req.TaskKind,
		Model:      backendModel,
		InputBytes: append([]byte(nil), inputBytes...),
	}

	planned, err := policy.Apply(task)
	if err != nil {
		return computebackend.Task{}, llm.ExecutionPolicy{}, "", err
	}

	if profileCfg, ok := h.cfg.LLMChatProfiles[profile]; ok {
		paramsBytes, marshalErr := json.Marshal(struct {
			MaxOutputTokens  uint32   `json:"max_output_tokens"`
			Temperature      *float64 `json:"temperature,omitempty"`
			TopP             *float64 `json:"top_p,omitempty"`
			Stop             []string `json:"stop,omitempty"`
			PresencePenalty  *float64 `json:"presence_penalty,omitempty"`
			FrequencyPenalty *float64 `json:"frequency_penalty,omitempty"`
			Seed             *int64   `json:"seed,omitempty"`
		}{
			MaxOutputTokens:  maxOutputTokens,
			Temperature:      profileCfg.OpenAI.Temperature,
			TopP:             profileCfg.OpenAI.TopP,
			Stop:             profileCfg.OpenAI.Stop,
			PresencePenalty:  profileCfg.OpenAI.PresencePenalty,
			FrequencyPenalty: profileCfg.OpenAI.FrequencyPenalty,
			Seed:             profileCfg.OpenAI.Seed,
		})
		if marshalErr != nil {
			return computebackend.Task{}, llm.ExecutionPolicy{}, "", fmt.Errorf(
				"%w: marshal backend params: %s",
				computebackend.ErrInvalidTask,
				marshalErr.Error(),
			)
		}
		planned.ParamsBytes = paramsBytes
	}

	return planned, policy.Policy(), profile, nil
}

func (h *Handler) priceTable() llm.PriceTable {
	if len(h.cfg.LLMChatProfiles) == 0 {
		return llm.DefaultPriceTable()
	}

	table := make(llm.PriceTable, len(h.cfg.LLMChatProfiles))
	for profile, cfg := range h.cfg.LLMChatProfiles {
		entry := cfg.Price
		entry.Model = profile
		table[profile] = entry
	}
	return table
}

func (h *Handler) quotePrice(req lcpwire.QuoteRequest, inputBytes []byte) (llm.PriceBreakdown, error) {
	planned, policy, profile, err := h.planTask(req, inputBytes)
	if err != nil {
		return llm.PriceBreakdown{}, err
	}

	estimation, err := h.estimator.Estimate(planned, policy)
	if err != nil {
		return llm.PriceBreakdown{}, err
	}

	return llm.QuotePrice(
		profile,
		estimation.Usage,
		0, // cachedInputTokens
		h.priceTable(),
	)
}
