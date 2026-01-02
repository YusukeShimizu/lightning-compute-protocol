package lcpd

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"

	"buf.build/go/protovalidate"
	lcpdv1 "github.com/bruwbird/lcp/go-lcpd/gen/go/lcpd/v1"
	"github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"
	"github.com/bruwbird/lcp/go-lcpd/internal/envconfig"
	"github.com/bruwbird/lcp/go-lcpd/internal/lcptasks"
	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
	"github.com/bruwbird/lcp/go-lcpd/internal/lightningrpc"
	"github.com/bruwbird/lcp/go-lcpd/internal/peerdirectory"
	"github.com/bruwbird/lcp/go-lcpd/internal/protocolcompat"
	"github.com/bruwbird/lcp/go-lcpd/internal/requesterjobstore"
	"github.com/bruwbird/lcp/go-lcpd/internal/requesterwait"
	"go.uber.org/fx"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

type Params struct {
	fx.In

	Logger        *zap.SugaredLogger       `optional:"true"`
	Clock         Clock                    `optional:"true"`
	PeerDirectory *peerdirectory.Directory `optional:"true"`
	LocalManifest *lcpwire.Manifest        `optional:"true"`

	PeerMessenger PeerMessenger `optional:"true"`
	LightningRPC  LightningRPC  `optional:"true"`

	JobStore *requesterjobstore.Store `optional:"true"`
	Waiter   *requesterwait.Waiter    `optional:"true"`
}

type Service struct {
	lcpdv1.UnimplementedLCPDServiceServer

	logger    *zap.SugaredLogger
	clock     Clock
	peers     *peerdirectory.Directory
	validator protovalidate.Validator

	localManifest lcpwire.Manifest
	messenger     PeerMessenger
	lightning     LightningRPC
	jobs          *requesterjobstore.Store
	waiter        *requesterwait.Waiter
}

func New(p Params) lcpdv1.LCPDServiceServer {
	logger := p.Logger
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	logger = logger.With("component", "grpcservice.lcpd")

	clock := p.Clock
	if clock == nil {
		clock = systemClock{}
	}

	validator, err := protovalidate.New()
	if err != nil {
		logger.Errorw("init protovalidate validator failed", "err", err)
		panic("init protovalidate validator: " + err.Error())
	}

	peers := p.PeerDirectory
	if peers == nil {
		peers = peerdirectory.New()
	}

	localManifest := defaultLocalManifest()
	if p.LocalManifest != nil {
		localManifest = *p.LocalManifest
	}

	jobs := p.JobStore
	if jobs == nil {
		jobs = requesterjobstore.NewWithClock(clock.Now)
	}

	waiter := p.Waiter
	if waiter == nil {
		waiter = requesterwait.New(logger)
	}

	return &Service{
		logger:        logger,
		clock:         clock,
		peers:         peers,
		validator:     validator,
		localManifest: localManifest,
		messenger:     p.PeerMessenger,
		lightning:     p.LightningRPC,
		jobs:          jobs,
		waiter:        waiter,
	}
}

func (s *Service) validate(msg proto.Message) error {
	if err := s.validator.Validate(msg); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	return nil
}

func (s *Service) ListLCPPeers(
	_ context.Context,
	req *lcpdv1.ListLCPPeersRequest,
) (*lcpdv1.ListLCPPeersResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	if err := s.validate(req); err != nil {
		return nil, err
	}

	peers := s.peers.ListLCPPeers()
	out := make([]*lcpdv1.LCPPeer, 0, len(peers))
	for _, p := range peers {
		out = append(out, &lcpdv1.LCPPeer{
			PeerId:         p.PeerID,
			Address:        p.RemoteAddr,
			RemoteManifest: toProtoManifest(p.RemoteManifest),
		})
	}

	s.logger.Debugw("list_lcp_peers",
		"count", len(out),
	)

	return &lcpdv1.ListLCPPeersResponse{Peers: out}, nil
}

func (s *Service) GetLocalInfo(
	ctx context.Context,
	req *lcpdv1.GetLocalInfoRequest,
) (*lcpdv1.GetLocalInfoResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	if err := s.validate(req); err != nil {
		return nil, err
	}

	if s.lightning == nil {
		return nil, status.Error(codes.Unavailable, "lightning rpc not configured")
	}

	info, err := s.lightning.GetInfo(ctx)
	if err != nil {
		return nil, grpcStatusFromLightningError(ctx, err)
	}

	resp := &lcpdv1.GetLocalInfoResponse{
		NodeId:   info.IdentityPubKey,
		Manifest: toProtoManifest(s.localManifest),
	}
	return resp, nil
}

