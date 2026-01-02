//nolint:testpackage // White-box tests need access to unexported fields.
package provider

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bruwbird/lcp/go-lcpd/internal/computebackend"
	"github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"
	"github.com/bruwbird/lcp/go-lcpd/internal/jobstore"
	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
	"github.com/bruwbird/lcp/go-lcpd/internal/llm"
	"github.com/bruwbird/lcp/go-lcpd/internal/lndpeermsg"
	"github.com/bruwbird/lcp/go-lcpd/internal/peerdirectory"
	"github.com/bruwbird/lcp/go-lcpd/internal/protocolcompat"
	"github.com/bruwbird/lcp/go-lcpd/internal/replaystore"
	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestHandler_HandleQuoteRequest_SendsQuoteResponse(t *testing.T) {
	t.Parallel()

	clock := &fakeClock{now: time.Unix(1000, 0)}
	messenger := &fakeMessenger{}
	invoices := newFakeInvoiceCreator()
	invoices.result = InvoiceResult{
		PaymentRequest: "lnbcrt1payment",
		PaymentHash:    mustHash32(0xAA),
		AddIndex:       7,
	}
	policy := llm.MustFixedExecutionPolicy(llm.DefaultMaxOutputTokens)
	estimator := llm.NewApproxUsageEstimator()
	jobs := jobstore.New()
	peers := peerdirectory.New()
	peers.UpsertPeer("peer1", "addr")
	peers.MarkConnected("peer1")
	peers.MarkCustomMsgEnabled("peer1", true)

	handler := NewHandler(
		Config{Enabled: true, QuoteTTLSeconds: 600},
		DefaultValidator(),
		messenger,
		invoices,
		&fakeBackend{},
		policy,
		estimator,
		jobs,
		replaystore.New(0),
		peers,
		nil,
	)
	handler.clock = clock
	handler.newMsgIDFn = func() (lcpwire.MsgID, error) { return fixedMsgID(0x03), nil }

	model := "gpt-5.2"
	req := newLLMChatQuoteRequest(model)
	payload, err := lcpwire.EncodeQuoteRequest(req)
	if err != nil {
		t.Fatalf("encode quote_request: %v", err)
	}

	if vErr := DefaultValidator().ValidateQuoteRequest(req, nil); vErr != nil {
		t.Fatalf("quote_request validation failed: %v", vErr)
	}
	if handler.invoices == nil || handler.messenger == nil {
		t.Fatalf("handler dependencies not initialized")
	}

	handler.handleQuoteRequest(context.Background(), lndpeermsg.InboundCustomMessage{
		PeerPubKey: "peer1",
		MsgType:    uint16(lcpwire.MessageTypeQuoteRequest),
		Payload:    payload,
	})
	key := jobstore.Key{PeerPubKey: "peer1", JobID: req.Envelope.JobID}
	t.Cleanup(func() { handler.cancelJob(key) })
	t.Cleanup(func() { handler.cancelJob(key) })

	messages := messenger.messages()
	if len(messages) != 1 {
		t.Fatalf("expected 1 message sent, got %d", len(messages))
	}
	sent := messages[0]
	if sent.msgType != lcpwire.MessageTypeQuoteResponse {
		t.Fatalf("expected quote_response message, got %d", sent.msgType)
	}

	gotResp, err := lcpwire.DecodeQuoteResponse(sent.payload)
	if err != nil {
		t.Fatalf("decode quote_response: %v", err)
	}
	price := mustQuotePriceForPrompt(t, policy, estimator, model, req.Input)
	if diff := cmp.Diff(price.PriceMsat, gotResp.PriceMsat); diff != "" {
		t.Fatalf("price_msat mismatch (-want +got):\n%s", diff)
	}
	wantExpiry := uint64(clock.now.Unix()) + 600
	if diff := cmp.Diff(wantExpiry, gotResp.QuoteExpiry); diff != "" {
		t.Fatalf("quote_expiry mismatch (-want +got):\n%s", diff)
	}
	if gotResp.PaymentRequest != "lnbcrt1payment" {
		t.Fatalf("payment_request mismatch: got %q", gotResp.PaymentRequest)
	}

	job, ok := jobs.Get(key)
	if !ok {
		t.Fatalf("job not stored")
	}
	if diff := cmp.Diff(jobstore.StateWaitingPayment, job.State); diff != "" {
		t.Fatalf("job state mismatch (-want +got):\n%s", diff)
	}
	if job.PaymentHash == nil || *job.PaymentHash != invoices.result.PaymentHash {
		t.Fatalf("payment_hash mismatch")
	}
	if job.InvoiceAddIndex == nil || *job.InvoiceAddIndex != invoices.result.AddIndex {
		t.Fatalf("invoice add_index mismatch: got nil or wrong value")
	}
}

