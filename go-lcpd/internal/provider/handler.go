package provider

import (
	"bytes"
	"context"
	"crypto/rand"
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
	remoteMaxPayload := cloneMaxPayloadBytes(remoteManifest)

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

	quoteTTL := h.quoteTTLSeconds()
	quoteExpiry := now + quoteTTL
	price, err := h.quotePrice(req)
	if err != nil {
		h.sendError(ctx, msg.PeerPubKey, req.Envelope, quoteErrorCode(err), err.Error())
		return
	}

	termsHash, err := h.computeTermsHash(req, quoteExpiry, price.PriceMsat)
	if err != nil {
		h.logger.Warnw(
			"compute terms_hash failed",
			"peer_pub_key", msg.PeerPubKey,
			"job_id", req.Envelope.JobID.String(),
			"err", err,
		)
		h.sendError(
			ctx,
			msg.PeerPubKey,
			req.Envelope,
			lcpwire.ErrorCodeUnsupportedTask,
			"failed to compute terms_hash",
		)
		return
	}

	key := jobstore.Key{PeerPubKey: msg.PeerPubKey, JobID: req.Envelope.JobID}

	if h.handleExistingQuoteRequest(
		ctx,
		msg.PeerPubKey,
		key,
		req,
		now,
		termsHash,
		remoteMaxPayload,
	) {
		return
	}

	h.createInvoiceAndStartQuoteJob(
		ctx,
		msg.PeerPubKey,
		key,
		req,
		quoteExpiry,
		quoteTTL,
		price.PriceMsat,
		termsHash,
		remoteMaxPayload,
	)
}

func (h *Handler) computeTermsHash(
	req lcpwire.QuoteRequest,
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
		TaskKind: req.TaskKind,
		Input:    req.Input,
		Params:   paramsBytes,
	})
}