func (s *Service) RequestQuote(
	ctx context.Context,
	req *lcpdv1.RequestQuoteRequest,
) (*lcpdv1.RequestQuoteResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	if err := s.validate(req); err != nil {
		return nil, err
	}

	peerID := strings.TrimSpace(req.GetPeerId())
	task := req.GetTask()
	if err := lcptasks.ValidateTask(task); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	started := time.Now()

	peer, err := s.requireReadyPeer(peerID)
	if err != nil {
		return nil, err
	}
	remoteManifest := peer.RemoteManifest

	summary := summarizeTask(task)

	jobID, payload, err := s.buildQuoteRequestPayload(task)
	if err != nil {
		return nil, err
	}
	payloadBytes := len(payload)

	if remoteManifest.MaxPayloadBytes != nil &&
		len(payload) > int(*remoteManifest.MaxPayloadBytes) {
		return nil, status.Error(
			codes.ResourceExhausted,
			"quote_request payload exceeds peer max_payload_bytes",
		)
	}

	if s.messenger == nil {
		return nil, status.Error(codes.Unavailable, "peer messaging not configured")
	}

	sendErr := s.messenger.SendCustomMessage(ctx, peerID, lcpwire.MessageTypeQuoteRequest, payload)
	if sendErr != nil {
		return nil, grpcStatusFromPeerSendError(ctx, sendErr)
	}

	outcome, err := s.waiter.WaitQuoteResponse(ctx, peerID, jobID)
	if err != nil {
		return nil, s.requestQuoteWaiterError(ctx, peerID, jobID, summary, err)
	}

	if outcome.Error != nil {
		return nil, s.requestQuoteLCPError(peerID, jobID, summary, outcome.Error)
	}
	if outcome.QuoteResponse == nil {
		return nil, status.Error(codes.Internal, "quote waiter returned empty outcome")
	}

	terms, err := termsFromQuoteResponse(task, *outcome.QuoteResponse)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}

	if putErr := s.jobs.PutQuote(peerID, task, terms); putErr != nil {
		s.logger.Errorw(
			"put quote failed",
			"peer_id",
			peerID,
			"job_id",
			jobID.String(),
			"err",
			putErr,
		)
		return nil, status.Error(codes.Internal, "put quote failed")
	}

	s.logQuoteReceived(peerID, jobID, summary, payloadBytes, terms, started)

	return &lcpdv1.RequestQuoteResponse{
		PeerId: peerID,
		Terms:  terms,
	}, nil
}

func (s *Service) buildQuoteRequestPayload(
	task *lcpdv1.Task,
) (lcp.JobID, []byte, error) {
	jobID, err := protocolcompat.NewJobID()
	if err != nil {
		s.logger.Errorw("generate job_id failed", "err", err)
		return lcp.JobID{}, nil, status.Error(codes.Internal, "generate job_id failed")
	}

	msgID, err := newMsgID()
	if err != nil {
		s.logger.Errorw("generate msg_id failed", "err", err)
		return lcp.JobID{}, nil, status.Error(codes.Internal, "generate msg_id failed")
	}

	expiry, err := s.newEnvelopeExpiry()
	if err != nil {
		s.logger.Errorw("generate envelope expiry failed", "err", err)
		return lcp.JobID{}, nil, status.Error(codes.Internal, "generate envelope expiry failed")
	}

	wireTask, err := lcptasks.ToWireQuoteRequestTask(task)
	if err != nil {
		return lcp.JobID{}, nil, status.Error(codes.InvalidArgument, err.Error())
	}

	paramsBytes := wireTask.ParamsBytes
	quoteReq := lcpwire.QuoteRequest{
		Envelope: lcpwire.JobEnvelope{
			ProtocolVersion: lcpwire.ProtocolVersionV01,
			JobID:           jobID,
			MsgID:           msgID,
			Expiry:          expiry,
		},
		TaskKind:      wireTask.TaskKind,
		Input:         wireTask.Input,
		ParamsBytes:   &paramsBytes,
		LLMChatParams: wireTask.LLMChatParams,
	}

	payload, err := lcpwire.EncodeQuoteRequest(quoteReq)
	if err != nil {
		return lcp.JobID{}, nil, status.Error(codes.InvalidArgument, err.Error())
	}

	return jobID, payload, nil
}