func TestHandler_CheckReplay_ClampsEnvelopeExpiryWindow(t *testing.T) {
	t.Setenv("LCP_MAX_ENVELOPE_EXPIRY_WINDOW_SECONDS", "10")

	handler := &Handler{replay: replaystore.New(0)}

	var jobID lcp.JobID
	for i := range jobID {
		jobID[i] = 0x01
	}
	var msgID lcpwire.MsgID
	for i := range msgID {
		msgID[i] = 0x02
	}

	now := uint64(1000)
	env := lcpwire.JobEnvelope{
		ProtocolVersion: lcpwire.ProtocolVersionV01,
		JobID:           jobID,
		MsgID:           msgID,
		Expiry:          now + 1_000_000,
	}

	if ok := handler.checkReplay("peer1", env, now, "lcp_quote_request"); !ok {
		t.Fatalf("expected first checkReplay to accept message")
	}

	// If the handler stores far-future expiry without clamping, this second call would
	// be treated as a duplicate. With clamping, the stored entry expires at
	// now+windowSeconds, so this call should be accepted.
	if ok := handler.checkReplay("peer1", env, now+11, "lcp_quote_request"); !ok {
		t.Fatalf("expected second checkReplay to accept message after clamp window")
	}
}

func TestHandler_HandleQuoteRequest_RejectsUnsupportedProfile(t *testing.T) {
	t.Parallel()

	clock := &fakeClock{now: time.Unix(1000, 0)}
	messenger := &fakeMessenger{}
	invoices := newFakeInvoiceCreator()
	invoices.result = InvoiceResult{
		PaymentRequest: "lnbcrt1payment",
		PaymentHash:    mustHash32(0xAA),
		AddIndex:       7,
	}
	policy := llm.MustFixedExecutionPolicy(llm.DefaultMaxOutputTokens)
	estimator := llm.NewApproxUsageEstimator()
	jobs := jobstore.New()
	peers := peerdirectory.New()
	peers.UpsertPeer("peer1", "addr")
	peers.MarkConnected("peer1")
	peers.MarkCustomMsgEnabled("peer1", true)

	handler := NewHandler(
		Config{
			Enabled:         true,
			QuoteTTLSeconds: 600,
			LLMChatProfiles: map[string]LLMChatProfile{
				"gpt-5.2": {
					BackendModel: "gpt-5.2",
					Price:        llm.DefaultPriceTable()["gpt-5.2"],
				},
			},
		},
		DefaultValidator(),
		messenger,
		invoices,
		&fakeBackend{},
		policy,
		estimator,
		jobs,
		replaystore.New(0),
		peers,
		nil,
	)
	handler.clock = clock
	handler.newMsgIDFn = func() (lcpwire.MsgID, error) { return fixedMsgID(0x03), nil }

	req := newLLMChatQuoteRequest("x")
	payload, err := lcpwire.EncodeQuoteRequest(req)
	if err != nil {
		t.Fatalf("encode quote_request: %v", err)
	}

	handler.handleQuoteRequest(context.Background(), lndpeermsg.InboundCustomMessage{
		PeerPubKey: "peer1",
		MsgType:    uint16(lcpwire.MessageTypeQuoteRequest),
		Payload:    payload,
	})

	if got := invoices.createCalls(); got != 0 {
		t.Fatalf("expected no invoice to be created, got %d", got)
	}

	messages := messenger.messages()
	if len(messages) != 1 {
		t.Fatalf("expected 1 message sent, got %d", len(messages))
	}
	if got, want := messages[0].msgType, lcpwire.MessageTypeError; got != want {
		t.Fatalf("expected lcp_error, got %d", got)
	}

	gotErr, err := lcpwire.DecodeError(messages[0].payload)
	if err != nil {
		t.Fatalf("decode lcp_error: %v", err)
	}
	if diff := cmp.Diff(lcpwire.ErrorCodeUnsupportedTask, gotErr.Code); diff != "" {
		t.Fatalf("error code mismatch (-want +got):\n%s", diff)
	}
	if gotErr.Message == nil {
		t.Fatalf("error message is nil")
	}
	if !strings.Contains(*gotErr.Message, "unsupported profile") {
		t.Fatalf("expected error message to mention unsupported profile, got %q", *gotErr.Message)
	}
	if !strings.Contains(*gotErr.Message, "x") {
		t.Fatalf("expected error message to include requested profile, got %q", *gotErr.Message)
	}
	if !strings.Contains(*gotErr.Message, "gpt-5.2") {
		t.Fatalf("expected error message to include supported profile, got %q", *gotErr.Message)
	}
}