func (h *Handler) createInvoiceAndStartQuoteJob(
	ctx context.Context,
	peerPubKey string,
	key jobstore.Key,
	req lcpwire.QuoteRequest,
	quoteExpiry uint64,
	quoteTTL uint64,
	priceMsat uint64,
	termsHash lcp.Hash32,
	remoteMaxPayload *uint32,
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

	if !h.sendQuoteResponse(ctx, peerPubKey, quoteResp) {
		return
	}

	h.logger.Infow(
		"quote issued",
		"peer_pub_key", peerPubKey,
		"job_id", req.Envelope.JobID.String(),
		"task_kind", req.TaskKind,
		"profile", modelFromRequest(req),
		"input_bytes", len(req.Input),
		"price_msat", priceMsat,
		"quote_expiry", quoteExpiry,
		"terms_hash", termsHash.String(),
		"invoice_expiry_seconds", invoiceExpirySeconds,
	)

	h.storeQuoteJob(key, quoteExpiry, quoteResp, invoice)
	h.startJobRunner(ctx, key, req, quoteResp, invoice, remoteMaxPayload)
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

func (h *Handler) storeQuoteJob(
	key jobstore.Key,
	quoteExpiry uint64,
	quoteResp lcpwire.QuoteResponse,
	invoice InvoiceResult,
) {
	if h.jobs == nil {
		return
	}

	paymentHash := invoice.PaymentHash
	addIndex := invoice.AddIndex

	h.jobs.Upsert(jobstore.Job{
		PeerPubKey:      key.PeerPubKey,
		JobID:           key.JobID,
		State:           jobstore.StateWaitingPayment,
		QuoteExpiry:     quoteExpiry,
		TermsHash:       &quoteResp.TermsHash,
		PaymentHash:     &paymentHash,
		InvoiceAddIndex: &addIndex,
		QuoteResponse:   &quoteResp,
	})
}

func (h *Handler) handleExistingQuoteRequest(
	ctx context.Context,
	peerPubKey string,
	key jobstore.Key,
	req lcpwire.QuoteRequest,
	now uint64,
	termsHash lcp.Hash32,
	remoteMaxPayload *uint32,
) bool {
	if h.jobs == nil {
		return false
	}

	existing, ok := h.jobs.Get(key)
	if !ok {
		return false
	}

	if existing.QuoteExpiry != 0 && existing.QuoteExpiry < now {
		h.sendError(ctx, peerPubKey, req.Envelope, lcpwire.ErrorCodeQuoteExpired, "quote expired")
		return true
	}

	if existing.TermsHash != nil && !bytes.Equal(existing.TermsHash[:], termsHash[:]) {
		h.sendError(
			ctx,
			peerPubKey,
			req.Envelope,
			lcpwire.ErrorCodePaymentInvalid,
			"terms_hash mismatch for job",
		)
		return true
	}

	if existing.QuoteResponse == nil {
		return false
	}

	h.logger.Debugw(
		"resend stored lcp_quote_response",
		"peer_pub_key", peerPubKey,
		"job_id", req.Envelope.JobID.String(),
	)

	invoice := invoiceFromJob(existing)
	if h.sendQuoteResponse(ctx, peerPubKey, *existing.QuoteResponse) {
		h.startJobRunner(ctx, key, req, *existing.QuoteResponse, invoice, remoteMaxPayload)
	}
	return true
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
	quoteResp lcpwire.QuoteResponse,
	invoice InvoiceResult,
	remoteMaxPayload *uint32,
) {
	if h.invoices == nil || h.messenger == nil {
		return
	}

	reqCopy := req
	reqCopy.Input = append([]byte(nil), req.Input...)
	if req.ParamsBytes != nil {
		bytesCopy := append([]byte(nil), (*req.ParamsBytes)...)
		reqCopy.ParamsBytes = &bytesCopy
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

	go h.runJob(jobCtx, key, reqCopy, quoteResp, invoice, remoteMaxPayload)
}

func (h *Handler) runJob(
	ctx context.Context,
	key jobstore.Key,
	req lcpwire.QuoteRequest,
	quoteResp lcpwire.QuoteResponse,
	invoice InvoiceResult,
	remoteMaxPayload *uint32,
) {
	defer h.forgetJob(key)

	jobID := quoteResp.Envelope.JobID.String()
	profile := modelFromRequest(req)
	backendModel := h.backendModelForProfile(profile)
	inputBytes := len(req.Input)
	meta := jobLogMeta{
		peerPubKey:   key.PeerPubKey,
		jobID:        jobID,
		taskKind:     req.TaskKind,
		profile:      profile,
		backendModel: backendModel,
		inputBytes:   inputBytes,
		priceMsat:    quoteResp.PriceMsat,
		termsHash:    quoteResp.TermsHash.String(),
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
	result, err := h.execute(ctx, req)
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
	execDuration := time.Since(execStart)
	outputBytes := len(result.OutputBytes)
	totalDuration := time.Since(settleStart)

	if ok := h.sendResult(ctx, key.PeerPubKey, quoteResp.Envelope, result.OutputBytes, remoteMaxPayload); !ok {
		h.updateState(key, jobstore.StateFailed)
		return
	}

	h.logJobCompleted(meta, settleDuration, execDuration, totalDuration, outputBytes, result.Usage)

	h.updateState(key, jobstore.StateDone)
}

func (h *Handler) execute(
	ctx context.Context,
	req lcpwire.QuoteRequest,
) (computebackend.ExecutionResult, error) {
	planned, _, _, err := h.planTask(req)
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
	remoteMaxPayload *uint32,
) bool {
	result := lcpwire.Result{
		Envelope: lcpwire.JobEnvelope{
			ProtocolVersion: env.ProtocolVersion,
			JobID:           env.JobID,
			MsgID:           h.mustNewMsgID(),
			Expiry:          h.resultExpiry(),
		},
		Result: output,
	}
	result.ContentType = ptrString(contentTypeTextPlain)

	payload, err := lcpwire.EncodeResult(result)
	if err != nil {
		h.logger.Warnw(
			"encode lcp_result failed",
			"peer_pub_key", peerPubKey,
			"job_id", env.JobID.String(),
			"err", err,
		)
		return false
	}

	if remoteMaxPayload != nil && uint64(len(payload)) > uint64(*remoteMaxPayload) {
		h.sendError(
			ctx,
			peerPubKey,
			env,
			lcpwire.ErrorCodePayloadTooLarge,
			"result payload exceeds remote max_payload_bytes",
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

func ptrString(s string) *string {
	return &s
}

func cloneMaxPayloadBytes(manifest *lcpwire.Manifest) *uint32 {
	if manifest == nil || manifest.MaxPayloadBytes == nil {
		return nil
	}
	val := *manifest.MaxPayloadBytes
	return &val
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
	peerPubKey   string
	jobID        string
	taskKind     string
	profile      string
	backendModel string
	inputBytes   int
	priceMsat    uint64
	termsHash    string
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
		"profile", meta.profile,
		"backend_model", meta.backendModel,
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
		"profile", meta.profile,
		"backend_model", meta.backendModel,
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
		InputBytes: append([]byte(nil), req.Input...),
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

func (h *Handler) quotePrice(req lcpwire.QuoteRequest) (llm.PriceBreakdown, error) {
	planned, policy, profile, err := h.planTask(req)
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