func (s *Service) AcceptAndExecute(
	ctx context.Context,
	req *lcpdv1.AcceptAndExecuteRequest,
) (*lcpdv1.AcceptAndExecuteResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	if err := s.validate(req); err != nil {
		return nil, err
	}

	peerID := strings.TrimSpace(req.GetPeerId())

	if !req.GetPayInvoice() {
		return nil, status.Error(codes.InvalidArgument, "pay_invoice must be true")
	}

	jobID, err := toJobID(req.GetJobId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	if s.lightning == nil {
		return nil, status.Error(codes.Unavailable, "lightning rpc not configured")
	}

	started := time.Now()

	terms, err := s.jobs.GetTerms(peerID, jobID)
	if err != nil {
		return nil, grpcStatusFromJobStoreError(err)
	}

	summary := s.summarizeStoredTask(peerID, jobID)

	s.markJobState(peerID, jobID, requesterjobstore.StatePaying)

	verifyErr := verifyInvoiceBinding(ctx, s.lightning, peerID, terms)
	if verifyErr != nil {
		return nil, s.acceptAndExecuteVerifyError(peerID, jobID, summary, verifyErr)
	}

	payStart := time.Now()
	_, payErr := s.lightning.PayInvoice(ctx, terms.GetPaymentRequest())
	if payErr != nil {
		return nil, s.acceptAndExecuteLightningError(ctx, peerID, jobID, summary, payErr)
	}
	payDuration := time.Since(payStart)

	s.markJobState(peerID, jobID, requesterjobstore.StateAwaitingResult)

	waitStart := time.Now()
	outcome, err := s.waiter.WaitResult(ctx, peerID, jobID)
	if err != nil {
		return nil, s.acceptAndExecuteWaiterError(ctx, peerID, jobID, summary, err)
	}
	waitDuration := time.Since(waitStart)
	if outcome.Error != nil {
		return nil, s.acceptAndExecuteLCPError(peerID, jobID, summary, outcome.Error)
	}
	if outcome.Result == nil {
		s.markJobState(peerID, jobID, requesterjobstore.StateFailed)
		return nil, status.Error(codes.Internal, "result waiter returned empty outcome")
	}

	res := &lcpdv1.Result{
		Result: outcome.Result.Result,
	}
	if outcome.Result.ContentType != nil {
		res.ContentType = *outcome.Result.ContentType
	}

	s.markJobState(peerID, jobID, requesterjobstore.StateDone)

	s.logResultReceived(peerID, jobID, summary, terms, payDuration, waitDuration, res, started)

	return &lcpdv1.AcceptAndExecuteResponse{Result: res}, nil
}

func (s *Service) CancelJob(
	ctx context.Context,
	req *lcpdv1.CancelJobRequest,
) (*lcpdv1.CancelJobResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	if err := s.validate(req); err != nil {
		return nil, err
	}

	peerID := strings.TrimSpace(req.GetPeerId())
	jobID, err := toJobID(req.GetJobId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	if s.messenger == nil {
		return nil, status.Error(codes.Unavailable, "peer messaging not configured")
	}

	peer, ok := s.peers.GetPeer(peerID)
	if !ok {
		return nil, status.Error(codes.NotFound, "peer not found")
	}
	if !peer.Connected || !peer.CustomMsgEnabled {
		return nil, status.Error(
			codes.FailedPrecondition,
			"peer is not connected with custom messages enabled",
		)
	}

	msgID, err := newMsgID()
	if err != nil {
		s.logger.Errorw("generate msg_id failed", "err", err)
		return nil, status.Error(codes.Internal, "generate msg_id failed")
	}

	expiry, err := s.newEnvelopeExpiry()
	if err != nil {
		s.logger.Errorw("generate envelope expiry failed", "err", err)
		return nil, status.Error(codes.Internal, "generate envelope expiry failed")
	}

	var reason *string
	if strings.TrimSpace(req.GetReason()) != "" {
		r := strings.TrimSpace(req.GetReason())
		reason = &r
	}

	cancel := lcpwire.Cancel{
		Envelope: lcpwire.JobEnvelope{
			ProtocolVersion: lcpwire.ProtocolVersionV01,
			JobID:           jobID,
			MsgID:           msgID,
			Expiry:          expiry,
		},
		Reason: reason,
	}

	payload, err := lcpwire.EncodeCancel(cancel)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	sendErr := s.messenger.SendCustomMessage(ctx, peerID, lcpwire.MessageTypeCancel, payload)
	if sendErr != nil {
		return nil, grpcStatusFromPeerSendError(ctx, sendErr)
	}

	s.markJobState(peerID, jobID, requesterjobstore.StateCancelled)

	return &lcpdv1.CancelJobResponse{Success: true}, nil
}

func toProtoManifest(m lcpwire.Manifest) *lcpdv1.LCPManifest {
	out := &lcpdv1.LCPManifest{
		ProtocolVersion: uint32(m.ProtocolVersion),
	}
	if m.MaxPayloadBytes != nil {
		out.MaxPayloadBytes = *m.MaxPayloadBytes
	}

	if len(m.SupportedTasks) == 0 {
		return out
	}

	out.SupportedTasks = make([]*lcpdv1.LCPTaskTemplate, 0, len(m.SupportedTasks))
	for _, tmpl := range m.SupportedTasks {
		switch tmpl.TaskKind {
		case taskKindLLMChat:
			if tmpl.LLMChatParams == nil {
				continue
			}

			llmChat := &lcpdv1.LLMChatParams{
				Profile: tmpl.LLMChatParams.Profile,
			}
			if tmpl.LLMChatParams.TemperatureMilli != nil {
				llmChat.TemperatureMilli = *tmpl.LLMChatParams.TemperatureMilli
			}
			if tmpl.LLMChatParams.MaxOutputTokens != nil {
				llmChat.MaxOutputTokens = *tmpl.LLMChatParams.MaxOutputTokens
			}

			out.SupportedTasks = append(out.SupportedTasks, &lcpdv1.LCPTaskTemplate{
				Kind: lcpdv1.LCPTaskKind_LCP_TASK_KIND_LLM_CHAT,
				ParamsTemplate: &lcpdv1.LCPTaskTemplate_LlmChat{
					LlmChat: llmChat,
				},
			})
		default:
			continue
		}
	}

	return out
}

type PeerMessenger interface {
	SendCustomMessage(
		ctx context.Context,
		peerPubKey string,
		msgType lcpwire.MessageType,
		payload []byte,
	) error
}

type LightningRPC interface {
	GetInfo(ctx context.Context) (lightningrpc.Info, error)
	DecodePaymentRequest(
		ctx context.Context,
		paymentRequest string,
	) (lightningrpc.PaymentRequestInfo, error)
	PayInvoice(ctx context.Context, paymentRequest string) (lcp.Hash32, error)
}

type ReadyPeer struct {
	PeerID         string
	RemoteManifest lcpwire.Manifest
}

func (s *Service) requireReadyPeer(peerID string) (ReadyPeer, error) {
	peer, ok := s.peers.GetPeer(peerID)
	if !ok {
		return ReadyPeer{}, status.Error(codes.NotFound, "peer not found")
	}

	if !peer.Connected || !peer.CustomMsgEnabled {
		return ReadyPeer{}, status.Error(
			codes.FailedPrecondition,
			"peer is not connected with custom messages enabled",
		)
	}
	if !peer.LCPReady || peer.RemoteManifest == nil {
		return ReadyPeer{}, status.Error(codes.FailedPrecondition, "peer is not ready for lcp")
	}

	if peer.RemoteManifest.ProtocolVersion != lcpwire.ProtocolVersionV01 {
		return ReadyPeer{}, status.Error(codes.FailedPrecondition, "peer protocol_version mismatch")
	}

	return ReadyPeer{
		PeerID:         peer.PeerID,
		RemoteManifest: *peer.RemoteManifest,
	}, nil
}

func (s *Service) newEnvelopeExpiry() (uint64, error) {
	now := s.clock.Now()
	unix := now.Unix()
	if unix < 0 {
		return 0, fmt.Errorf("clock returned negative unix time: %d", unix)
	}
	sec := uint64(unix)
	if sec > ^uint64(0)-defaultEnvelopeTTLSeconds {
		return 0, fmt.Errorf("envelope expiry overflows uint64: %d", sec)
	}
	return sec + defaultEnvelopeTTLSeconds, nil
}

func (s *Service) markJobState(peerID string, jobID lcp.JobID, state requesterjobstore.State) {
	if s.jobs == nil {
		return
	}
	_ = s.jobs.MarkState(peerID, jobID, state)
}

func termsFromQuoteResponse(task *lcpdv1.Task, resp lcpwire.QuoteResponse) (*lcpdv1.Terms, error) {
	wireTask, err := lcptasks.ToWireQuoteRequestTask(task)
	if err != nil {
		return nil, err
	}

	wantHash, err := protocolcompat.ComputeTermsHash(lcp.Terms{
		ProtocolVersion: resp.Envelope.ProtocolVersion,
		JobID:           resp.Envelope.JobID,
		PriceMsat:       resp.PriceMsat,
		QuoteExpiry:     resp.QuoteExpiry,
	}, protocolcompat.TermsCommit{
		TaskKind: wireTask.TaskKind,
		Input:    wireTask.Input,
		Params:   wireTask.ParamsBytes,
	})
	if err != nil {
		return nil, err
	}
	if wantHash != resp.TermsHash {
		return nil, errors.New("terms_hash mismatch")
	}

	ts, err := timestampFromUnixSeconds(resp.QuoteExpiry)
	if err != nil {
		return nil, err
	}

	termsHashBytes := resp.TermsHash[:]
	jobIDBytes := resp.Envelope.JobID[:]

	return &lcpdv1.Terms{
		ProtocolVersion: uint32(resp.Envelope.ProtocolVersion),
		JobId:           append([]byte(nil), jobIDBytes...),
		PriceMsat:       resp.PriceMsat,
		QuoteExpiry:     ts,
		TermsHash:       append([]byte(nil), termsHashBytes...),
		PaymentRequest:  resp.PaymentRequest,
	}, nil
}

func verifyInvoiceBinding(
	ctx context.Context,
	ln LightningRPC,
	peerID string,
	terms *lcpdv1.Terms,
) error {
	info, err := ln.DecodePaymentRequest(ctx, terms.GetPaymentRequest())
	if err != nil {
		return grpcStatusFromLightningError(ctx, err)
	}

	termsHash, err := toHash32(terms.GetTermsHash())
	if err != nil {
		return status.Error(codes.Internal, "invalid stored terms_hash")
	}

	if info.DescriptionHash != termsHash {
		return status.Error(
			codes.FailedPrecondition,
			"invoice description_hash does not match terms_hash",
		)
	}
	if !strings.EqualFold(info.PayeePubKey, peerID) {
		return status.Error(codes.FailedPrecondition, "invoice destination does not match peer_id")
	}

	priceMsat := terms.GetPriceMsat()
	if priceMsat == 0 {
		return status.Error(codes.FailedPrecondition, "price_msat must be > 0")
	}
	if info.AmountMsat <= 0 {
		return status.Error(codes.FailedPrecondition, "invoice amount is missing or invalid")
	}
	if uint64(info.AmountMsat) != priceMsat {
		return status.Error(codes.FailedPrecondition, "invoice amount does not match price_msat")
	}

	quoteExpiry := terms.GetQuoteExpiry()
	if quoteExpiry == nil {
		return status.Error(codes.Internal, "invalid stored quote_expiry")
	}
	quoteExpiryUnix := quoteExpiry.GetSeconds()
	if quoteExpiryUnix < 0 {
		return status.Error(codes.Internal, "invalid stored quote_expiry")
	}

	allowedClockSkewSeconds := envconfig.Int64(
		"LCP_ALLOWED_CLOCK_SKEW_SECONDS",
		defaultAllowedClockSkewSeconds,
	)
	allowedClockSkewSeconds = max(allowedClockSkewSeconds, 0)

	if info.TimestampUnix > maxInt64-info.ExpirySeconds {
		return status.Error(codes.FailedPrecondition, "invoice expiry overflows")
	}
	invoiceExpiryUnix := info.TimestampUnix + info.ExpirySeconds
	if invoiceExpiryUnix > quoteExpiryUnix+allowedClockSkewSeconds {
		return status.Error(codes.FailedPrecondition, "invoice expiry exceeds quote_expiry")
	}

	return nil
}

func grpcStatusFromWaiterError(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, requesterwait.ErrWaitCancelled):
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return status.Error(
				codes.DeadlineExceeded,
				"deadline exceeded waiting for peer response",
			)
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return status.Error(codes.Canceled, "request cancelled")
		}
		return status.Error(codes.DeadlineExceeded, "wait cancelled")
	case errors.Is(err, requesterwait.ErrAlreadyWaitingQuote),
		errors.Is(err, requesterwait.ErrAlreadyWaitingResult):
		return status.Error(
			codes.FailedPrecondition,
			"another request is already waiting for this job",
		)
	case errors.Is(err, requesterwait.ErrPeerIDRequired):
		return status.Error(codes.InvalidArgument, err.Error())
	default:
		return status.Error(codes.Internal, err.Error())
	}
}