func TestHandler_HandleQuoteRequest_ReusesExistingQuoteResponse(t *testing.T) {
	t.Parallel()

	clock := &fakeClock{now: time.Unix(1000, 0)}
	messenger := &fakeMessenger{}
	invoices := newFakeInvoiceCreator()
	invoices.result = InvoiceResult{
		PaymentRequest: "lnbcrt1existing",
		PaymentHash:    mustHash32(0xBB),
		AddIndex:       4,
	}
	jobs := jobstore.New()

	policy := llm.MustFixedExecutionPolicy(llm.DefaultMaxOutputTokens)
	estimator := llm.NewApproxUsageEstimator()
	model := "gpt-5.2"
	req := newLLMChatQuoteRequest(model)
	price := mustQuotePriceForPrompt(t, policy, estimator, model, req.Input).PriceMsat
	quoteTTL := uint64(600)
	quoteExpiry := uint64(clock.now.Unix()) + quoteTTL
	paramsBytes := []byte(nil)
	if req.ParamsBytes != nil {
		paramsBytes = *req.ParamsBytes
	}
	termsHash, err := protocolcompat.ComputeTermsHash(lcp.Terms{
		ProtocolVersion: req.Envelope.ProtocolVersion,
		JobID:           req.Envelope.JobID,
		PriceMsat:       price,
		QuoteExpiry:     quoteExpiry,
	}, protocolcompat.TermsCommit{
		TaskKind: req.TaskKind,
		Input:    req.Input,
		Params:   paramsBytes,
	})
	if err != nil {
		t.Fatalf("ComputeTermsHash: %v", err)
	}

	quoteResp := lcpwire.QuoteResponse{
		Envelope: lcpwire.JobEnvelope{
			ProtocolVersion: req.Envelope.ProtocolVersion,
			JobID:           req.Envelope.JobID,
			MsgID:           fixedMsgID(0x10),
			Expiry:          quoteExpiry,
		},
		PriceMsat:      price,
		QuoteExpiry:    quoteExpiry,
		TermsHash:      termsHash,
		PaymentRequest: invoices.result.PaymentRequest,
	}
	paymentHash := invoices.result.PaymentHash
	addIndex := invoices.result.AddIndex
	jobs.Upsert(jobstore.Job{
		PeerPubKey:      "peer1",
		JobID:           req.Envelope.JobID,
		State:           jobstore.StateWaitingPayment,
		QuoteExpiry:     quoteExpiry,
		TermsHash:       &quoteResp.TermsHash,
		PaymentHash:     &paymentHash,
		InvoiceAddIndex: &addIndex,
		QuoteResponse:   &quoteResp,
	})

	handler := NewHandler(
		Config{Enabled: true, QuoteTTLSeconds: quoteTTL},
		DefaultValidator(),
		messenger,
		invoices,
		&fakeBackend{},
		policy,
		estimator,
		jobs,
		replaystore.New(0),
		nil,
		nil,
	)
	handler.clock = clock

	payload, err := lcpwire.EncodeQuoteRequest(req)
	if err != nil {
		t.Fatalf("encode quote_request: %v", err)
	}

	handler.HandleInboundCustomMessage(context.Background(), lndpeermsg.InboundCustomMessage{
		PeerPubKey: "peer1",
		MsgType:    uint16(lcpwire.MessageTypeQuoteRequest),
		Payload:    payload,
	})
	key := jobstore.Key{PeerPubKey: "peer1", JobID: req.Envelope.JobID}
	t.Cleanup(func() { handler.cancelJob(key) })

	if invoices.createCalls() != 0 {
		t.Fatalf("expected invoice not to be recreated, got %d calls", invoices.createCalls())
	}
	messages := messenger.messages()
	if len(messages) != 1 {
		t.Fatalf("expected 1 message sent, got %d", len(messages))
	}
	gotResp, err := lcpwire.DecodeQuoteResponse(messages[0].payload)
	if err != nil {
		t.Fatalf("decode quote_response: %v", err)
	}
	if diff := cmp.Diff(quoteResp, gotResp); diff != "" {
		t.Fatalf("quote_response mismatch (-want +got):\n%s", diff)
	}
}

