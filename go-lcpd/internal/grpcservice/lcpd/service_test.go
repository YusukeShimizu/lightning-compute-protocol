//nolint:testpackage // Tests validate internal helpers/state for gRPC service.
package lcpd

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"
	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
	"github.com/bruwbird/lcp/go-lcpd/internal/lightningrpc"
	"github.com/bruwbird/lcp/go-lcpd/internal/lndpeermsg"
	"github.com/bruwbird/lcp/go-lcpd/internal/peerdirectory"
	"github.com/bruwbird/lcp/go-lcpd/internal/protocolcompat"
	"github.com/bruwbird/lcp/go-lcpd/internal/requesterjobstore"
	"github.com/bruwbird/lcp/go-lcpd/internal/requesterwait"
	lcpdv1 "github.com/bruwbird/lcp/proto-go/lcpd/v1"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type fakeClock struct {
	now time.Time
}

func (f fakeClock) Now() time.Time { return f.now }

type fakeLightning struct {
	mu           sync.Mutex
	getInfoFn    func(context.Context) (lightningrpc.Info, error)
	decodeFn     func(context.Context, string) (lightningrpc.PaymentRequestInfo, error)
	payInvoiceFn func(context.Context, string) (lcp.Hash32, error)
}

func (f *fakeLightning) GetInfo(ctx context.Context) (lightningrpc.Info, error) {
	f.mu.Lock()
	fn := f.getInfoFn
	f.mu.Unlock()
	if fn == nil {
		return lightningrpc.Info{}, errors.New("GetInfo not implemented")
	}
	return fn(ctx)
}

func (f *fakeLightning) DecodePaymentRequest(
	ctx context.Context,
	paymentRequest string,
) (lightningrpc.PaymentRequestInfo, error) {
	f.mu.Lock()
	fn := f.decodeFn
	f.mu.Unlock()
	if fn == nil {
		return lightningrpc.PaymentRequestInfo{}, errors.New("DecodePaymentRequest not implemented")
	}
	return fn(ctx, paymentRequest)
}

func (f *fakeLightning) PayInvoice(ctx context.Context, paymentRequest string) (lcp.Hash32, error) {
	f.mu.Lock()
	fn := f.payInvoiceFn
	f.mu.Unlock()
	if fn == nil {
		return lcp.Hash32{}, errors.New("PayInvoice not implemented")
	}
	return fn(ctx, paymentRequest)
}

type sentMsg struct {
	peerID  string
	msgType lcpwire.MessageType
	payload []byte
}

type fakeMessenger struct {
	mu     sync.Mutex
	sent   []sentMsg
	sendFn func(context.Context, sentMsg) error
}

func (m *fakeMessenger) SendCustomMessage(
	ctx context.Context,
	peerPubKey string,
	msgType lcpwire.MessageType,
	payload []byte,
) error {
	msg := sentMsg{
		peerID:  peerPubKey,
		msgType: msgType,
		payload: append([]byte(nil), payload...),
	}

	m.mu.Lock()
	m.sent = append(m.sent, msg)
	fn := m.sendFn
	m.mu.Unlock()

	if fn == nil {
		return nil
	}
	return fn(ctx, msg)
}

func (m *fakeMessenger) messages() []sentMsg {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]sentMsg, len(m.sent))
	copy(out, m.sent)
	return out
}

func TestGetLocalInfo_UnavailableWhenNotConfigured(t *testing.T) {
	t.Parallel()

	svc := mustNewService(t, Params{
		PeerDirectory: peerdirectory.New(),
	})

	_, err := svc.GetLocalInfo(context.Background(), &lcpdv1.GetLocalInfoRequest{})
	assertStatusCode(t, err, codes.Unavailable)
}