func grpcStatusFromJobStoreError(err error) error {
	switch {
	case errors.Is(err, requesterjobstore.ErrNotFound):
		return status.Error(codes.NotFound, "job not found")
	case errors.Is(err, requesterjobstore.ErrExpired):
		return status.Error(codes.FailedPrecondition, "quote expired")
	default:
		return status.Error(codes.Internal, "job store error")
	}
}

func grpcStatusFromLCPError(errMsg lcpwire.Error) error {
	code := grpcCodeFromLCPErrorCode(errMsg.Code)
	msg := fmt.Sprintf("lcp_error code=%d", errMsg.Code)
	if errMsg.Message != nil && strings.TrimSpace(*errMsg.Message) != "" {
		msg = fmt.Sprintf("%s: %s", msg, strings.TrimSpace(*errMsg.Message))
	}
	return status.Error(code, msg)
}

func grpcCodeFromLCPErrorCode(code lcpwire.ErrorCode) codes.Code {
	switch code {
	case lcpwire.ErrorCodePayloadTooLarge, lcpwire.ErrorCodeRateLimited:
		return codes.ResourceExhausted
	case lcpwire.ErrorCodeUnsupportedVersion,
		lcpwire.ErrorCodeUnsupportedTask,
		lcpwire.ErrorCodeUnsupportedParams,
		lcpwire.ErrorCodeQuoteExpired,
		lcpwire.ErrorCodePaymentRequired,
		lcpwire.ErrorCodePaymentInvalid:
		return codes.FailedPrecondition
	default:
		return codes.Internal
	}
}