func TestHandler_RunJob_SettledSendsResult(t *testing.T) {
	t.Parallel()

	clock := &fakeClock{now: time.Unix(2000, 0)}
	messenger := &fakeMessenger{}
	invoices := newFakeInvoiceCreator()
	invoices.result = InvoiceResult{
		PaymentRequest: "lnbcrt1settle",
		PaymentHash:    mustHash32(0xCC),
		AddIndex:       12,
	}
	jobs := jobstore.New()
	backend := &fakeBackend{
		result: computebackend.ExecutionResult{
			OutputBytes: []byte("hello world"),
		},
	}
	policy := llm.MustFixedExecutionPolicy(llm.DefaultMaxOutputTokens)
	estimator := llm.NewApproxUsageEstimator()

	handler := NewHandler(
		Config{Enabled: true, QuoteTTLSeconds: 300},
		DefaultValidator(),
		messenger,
		invoices,
		backend,
		policy,
		estimator,
		jobs,
		replaystore.New(0),
		nil,
		nil,
	)
	handler.clock = clock
	handler.replay = nil

	req := newLLMChatQuoteRequest("gpt-5.2", func(r *lcpwire.QuoteRequest) {
		r.Envelope.Expiry = uint64(clock.now.Add(30 * time.Second).Unix())
	})
	payload, err := lcpwire.EncodeQuoteRequest(req)
	if err != nil {
		t.Fatalf("encode quote_request: %v", err)
	}

	handler.HandleInboundCustomMessage(context.Background(), lndpeermsg.InboundCustomMessage{
		PeerPubKey: "peer1",
		MsgType:    uint16(lcpwire.MessageTypeQuoteRequest),
		Payload:    payload,
	})
	key := jobstore.Key{PeerPubKey: "peer1", JobID: req.Envelope.JobID}

	if invoices.createCalls() == 0 {
		t.Fatalf("expected invoice to be created")
	}

	// Release settlement to allow runJob to proceed.
	invoices.Settle()

	deadline := time.After(3 * time.Second)
	for len(messenger.messages()) < 2 {
		select {
		case <-deadline:
			job, _ := jobs.Get(key)
			t.Fatalf(
				"expected quote_response + result, got %d messages (job_state=%v)",
				len(messenger.messages()),
				job.State,
			)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	messages := messenger.messages()
	resultMsg := messages[1]
	if resultMsg.msgType != lcpwire.MessageTypeResult {
		t.Fatalf("expected lcp_result, got %d", resultMsg.msgType)
	}

	gotResult, err := lcpwire.DecodeResult(resultMsg.payload)
	if err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if diff := cmp.Diff([]byte("hello world"), gotResult.Result); diff != "" {
		t.Fatalf("result payload mismatch (-want +got):\n%s", diff)
	}
	if gotResult.ContentType == nil || *gotResult.ContentType != contentTypeTextPlain {
		t.Fatalf("content_type mismatch: %v", gotResult.ContentType)
	}

	waitFor(t, time.Second, func() bool {
		job, ok := jobs.Get(key)
		return ok && job.State == jobstore.StateDone
	})
}

func TestHandler_LogsDoNotContainPromptOrResult(t *testing.T) {
	t.Parallel()

	const (
		promptText      = "SENSITIVE_PROMPT_DO_NOT_LOG"
		outputText      = "SENSITIVE_OUTPUT_DO_NOT_LOG"
		paymentRequest  = "lnbcrt1settle"
		contentTypeText = contentTypeTextPlain
	)

	core, observed := observer.New(zapcore.DebugLevel)
	logger := zap.New(core).Sugar()

	clock := &fakeClock{now: time.Unix(2000, 0)}
	messenger := &fakeMessenger{}
	invoices := newFakeInvoiceCreator()
	invoices.result = InvoiceResult{
		PaymentRequest: paymentRequest,
		PaymentHash:    mustHash32(0xCC),
		AddIndex:       12,
	}
	jobs := jobstore.New()
	backend := &fakeBackend{
		result: computebackend.ExecutionResult{
			OutputBytes: []byte(outputText),
		},
	}
	policy := llm.MustFixedExecutionPolicy(llm.DefaultMaxOutputTokens)
	estimator := llm.NewApproxUsageEstimator()

	handler := NewHandler(
		Config{Enabled: true, QuoteTTLSeconds: 300},
		DefaultValidator(),
		messenger,
		invoices,
		backend,
		policy,
		estimator,
		jobs,
		replaystore.New(0),
		nil,
		logger,
	)
	handler.clock = clock
	handler.replay = nil

	req := newLLMChatQuoteRequest("gpt-5.2", func(r *lcpwire.QuoteRequest) {
		r.Envelope.Expiry = uint64(clock.now.Add(30 * time.Second).Unix())
		r.Input = []byte(promptText)
	})
	payload, err := lcpwire.EncodeQuoteRequest(req)
	if err != nil {
		t.Fatalf("encode quote_request: %v", err)
	}

	handler.HandleInboundCustomMessage(context.Background(), lndpeermsg.InboundCustomMessage{
		PeerPubKey: "peer1",
		MsgType:    uint16(lcpwire.MessageTypeQuoteRequest),
		Payload:    payload,
	})
	key := jobstore.Key{PeerPubKey: "peer1", JobID: req.Envelope.JobID}

	if invoices.createCalls() == 0 {
		t.Fatalf("expected invoice to be created")
	}

	// Release settlement to allow runJob to proceed.
	invoices.Settle()

	waitFor(t, time.Second, func() bool {
		job, ok := jobs.Get(key)
		return ok && job.State == jobstore.StateDone
	})

	messages := messenger.messages()
	if len(messages) < 2 {
		t.Fatalf("expected quote_response + result, got %d messages", len(messages))
	}
	gotResult, decodeErr := lcpwire.DecodeResult(messages[len(messages)-1].payload)
	if decodeErr != nil {
		t.Fatalf("decode result: %v", decodeErr)
	}
	if gotResult.ContentType == nil || *gotResult.ContentType != contentTypeText {
		t.Fatalf("content_type mismatch: %v", gotResult.ContentType)
	}

	entries := observed.All()
	if len(entries) == 0 {
		t.Fatalf("expected logs to be emitted")
	}

	sensitive := []string{promptText, outputText, paymentRequest}
	for _, entry := range entries {
		assertNotContainsAny(t, entry.Message, sensitive...)
		for _, v := range entry.ContextMap() {
			s, ok := v.(string)
			if !ok {
				continue
			}
			assertNotContainsAny(t, s, sensitive...)
		}
	}
}

func assertNotContainsAny(t *testing.T, haystack string, needles ...string) {
	t.Helper()
	for _, needle := range needles {
		if strings.Contains(haystack, needle) {
			t.Fatalf("unexpected sensitive content %q in %q", needle, haystack)
		}
	}
}

type fakeClock struct {
	now time.Time
}

func (f *fakeClock) Now() time.Time { return f.now }

type fakeMessenger struct {
	mu   sync.Mutex
	sent []sentMessage
}

type sentMessage struct {
	peerPubKey string
	msgType    lcpwire.MessageType
	payload    []byte
}

func (f *fakeMessenger) SendCustomMessage(
	_ context.Context,
	peerPubKey string,
	msgType lcpwire.MessageType,
	payload []byte,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sentMessage{
		peerPubKey: peerPubKey,
		msgType:    msgType,
		payload:    payload,
	})
	return nil
}

func (f *fakeMessenger) messages() []sentMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]sentMessage, len(f.sent))
	copy(out, f.sent)
	return out
}

