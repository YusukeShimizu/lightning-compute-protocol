//nolint:testpackage // White-box tests need access to unexported fields.
package provider

import (
	"context"
	"crypto/sha256"
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
)

func TestHandler_HandleInputStream_SendsQuoteResponse(t *testing.T) {
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
	peers.MarkLCPReady("peer1", lcpwire.Manifest{
		ProtocolVersion: lcpwire.ProtocolVersionV02,
		MaxPayloadBytes: 65535,
		MaxStreamBytes:  4_194_304,
		MaxJobBytes:     8_388_608,
	})

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
	inputBytes := []byte("prompt")
	payload := mustEncodeQuoteRequest(t, req)

	handler.HandleInboundCustomMessage(context.Background(), lndpeermsg.InboundCustomMessage{
		PeerPubKey: "peer1",
		MsgType:    uint16(lcpwire.MessageTypeQuoteRequest),
		Payload:    payload,
	})
	key := jobstore.Key{PeerPubKey: "peer1", JobID: req.Envelope.JobID}
	t.Cleanup(func() { handler.cancelJob(key) })

	streamID := mustHash32(0x11)
	totalLen := uint64(len(inputBytes))
	sum := sha256.Sum256(inputBytes)
	inputHash := lcp.Hash32(sum)

	beginPayload := mustEncodeStreamBegin(t, lcpwire.StreamBegin{
		Envelope: lcpwire.JobEnvelope{
			ProtocolVersion: req.Envelope.ProtocolVersion,
			JobID:           req.Envelope.JobID,
			MsgID:           fixedMsgID(0x04),
			Expiry:          req.Envelope.Expiry,
		},
		StreamID:        streamID,
		Kind:            lcpwire.StreamKindInput,
		TotalLen:        &totalLen,
		SHA256:          &inputHash,
		ContentType:     contentTypeTextPlain,
		ContentEncoding: "identity",
	})

	chunkPayload := mustEncodeStreamChunk(t, lcpwire.StreamChunk{
		Envelope: lcpwire.JobEnvelope{
			ProtocolVersion: req.Envelope.ProtocolVersion,
			JobID:           req.Envelope.JobID,
			Expiry:          req.Envelope.Expiry,
		},
		StreamID: streamID,
		Seq:      0,
		Data:     inputBytes,
	})

	endPayload := mustEncodeStreamEnd(t, lcpwire.StreamEnd{
		Envelope: lcpwire.JobEnvelope{
			ProtocolVersion: req.Envelope.ProtocolVersion,
			JobID:           req.Envelope.JobID,
			MsgID:           fixedMsgID(0x05),
			Expiry:          req.Envelope.Expiry,
		},
		StreamID: streamID,
		TotalLen: totalLen,
		SHA256:   inputHash,
	})

	handler.HandleInboundCustomMessage(context.Background(), lndpeermsg.InboundCustomMessage{
		PeerPubKey: "peer1",
		MsgType:    uint16(lcpwire.MessageTypeStreamBegin),
		Payload:    beginPayload,
	})
	handler.HandleInboundCustomMessage(context.Background(), lndpeermsg.InboundCustomMessage{
		PeerPubKey: "peer1",
		MsgType:    uint16(lcpwire.MessageTypeStreamChunk),
		Payload:    chunkPayload,
	})
	handler.HandleInboundCustomMessage(context.Background(), lndpeermsg.InboundCustomMessage{
		PeerPubKey: "peer1",
		MsgType:    uint16(lcpwire.MessageTypeStreamEnd),
		Payload:    endPayload,
	})

	sent := requireSingleSentMessage(t, messenger.messages())
	if sent.msgType != lcpwire.MessageTypeQuoteResponse {
		t.Fatalf("expected quote_response message, got %d", sent.msgType)
	}

	gotResp := mustDecodeQuoteResponse(t, sent.payload)
	price := mustQuotePriceForPrompt(t, policy, estimator, model, inputBytes)
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

	job := requireStoredJob(t, jobs, key)
	if diff := cmp.Diff(jobstore.StateWaitingPayment, job.State); diff != "" {
		t.Fatalf("job state mismatch (-want +got):\n%s", diff)
	}
	if job.PaymentHash == nil || *job.PaymentHash != invoices.result.PaymentHash {
		t.Fatalf("payment_hash mismatch")
	}
	if job.InvoiceAddIndex == nil || *job.InvoiceAddIndex != invoices.result.AddIndex {
		t.Fatalf("invoice add_index mismatch: got nil or wrong value")
	}

	paramsBytes := []byte(nil)
	if req.ParamsBytes != nil {
		paramsBytes = *req.ParamsBytes
	}
	wantTermsHash := mustComputeTermsHash(t, lcp.Terms{
		ProtocolVersion: req.Envelope.ProtocolVersion,
		JobID:           req.Envelope.JobID,
		PriceMsat:       price.PriceMsat,
		QuoteExpiry:     wantExpiry,
	}, protocolcompat.TermsCommit{
		TaskKind:             req.TaskKind,
		Input:                inputBytes,
		InputContentType:     contentTypeTextPlain,
		InputContentEncoding: "identity",
		Params:               paramsBytes,
	})
	if diff := cmp.Diff(wantTermsHash, gotResp.TermsHash); diff != "" {
		t.Fatalf("terms_hash mismatch (-want +got):\n%s", diff)
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
		ProtocolVersion: lcpwire.ProtocolVersionV02,
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
	inputBytes := []byte("prompt")
	price := mustQuotePriceForPrompt(t, policy, estimator, model, inputBytes).PriceMsat
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
		TaskKind:             req.TaskKind,
		Input:                inputBytes,
		InputContentType:     contentTypeTextPlain,
		InputContentEncoding: "identity",
		Params:               paramsBytes,
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
		manifestPeerDirectory("peer1"),
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
		manifestPeerDirectory("peer1"),
		nil,
	)
	handler.clock = clock
	handler.replay = nil

	req := newLLMChatQuoteRequest("gpt-5.2", func(r *lcpwire.QuoteRequest) {
		r.Envelope.Expiry = uint64(clock.now.Add(30 * time.Second).Unix())
	})
	inputBytes := []byte("prompt")
	payload := mustEncodeQuoteRequest(t, req)

	handler.HandleInboundCustomMessage(context.Background(), lndpeermsg.InboundCustomMessage{
		PeerPubKey: "peer1",
		MsgType:    uint16(lcpwire.MessageTypeQuoteRequest),
		Payload:    payload,
	})
	key := jobstore.Key{PeerPubKey: "peer1", JobID: req.Envelope.JobID}

	inputStreamID := mustHash32(0x11)
	inputTotalLen := uint64(len(inputBytes))
	inputSum := sha256.Sum256(inputBytes)
	inputHash := lcp.Hash32(inputSum)

	beginPayload := mustEncodeStreamBegin(t, lcpwire.StreamBegin{
		Envelope: lcpwire.JobEnvelope{
			ProtocolVersion: req.Envelope.ProtocolVersion,
			JobID:           req.Envelope.JobID,
			MsgID:           fixedMsgID(0x04),
			Expiry:          req.Envelope.Expiry,
		},
		StreamID:        inputStreamID,
		Kind:            lcpwire.StreamKindInput,
		TotalLen:        &inputTotalLen,
		SHA256:          &inputHash,
		ContentType:     contentTypeTextPlain,
		ContentEncoding: "identity",
	})

	chunkPayload := mustEncodeStreamChunk(t, lcpwire.StreamChunk{
		Envelope: lcpwire.JobEnvelope{
			ProtocolVersion: req.Envelope.ProtocolVersion,
			JobID:           req.Envelope.JobID,
			Expiry:          req.Envelope.Expiry,
		},
		StreamID: inputStreamID,
		Seq:      0,
		Data:     inputBytes,
	})

	endPayload := mustEncodeStreamEnd(t, lcpwire.StreamEnd{
		Envelope: lcpwire.JobEnvelope{
			ProtocolVersion: req.Envelope.ProtocolVersion,
			JobID:           req.Envelope.JobID,
			MsgID:           fixedMsgID(0x05),
			Expiry:          req.Envelope.Expiry,
		},
		StreamID: inputStreamID,
		TotalLen: inputTotalLen,
		SHA256:   inputHash,
	})

	handler.HandleInboundCustomMessage(context.Background(), lndpeermsg.InboundCustomMessage{
		PeerPubKey: "peer1",
		MsgType:    uint16(lcpwire.MessageTypeStreamBegin),
		Payload:    beginPayload,
	})
	handler.HandleInboundCustomMessage(context.Background(), lndpeermsg.InboundCustomMessage{
		PeerPubKey: "peer1",
		MsgType:    uint16(lcpwire.MessageTypeStreamChunk),
		Payload:    chunkPayload,
	})
	handler.HandleInboundCustomMessage(context.Background(), lndpeermsg.InboundCustomMessage{
		PeerPubKey: "peer1",
		MsgType:    uint16(lcpwire.MessageTypeStreamEnd),
		Payload:    endPayload,
	})

	if invoices.createCalls() == 0 {
		t.Fatalf("expected invoice to be created")
	}

	// Release settlement to allow runJob to proceed.
	invoices.Settle()

	waitFor(t, 3*time.Second, func() bool { return len(messenger.messages()) >= 5 })
	messages := messenger.messages()
	wantTypes := []lcpwire.MessageType{
		lcpwire.MessageTypeQuoteResponse,
		lcpwire.MessageTypeStreamBegin,
		lcpwire.MessageTypeStreamChunk,
		lcpwire.MessageTypeStreamEnd,
		lcpwire.MessageTypeResult,
	}
	for i, wantType := range wantTypes {
		if got := messages[i].msgType; got != wantType {
			t.Fatalf("unexpected msg_type at index=%d (-want +got):\n%s", i, cmp.Diff(wantType, got))
		}
	}

	begin := mustDecodeStreamBegin(t, messages[1].payload)
	if got, want := begin.Kind, lcpwire.StreamKindResult; got != want {
		t.Fatalf("stream_begin.kind mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}

	chunk := mustDecodeStreamChunk(t, messages[2].payload)
	if got, want := chunk.StreamID, begin.StreamID; got != want {
		t.Fatalf("stream_chunk.stream_id mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
	if got, want := chunk.Seq, uint32(0); got != want {
		t.Fatalf("stream_chunk.seq mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
	if diff := cmp.Diff([]byte("hello world"), chunk.Data); diff != "" {
		t.Fatalf("stream_chunk.data mismatch (-want +got):\n%s", diff)
	}

	end := mustDecodeStreamEnd(t, messages[3].payload)
	if got, want := end.StreamID, begin.StreamID; got != want {
		t.Fatalf("stream_end.stream_id mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}

	gotResult := mustDecodeResult(t, messages[4].payload)
	if got, want := gotResult.Status, lcpwire.ResultStatusOK; got != want {
		t.Fatalf("result.status mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
	if gotResult.OK == nil {
		t.Fatalf("result ok metadata is nil")
	}
	if got, want := gotResult.OK.ResultStreamID, begin.StreamID; got != want {
		t.Fatalf("result.ok.result_stream_id mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
	if got, want := gotResult.OK.ResultContentType, contentTypeTextPlain; got != want {
		t.Fatalf("result.ok.result_content_type mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
	if got, want := gotResult.OK.ResultContentEncoding, "identity"; got != want {
		t.Fatalf("result.ok.result_content_encoding mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}

	wantSum := sha256.Sum256([]byte("hello world"))
	wantHash := lcp.Hash32(wantSum)
	if got, want := end.SHA256, wantHash; got != want {
		t.Fatalf("stream_end.sha256 mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
	if got, want := gotResult.OK.ResultHash, wantHash; got != want {
		t.Fatalf("result.ok.result_hash mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
	if got, want := gotResult.OK.ResultLen, uint64(len([]byte("hello world"))); got != want {
		t.Fatalf("result.ok.result_len mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}

	waitFor(t, time.Second, func() bool {
		job, ok := jobs.Get(key)
		return ok && job.State == jobstore.StateDone
	})
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

func mustEncodeQuoteRequest(t *testing.T, req lcpwire.QuoteRequest) []byte {
	t.Helper()

	payload, err := lcpwire.EncodeQuoteRequest(req)
	if err != nil {
		t.Fatalf("encode quote_request: %v", err)
	}
	return payload
}

func mustEncodeStreamBegin(t *testing.T, begin lcpwire.StreamBegin) []byte {
	t.Helper()

	payload, err := lcpwire.EncodeStreamBegin(begin)
	if err != nil {
		t.Fatalf("encode stream_begin: %v", err)
	}
	return payload
}

func mustEncodeStreamChunk(t *testing.T, chunk lcpwire.StreamChunk) []byte {
	t.Helper()

	payload, _, err := lcpwire.EncodeStreamChunk(chunk)
	if err != nil {
		t.Fatalf("encode stream_chunk: %v", err)
	}
	return payload
}

func mustEncodeStreamEnd(t *testing.T, end lcpwire.StreamEnd) []byte {
	t.Helper()

	payload, err := lcpwire.EncodeStreamEnd(end)
	if err != nil {
		t.Fatalf("encode stream_end: %v", err)
	}
	return payload
}

func mustDecodeQuoteResponse(t *testing.T, payload []byte) lcpwire.QuoteResponse {
	t.Helper()

	resp, err := lcpwire.DecodeQuoteResponse(payload)
	if err != nil {
		t.Fatalf("decode quote_response: %v", err)
	}
	return resp
}

func mustDecodeStreamBegin(t *testing.T, payload []byte) lcpwire.StreamBegin {
	t.Helper()

	begin, err := lcpwire.DecodeStreamBegin(payload)
	if err != nil {
		t.Fatalf("decode stream_begin: %v", err)
	}
	return begin
}

func mustDecodeStreamChunk(t *testing.T, payload []byte) lcpwire.StreamChunk {
	t.Helper()

	chunk, err := lcpwire.DecodeStreamChunk(payload)
	if err != nil {
		t.Fatalf("decode stream_chunk: %v", err)
	}
	return chunk
}

func mustDecodeStreamEnd(t *testing.T, payload []byte) lcpwire.StreamEnd {
	t.Helper()

	end, err := lcpwire.DecodeStreamEnd(payload)
	if err != nil {
		t.Fatalf("decode stream_end: %v", err)
	}
	return end
}

func mustDecodeResult(t *testing.T, payload []byte) lcpwire.Result {
	t.Helper()

	result, err := lcpwire.DecodeResult(payload)
	if err != nil {
		t.Fatalf("decode lcp_result: %v", err)
	}
	return result
}

func mustComputeTermsHash(t *testing.T, terms lcp.Terms, commit protocolcompat.TermsCommit) lcp.Hash32 {
	t.Helper()

	hash, err := protocolcompat.ComputeTermsHash(terms, commit)
	if err != nil {
		t.Fatalf("ComputeTermsHash: %v", err)
	}
	return hash
}

func requireSingleSentMessage(t *testing.T, messages []sentMessage) sentMessage {
	t.Helper()

	if got, want := len(messages), 1; got != want {
		t.Fatalf("expected 1 message sent, got %d", got)
	}
	return messages[0]
}

func requireStoredJob(t *testing.T, jobs *jobstore.Store, key jobstore.Key) jobstore.Job {
	t.Helper()

	job, ok := jobs.Get(key)
	if !ok {
		t.Fatalf("job not stored")
	}
	return job
}