func TestGetLocalInfo_ReturnsManifestAndNodeID(t *testing.T) {
	t.Parallel()

	nodeID := "02" + strings.Repeat("a", 64)
	ln := &fakeLightning{
		getInfoFn: func(context.Context) (lightningrpc.Info, error) {
			return lightningrpc.Info{IdentityPubKey: nodeID}, nil
		},
	}

	maxPayload := uint32(1234)
	localManifest := &lcpwire.Manifest{
		ProtocolVersion: lcpwire.ProtocolVersionV01,
		MaxPayloadBytes: &maxPayload,
	}

	svc := mustNewService(t, Params{
		PeerDirectory: peerdirectory.New(),
		LocalManifest: localManifest,
		LightningRPC:  ln,
	})

	resp, err := svc.GetLocalInfo(context.Background(), &lcpdv1.GetLocalInfoRequest{})
	if err != nil {
		t.Fatalf("GetLocalInfo: %v", err)
	}
	if got, want := resp.GetNodeId(), nodeID; got != want {
		t.Fatalf("node_id mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
	if got, want := resp.GetManifest().GetMaxPayloadBytes(), uint32(1234); got != want {
		t.Fatalf("max_payload_bytes mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
}

func TestRequestQuote_SuccessStoresQuote(t *testing.T) {
	t.Parallel()

	peerID := "02" + strings.Repeat("a", 64)
	peers := peerdirectory.New()
	peers.MarkConnected(peerID)
	peers.MarkCustomMsgEnabled(peerID, true)
	peers.MarkLCPReady(peerID, lcpwire.Manifest{
		ProtocolVersion: lcpwire.ProtocolVersionV01,
		MaxPayloadBytes: ptrU32(65535),
	})

	clock := fakeClock{now: time.Unix(1_700_000_000, 0)}
	jobs := requesterjobstore.NewWithClock(clock.Now)
	waiter := requesterwait.New(nil)

	messenger := &fakeMessenger{
		sendFn: func(_ context.Context, msg sentMsg) error {
			req, err := lcpwire.DecodeQuoteRequest(msg.payload)
			if err != nil {
				return err
			}

			quoteExpiry := uint64(clock.Now().Unix()) + 60
			paramsBytes := []byte(nil)
			if req.ParamsBytes != nil {
				paramsBytes = *req.ParamsBytes
			}
			termsHash, err := protocolcompat.ComputeTermsHash(lcp.Terms{
				ProtocolVersion: req.Envelope.ProtocolVersion,
				JobID:           req.Envelope.JobID,
				PriceMsat:       123,
				QuoteExpiry:     quoteExpiry,
			}, protocolcompat.TermsCommit{
				TaskKind: req.TaskKind,
				Input:    req.Input,
				Params:   paramsBytes,
			})
			if err != nil {
				return err
			}

			respPayload, err := lcpwire.EncodeQuoteResponse(lcpwire.QuoteResponse{
				Envelope: lcpwire.JobEnvelope{
					ProtocolVersion: req.Envelope.ProtocolVersion,
					JobID:           req.Envelope.JobID,
					MsgID:           lcpwire.MsgID{},
					Expiry:          req.Envelope.Expiry,
				},
				PriceMsat:      123,
				QuoteExpiry:    quoteExpiry,
				TermsHash:      termsHash,
				PaymentRequest: "lnbc1dummy",
			})
			if err != nil {
				return err
			}

			waiter.HandleInboundCustomMessage(
				context.Background(),
				inbound(peerID, lcpwire.MessageTypeQuoteResponse, respPayload),
			)
			return nil
		},
	}

	svc := mustNewService(t, Params{
		Clock:         clock,
		PeerDirectory: peers,
		PeerMessenger: messenger,
		JobStore:      jobs,
		Waiter:        waiter,
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	resp, err := svc.RequestQuote(ctx, &lcpdv1.RequestQuoteRequest{
		PeerId: peerID,
		Task:   llmChatTask("hello", "model-2"),
	})
	if err != nil {
		t.Fatalf("RequestQuote: %v", err)
	}

	if got, want := resp.GetPeerId(), peerID; got != want {
		t.Fatalf("peer_id mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}

	terms := resp.GetTerms()
	if terms == nil {
		t.Fatalf("RequestQuote: terms is nil")
	}
	if got, want := terms.GetPriceMsat(), uint64(123); got != want {
		t.Fatalf("price_msat mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
	if got, want := terms.GetPaymentRequest(), "lnbc1dummy"; got != want {
		t.Fatalf("payment_request mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}

	storedJobID, err := toJobID(terms.GetJobId())
	if err != nil {
		t.Fatalf("toJobID: %v", err)
	}
	storedTerms, err := jobs.GetTerms(peerID, storedJobID)
	if err != nil {
		t.Fatalf("jobs.GetTerms: %v", err)
	}
	if got, want := storedTerms.GetPaymentRequest(), "lnbc1dummy"; got != want {
		t.Fatalf("stored payment_request mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
}

func TestRequestQuote_RespectsRemoteMaxPayloadBytes(t *testing.T) {
	t.Parallel()

	peerID := "02" + strings.Repeat("b", 64)
	peers := peerdirectory.New()
	peers.MarkConnected(peerID)
	peers.MarkCustomMsgEnabled(peerID, true)
	peers.MarkLCPReady(peerID, lcpwire.Manifest{
		ProtocolVersion: lcpwire.ProtocolVersionV01,
		MaxPayloadBytes: ptrU32(1),
	})

	messenger := &fakeMessenger{}

	svc := mustNewService(t, Params{
		PeerDirectory: peers,
		PeerMessenger: messenger,
		JobStore:      requesterjobstore.New(),
		Waiter:        requesterwait.New(nil),
	})

	_, err := svc.RequestQuote(context.Background(), &lcpdv1.RequestQuoteRequest{
		PeerId: peerID,
		Task:   llmChatTask("hi", "model-1"),
	})
	assertStatusCode(t, err, codes.ResourceExhausted)
	if got := len(messenger.messages()); got != 0 {
		t.Fatalf("expected no messages sent, got %d", got)
	}
}

func TestRequestQuote_PeerNotFound(t *testing.T) {
	t.Parallel()

	svc := mustNewService(t, Params{
		PeerDirectory: peerdirectory.New(),
		PeerMessenger: &fakeMessenger{},
		JobStore:      requesterjobstore.New(),
		Waiter:        requesterwait.New(nil),
	})

	_, err := svc.RequestQuote(context.Background(), &lcpdv1.RequestQuoteRequest{
		PeerId: "02" + strings.Repeat("c", 64),
		Task:   llmChatTask("hello", "model-1"),
	})
	assertStatusCode(t, err, codes.NotFound)
}

func TestRequestQuote_PeerNotReady(t *testing.T) {
	t.Parallel()

	peerID := "02" + strings.Repeat("d", 64)
	peers := peerdirectory.New()
	peers.MarkConnected(peerID)
	peers.MarkCustomMsgEnabled(peerID, true)

	svc := mustNewService(t, Params{
		PeerDirectory: peers,
		PeerMessenger: &fakeMessenger{},
		JobStore:      requesterjobstore.New(),
		Waiter:        requesterwait.New(nil),
	})

	_, err := svc.RequestQuote(context.Background(), &lcpdv1.RequestQuoteRequest{
		PeerId: peerID,
		Task:   llmChatTask("hello", "model-1"),
	})
	assertStatusCode(t, err, codes.FailedPrecondition)
}

func TestAcceptAndExecute_SuccessWaitsForResult(t *testing.T) {
	t.Parallel()

	peerID := "02" + strings.Repeat("e", 64)
	clock := fakeClock{now: time.Unix(1_700_000_000, 0)}
	jobs := requesterjobstore.NewWithClock(clock.Now)
	waiter := requesterwait.New(nil)

	var jobID lcp.JobID
	for i := range jobID {
		jobID[i] = 0x11
	}

	quoteExpiry := uint64(clock.Now().Unix()) + 60
	paramsBytes, err := lcpwire.EncodeLLMChatParams(lcpwire.LLMChatParams{Profile: "model-1"})
	if err != nil {
		t.Fatalf("EncodeLLMChatParams: %v", err)
	}
	termsHash, err := protocolcompat.ComputeTermsHash(lcp.Terms{
		ProtocolVersion: lcpwire.ProtocolVersionV01,
		JobID:           jobID,
		PriceMsat:       123,
		QuoteExpiry:     quoteExpiry,
	}, protocolcompat.TermsCommit{
		TaskKind: "llm.chat",
		Input:    []byte("hello"),
		Params:   paramsBytes,
	})
	if err != nil {
		t.Fatalf("ComputeTermsHash: %v", err)
	}

	terms := &lcpdv1.Terms{
		ProtocolVersion: uint32(lcpwire.ProtocolVersionV01),
		JobId:           append([]byte(nil), jobID[:]...),
		PriceMsat:       123,
		QuoteExpiry:     timestamppb.New(time.Unix(int64(quoteExpiry), 0)),
		TermsHash:       append([]byte(nil), termsHash[:]...),
		PaymentRequest:  "lnbc1dummy",
	}

	if putErr := jobs.PutQuote(peerID, llmChatTask("hello", "model-1"), terms); putErr != nil {
		t.Fatalf("PutQuote: %v", putErr)
	}

	ln := &fakeLightning{
		decodeFn: func(context.Context, string) (lightningrpc.PaymentRequestInfo, error) {
			return lightningrpc.PaymentRequestInfo{
				DescriptionHash: termsHash,
				PayeePubKey:     peerID,
				AmountMsat:      123,
				TimestampUnix:   int64(quoteExpiry) - 30,
				ExpirySeconds:   30,
			}, nil
		},
		payInvoiceFn: func(context.Context, string) (lcp.Hash32, error) {
			var preimage lcp.Hash32
			preimage[0] = 0xaa
			return preimage, nil
		},
	}

	resultPayload, err := lcpwire.EncodeResult(lcpwire.Result{
		Envelope: lcpwire.JobEnvelope{
			ProtocolVersion: lcpwire.ProtocolVersionV01,
			JobID:           jobID,
			MsgID:           lcpwire.MsgID{},
			Expiry:          uint64(clock.Now().Unix()) + 300,
		},
		Result:      []byte("ok"),
		ContentType: ptrString("text/plain"),
	})
	if err != nil {
		t.Fatalf("EncodeResult: %v", err)
	}
	waiter.HandleInboundCustomMessage(
		context.Background(),
		inbound(peerID, lcpwire.MessageTypeResult, resultPayload),
	)

	svc := mustNewService(t, Params{
		Clock:         clock,
		JobStore:      jobs,
		Waiter:        waiter,
		LightningRPC:  ln,
		PeerDirectory: peerdirectory.New(),
	})

	resp, err := svc.AcceptAndExecute(context.Background(), &lcpdv1.AcceptAndExecuteRequest{
		PeerId:     peerID,
		JobId:      append([]byte(nil), jobID[:]...),
		PayInvoice: true,
	})
	if err != nil {
		t.Fatalf("AcceptAndExecute: %v", err)
	}

	if got, want := string(resp.GetResult().GetResult()), "ok"; got != want {
		t.Fatalf("result mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
	if got, want := resp.GetResult().GetContentType(), "text/plain"; got != want {
		t.Fatalf("content_type mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
}

func TestAcceptAndExecute_FailsOnInvoiceAmountMismatch(t *testing.T) {
	t.Parallel()

	peerID := "02" + strings.Repeat("e", 64)
	clock := fakeClock{now: time.Unix(1_700_000_000, 0)}
	jobs := requesterjobstore.NewWithClock(clock.Now)

	var jobID lcp.JobID
	for i := range jobID {
		jobID[i] = 0x11
	}

	quoteExpiry := uint64(clock.Now().Unix()) + 60
	paramsBytes, err := lcpwire.EncodeLLMChatParams(lcpwire.LLMChatParams{Profile: "model-1"})
	if err != nil {
		t.Fatalf("EncodeLLMChatParams: %v", err)
	}
	termsHash, err := protocolcompat.ComputeTermsHash(lcp.Terms{
		ProtocolVersion: lcpwire.ProtocolVersionV01,
		JobID:           jobID,
		PriceMsat:       123,
		QuoteExpiry:     quoteExpiry,
	}, protocolcompat.TermsCommit{
		TaskKind: "llm.chat",
		Input:    []byte("hello"),
		Params:   paramsBytes,
	})
	if err != nil {
		t.Fatalf("ComputeTermsHash: %v", err)
	}

	terms := &lcpdv1.Terms{
		ProtocolVersion: uint32(lcpwire.ProtocolVersionV01),
		JobId:           append([]byte(nil), jobID[:]...),
		PriceMsat:       123,
		QuoteExpiry:     timestamppb.New(time.Unix(int64(quoteExpiry), 0)),
		TermsHash:       append([]byte(nil), termsHash[:]...),
		PaymentRequest:  "lnbc1dummy",
	}
	if putErr := jobs.PutQuote(peerID, llmChatTask("hello", "model-1"), terms); putErr != nil {
		t.Fatalf("PutQuote: %v", putErr)
	}

	ln := &fakeLightning{
		decodeFn: func(context.Context, string) (lightningrpc.PaymentRequestInfo, error) {
			return lightningrpc.PaymentRequestInfo{
				DescriptionHash: termsHash,
				PayeePubKey:     peerID,
				AmountMsat:      124,
				TimestampUnix:   int64(quoteExpiry) - 30,
				ExpirySeconds:   30,
			}, nil
		},
	}

	svc := mustNewService(t, Params{
		Clock:         clock,
		JobStore:      jobs,
		Waiter:        requesterwait.New(nil),
		LightningRPC:  ln,
		PeerDirectory: peerdirectory.New(),
	})

	_, err = svc.AcceptAndExecute(context.Background(), &lcpdv1.AcceptAndExecuteRequest{
		PeerId:     peerID,
		JobId:      append([]byte(nil), jobID[:]...),
		PayInvoice: true,
	})
	assertStatusCode(t, err, codes.FailedPrecondition)
}

func TestAcceptAndExecute_FailsOnInvoiceExpiryMismatch(t *testing.T) {
	t.Parallel()

	peerID := "02" + strings.Repeat("e", 64)
	clock := fakeClock{now: time.Unix(1_700_000_000, 0)}
	jobs := requesterjobstore.NewWithClock(clock.Now)

	var jobID lcp.JobID
	for i := range jobID {
		jobID[i] = 0x11
	}

	quoteExpiry := uint64(clock.Now().Unix()) + 60
	paramsBytes, err := lcpwire.EncodeLLMChatParams(lcpwire.LLMChatParams{Profile: "model-1"})
	if err != nil {
		t.Fatalf("EncodeLLMChatParams: %v", err)
	}
	termsHash, err := protocolcompat.ComputeTermsHash(lcp.Terms{
		ProtocolVersion: lcpwire.ProtocolVersionV01,
		JobID:           jobID,
		PriceMsat:       123,
		QuoteExpiry:     quoteExpiry,
	}, protocolcompat.TermsCommit{
		TaskKind: "llm.chat",
		Input:    []byte("hello"),
		Params:   paramsBytes,
	})
	if err != nil {
		t.Fatalf("ComputeTermsHash: %v", err)
	}

	terms := &lcpdv1.Terms{
		ProtocolVersion: uint32(lcpwire.ProtocolVersionV01),
		JobId:           append([]byte(nil), jobID[:]...),
		PriceMsat:       123,
		QuoteExpiry:     timestamppb.New(time.Unix(int64(quoteExpiry), 0)),
		TermsHash:       append([]byte(nil), termsHash[:]...),
		PaymentRequest:  "lnbc1dummy",
	}
	if putErr := jobs.PutQuote(peerID, llmChatTask("hello", "model-1"), terms); putErr != nil {
		t.Fatalf("PutQuote: %v", putErr)
	}

	ln := &fakeLightning{
		decodeFn: func(context.Context, string) (lightningrpc.PaymentRequestInfo, error) {
			return lightningrpc.PaymentRequestInfo{
				DescriptionHash: termsHash,
				PayeePubKey:     peerID,
				AmountMsat:      123,
				TimestampUnix:   int64(quoteExpiry),
				ExpirySeconds:   60,
			}, nil
		},
	}

	svc := mustNewService(t, Params{
		Clock:         clock,
		JobStore:      jobs,
		Waiter:        requesterwait.New(nil),
		LightningRPC:  ln,
		PeerDirectory: peerdirectory.New(),
	})

	_, err = svc.AcceptAndExecute(context.Background(), &lcpdv1.AcceptAndExecuteRequest{
		PeerId:     peerID,
		JobId:      append([]byte(nil), jobID[:]...),
		PayInvoice: true,
	})
	assertStatusCode(t, err, codes.FailedPrecondition)
}

func TestAcceptAndExecute_AllowedClockSkewIsConfigurableViaEnv(t *testing.T) {
	t.Setenv("LCP_ALLOWED_CLOCK_SKEW_SECONDS", "0")

	peerID := "02" + strings.Repeat("e", 64)
	clock := fakeClock{now: time.Unix(1_700_000_000, 0)}
	jobs := requesterjobstore.NewWithClock(clock.Now)

	var jobID lcp.JobID
	for i := range jobID {
		jobID[i] = 0x11
	}

	quoteExpiry := uint64(clock.Now().Unix()) + 60
	paramsBytes, err := lcpwire.EncodeLLMChatParams(lcpwire.LLMChatParams{Profile: "model-1"})
	if err != nil {
		t.Fatalf("EncodeLLMChatParams: %v", err)
	}
	termsHash, err := protocolcompat.ComputeTermsHash(lcp.Terms{
		ProtocolVersion: lcpwire.ProtocolVersionV01,
		JobID:           jobID,
		PriceMsat:       123,
		QuoteExpiry:     quoteExpiry,
	}, protocolcompat.TermsCommit{
		TaskKind: "llm.chat",
		Input:    []byte("hello"),
		Params:   paramsBytes,
	})
	if err != nil {
		t.Fatalf("ComputeTermsHash: %v", err)
	}

	terms := &lcpdv1.Terms{
		ProtocolVersion: uint32(lcpwire.ProtocolVersionV01),
		JobId:           append([]byte(nil), jobID[:]...),
		PriceMsat:       123,
		QuoteExpiry:     timestamppb.New(time.Unix(int64(quoteExpiry), 0)),
		TermsHash:       append([]byte(nil), termsHash[:]...),
		PaymentRequest:  "lnbc1dummy",
	}
	if putErr := jobs.PutQuote(peerID, llmChatTask("hello", "model-1"), terms); putErr != nil {
		t.Fatalf("PutQuote: %v", putErr)
	}

	ln := &fakeLightning{
		decodeFn: func(context.Context, string) (lightningrpc.PaymentRequestInfo, error) {
			return lightningrpc.PaymentRequestInfo{
				DescriptionHash: termsHash,
				PayeePubKey:     peerID,
				AmountMsat:      123,
				TimestampUnix:   int64(quoteExpiry),
				ExpirySeconds:   1,
			}, nil
		},
	}

	svc := mustNewService(t, Params{
		Clock:         clock,
		JobStore:      jobs,
		Waiter:        requesterwait.New(nil),
		LightningRPC:  ln,
		PeerDirectory: peerdirectory.New(),
	})

	_, err = svc.AcceptAndExecute(context.Background(), &lcpdv1.AcceptAndExecuteRequest{
		PeerId:     peerID,
		JobId:      append([]byte(nil), jobID[:]...),
		PayInvoice: true,
	})
	assertStatusCode(t, err, codes.FailedPrecondition)
}

func TestAcceptAndExecute_RejectsPayInvoiceFalse(t *testing.T) {
	t.Parallel()

	svc := mustNewService(t, Params{
		PeerDirectory: peerdirectory.New(),
		JobStore:      requesterjobstore.New(),
		Waiter:        requesterwait.New(nil),
		LightningRPC:  &fakeLightning{},
	})

	_, err := svc.AcceptAndExecute(context.Background(), &lcpdv1.AcceptAndExecuteRequest{
		PeerId:     "02" + strings.Repeat("f", 64),
		JobId:      make([]byte, 32),
		PayInvoice: false,
	})
	assertStatusCode(t, err, codes.InvalidArgument)
}

func TestAcceptAndExecute_JobNotFound(t *testing.T) {
	t.Parallel()

	svc := mustNewService(t, Params{
		PeerDirectory: peerdirectory.New(),
		JobStore:      requesterjobstore.New(),
		Waiter:        requesterwait.New(nil),
		LightningRPC:  &fakeLightning{},
	})

	_, err := svc.AcceptAndExecute(context.Background(), &lcpdv1.AcceptAndExecuteRequest{
		PeerId:     "02" + strings.Repeat("f", 64),
		JobId:      make([]byte, 32),
		PayInvoice: true,
	})
	assertStatusCode(t, err, codes.NotFound)
}

func TestCancelJob_SendsMessage(t *testing.T) {
	t.Parallel()

	peerID := "02" + strings.Repeat("a", 64)
	var jobID lcp.JobID
	jobID[0] = 0x42

	peers := peerdirectory.New()
	peers.MarkConnected(peerID)
	peers.MarkCustomMsgEnabled(peerID, true)

	messenger := &fakeMessenger{}

	svc := mustNewService(t, Params{
		PeerDirectory: peers,
		PeerMessenger: messenger,
		JobStore:      requesterjobstore.New(),
		Waiter:        requesterwait.New(nil),
	})

	resp, err := svc.CancelJob(context.Background(), &lcpdv1.CancelJobRequest{
		PeerId: peerID,
		JobId:  append([]byte(nil), jobID[:]...),
		Reason: "stop",
	})
	if err != nil {
		t.Fatalf("CancelJob: %v", err)
	}
	if got, want := resp.GetSuccess(), true; got != want {
		t.Fatalf("success mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}

	msgs := messenger.messages()
	if got, want := len(msgs), 1; got != want {
		t.Fatalf("sent message count mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
	if got, want := msgs[0].msgType, lcpwire.MessageTypeCancel; got != want {
		t.Fatalf("sent msg_type mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
}

func mustNewService(t *testing.T, p Params) *Service {
	t.Helper()

	server := New(p)
	svc, ok := server.(*Service)
	if !ok {
		t.Fatalf("New: expected *Service, got %T", server)
	}
	return svc
}

func assertStatusCode(t *testing.T, err error, want codes.Code) {
	t.Helper()

	if err == nil {
		t.Fatalf("expected gRPC error code=%s, got nil", want)
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected status error, got %T", err)
	}
	if got := st.Code(); got != want {
		t.Fatalf("status code mismatch (-want +got):\n%s", cmp.Diff(want.String(), got.String()))
	}
}

func llmChatTask(prompt, profile string) *lcpdv1.Task {
	return &lcpdv1.Task{
		Spec: &lcpdv1.Task_LlmChat{
			LlmChat: &lcpdv1.LLMChatTaskSpec{
				Prompt: prompt,
				Params: &lcpdv1.LLMChatParams{Profile: profile},
			},
		},
	}
}

func ptrU32(v uint32) *uint32 { return &v }

func ptrString(s string) *string { return &s }

func inbound(
	peerID string,
	msgType lcpwire.MessageType,
	payload []byte,
) lndpeermsg.InboundCustomMessage {
	return lndpeermsg.InboundCustomMessage{
		PeerPubKey: peerID,
		MsgType:    uint16(msgType),
		Payload:    payload,
	}
}