type fakeInvoiceCreator struct {
	mu      sync.Mutex
	result  InvoiceResult
	err     error
	settleC chan struct{}
	created int
}

func newFakeInvoiceCreator() *fakeInvoiceCreator {
	return &fakeInvoiceCreator{
		settleC: make(chan struct{}),
	}
}

func (f *fakeInvoiceCreator) CreateInvoice(
	_ context.Context,
	_ InvoiceRequest,
) (InvoiceResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created++
	if f.err != nil {
		return InvoiceResult{}, f.err
	}
	return f.result, nil
}

func (f *fakeInvoiceCreator) WaitForSettlement(ctx context.Context, _ lcp.Hash32, _ uint64) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-f.settleC:
		return nil
	}
}

func (f *fakeInvoiceCreator) createCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.created
}

func (f *fakeInvoiceCreator) Settle() {
	f.mu.Lock()
	select {
	case <-f.settleC:
	default:
		close(f.settleC)
	}
	f.mu.Unlock()
}

type fakeBackend struct {
	result computebackend.ExecutionResult
	err    error
}

func (f *fakeBackend) Execute(
	context.Context,
	computebackend.Task,
) (computebackend.ExecutionResult, error) {
	if f.err != nil {
		return computebackend.ExecutionResult{}, f.err
	}
	return f.result, nil
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.After(timeout)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline:
			t.Fatalf("condition not met within %s", timeout)
		case <-tick.C:
			if condition() {
				return
			}
		}
	}
}