func grpcStatusFromLightningError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, lightningrpc.ErrNotConfigured) {
		return status.Error(codes.Unavailable, "lightning rpc not configured")
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return status.Error(codes.DeadlineExceeded, "deadline exceeded contacting lightning node")
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return status.Error(codes.Canceled, "request cancelled")
	}
	if errors.Is(err, lightningrpc.ErrPaymentFailed) {
		return status.Error(codes.FailedPrecondition, err.Error())
	}
	if errors.Is(err, lightningrpc.ErrInvalidRequest) {
		return status.Error(codes.FailedPrecondition, err.Error())
	}
	return status.Error(codes.Unavailable, err.Error())
}

func grpcStatusFromPeerSendError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return status.Error(codes.DeadlineExceeded, "deadline exceeded sending peer message")
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return status.Error(codes.Canceled, "request cancelled")
	}
	return status.Error(codes.Unavailable, err.Error())
}

func toJobID(b []byte) (lcp.JobID, error) {
	if len(b) != lcp.Hash32Len {
		return lcp.JobID{}, fmt.Errorf("job_id must be %d bytes, got %d", lcp.Hash32Len, len(b))
	}
	var id lcp.JobID
	copy(id[:], b)
	return id, nil
}

func toHash32(b []byte) (lcp.Hash32, error) {
	if len(b) != lcp.Hash32Len {
		return lcp.Hash32{}, fmt.Errorf("hash must be %d bytes, got %d", lcp.Hash32Len, len(b))
	}
	var out lcp.Hash32
	copy(out[:], b)
	return out, nil
}

func newMsgID() (lcpwire.MsgID, error) {
	var id lcpwire.MsgID
	if _, err := rand.Read(id[:]); err != nil {
		return lcpwire.MsgID{}, fmt.Errorf("rand read: %w", err)
	}
	return id, nil
}

func timestampFromUnixSeconds(sec uint64) (*timestamppb.Timestamp, error) {
	if sec > uint64(maxInt64) {
		return nil, fmt.Errorf("unix seconds overflows int64: %d", sec)
	}
	return timestamppb.New(time.Unix(int64(sec), 0)), nil
}

const (
	maxInt64                       = int64(^uint64(0) >> 1)
	defaultMaxPayloadBytes         = uint32(16384)
	defaultEnvelopeTTLSeconds      = uint64(300)
	defaultAllowedClockSkewSeconds = int64(5)
)

func defaultLocalManifest() lcpwire.Manifest {
	maxPayloadBytes := defaultMaxPayloadBytes
	return lcpwire.Manifest{
		ProtocolVersion: lcpwire.ProtocolVersionV01,
		MaxPayloadBytes: &maxPayloadBytes,
	}
}