func mustHash32(fill byte) lcp.Hash32 {
	h := hash(fill)
	return h
}

func hash(fill byte) lcp.Hash32 {
	var h lcp.Hash32
	for i := range h {
		h[i] = fill
	}
	return h
}

func fixedMsgID(fill byte) lcpwire.MsgID {
	var id lcpwire.MsgID
	for i := range id {
		id[i] = fill
	}
	return id
}

func mustQuotePriceForPrompt(
	t *testing.T,
	policy llm.ExecutionPolicyProvider,
	estimator llm.UsageEstimator,
	model string,
	input []byte,
) llm.PriceBreakdown {
	t.Helper()

	task, err := policy.Apply(computebackend.Task{
		TaskKind:   "llm.chat",
		Model:      model,
		InputBytes: append([]byte(nil), input...),
	})
	if err != nil {
		t.Fatalf("plan task: %v", err)
	}

	estimation, err := estimator.Estimate(task, policy.Policy())
	if err != nil {
		t.Fatalf("estimate usage: %v", err)
	}

	price, err := llm.QuotePrice(
		model,
		estimation.Usage,
		0, // cachedInputTokens
		llm.DefaultPriceTable(),
	)
	if err != nil {
		t.Fatalf("quote price: %v", err)
	}
	return price
}
