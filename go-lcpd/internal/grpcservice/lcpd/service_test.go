//nolint:testpackage // Tests validate internal helpers/state for gRPC service.
package lcpd

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
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
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type fakeClock struct {
	now time.Time
}

func (f fakeClock) Now() time.Time { return f.now }

const (
	requestContentTypeJSON  = "application/json; charset=utf-8"
	contentEncodingIdentity = "identity"
)

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

type recordingStreamServer struct {
	ctx  context.Context
	sent []*lcpdv1.AcceptAndExecuteStreamResponse
}

func (s *recordingStreamServer) Send(resp *lcpdv1.AcceptAndExecuteStreamResponse) error {
	s.sent = append(s.sent, resp)
	return nil
}

func (s *recordingStreamServer) SetHeader(metadata.MD) error  { return nil }
func (s *recordingStreamServer) SendHeader(metadata.MD) error { return nil }
func (s *recordingStreamServer) SetTrailer(metadata.MD)       {}
func (s *recordingStreamServer) Context() context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}
func (s *recordingStreamServer) SendMsg(any) error { return nil }
func (s *recordingStreamServer) RecvMsg(any) error { return nil }

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
		ProtocolVersion: lcpwire.ProtocolVersionV03,
		MaxPayloadBytes: maxPayload,
		MaxStreamBytes:  1024,
		MaxCallBytes:    2048,
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
	peers.MarkManifestSent(peerID)
	remoteManifest := lcpwire.Manifest{
		ProtocolVersion: lcpwire.ProtocolVersionV03,
		MaxPayloadBytes: 65535,
		MaxStreamBytes:  4_194_304,
		MaxCallBytes:    8_388_608,
	}
	peers.MarkLCPReady(peerID, remoteManifest)

	clock := fakeClock{now: time.Unix(1_700_000_000, 0)}
	jobs := requesterjobstore.NewWithClock(clock.Now)
	waiter := requesterwait.New(nil, nil)

	inputBytes := openaiChatCompletionsRequestJSON("hello", "model-2")
	const (
		wantPriceMsat      = uint64(123)
		wantPaymentRequest = "lnbc1dummy"
	)

	messenger := newQuoteRespondingFakeMessenger(
		peerID,
		clock,
		waiter,
		inputBytes,
		wantPriceMsat,
		wantPaymentRequest,
	)

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
		Call:   openaiChatCall(t, "hello", "model-2"),
	})
	if err != nil {
		t.Fatalf("RequestQuote: %v", err)
	}

	assertRequestQuoteResponse(t, resp, peerID, wantPriceMsat, wantPaymentRequest)
	assertQuoteStored(t, jobs, peerID, resp.GetQuote(), wantPaymentRequest)
	assertSentQuoteRequestAndInputStream(t, messenger.messages(), inputBytes)
}

func TestRequestQuote_RespectsRemoteMaxPayloadBytes(t *testing.T) {
	t.Parallel()

	peerID := "02" + strings.Repeat("b", 64)
	peers := peerdirectory.New()
	peers.MarkConnected(peerID)
	peers.MarkCustomMsgEnabled(peerID, true)
	peers.MarkManifestSent(peerID)
	peers.MarkLCPReady(peerID, lcpwire.Manifest{
		ProtocolVersion: lcpwire.ProtocolVersionV03,
		MaxPayloadBytes: 1,
		MaxStreamBytes:  1024,
		MaxCallBytes:    2048,
	})

	messenger := &fakeMessenger{}

	svc := mustNewService(t, Params{
		PeerDirectory: peers,
		PeerMessenger: messenger,
		JobStore:      requesterjobstore.New(),
		Waiter:        requesterwait.New(nil, nil),
	})

	_, err := svc.RequestQuote(context.Background(), &lcpdv1.RequestQuoteRequest{
		PeerId: peerID,
		Call:   openaiChatCall(t, "hi", "model-1"),
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
		Waiter:        requesterwait.New(nil, nil),
	})

	_, err := svc.RequestQuote(context.Background(), &lcpdv1.RequestQuoteRequest{
		PeerId: "02" + strings.Repeat("c", 64),
		Call:   openaiChatCall(t, "hello", "model-1"),
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
		Waiter:        requesterwait.New(nil, nil),
	})

	_, err := svc.RequestQuote(context.Background(), &lcpdv1.RequestQuoteRequest{
		PeerId: peerID,
		Call:   openaiChatCall(t, "hello", "model-1"),
	})
	assertStatusCode(t, err, codes.FailedPrecondition)
}

func TestAcceptAndExecute_SuccessWaitsForResult(t *testing.T) {
	t.Parallel()

	peerID := "02" + strings.Repeat("e", 64)
	clock := fakeClock{now: time.Unix(1_700_000_000, 0)}
	jobs := requesterjobstore.NewWithClock(clock.Now)
	waiter := requesterwait.New(nil, nil)

	var jobID lcp.JobID
	for i := range jobID {
		jobID[i] = 0x11
	}

	quoteExpiry := uint64(clock.Now().Unix()) + 60
	requestJSON := openaiChatCompletionsRequestJSON("hello", "model-1")
	paramsBytes, err := lcpwire.EncodeOpenAIChatCompletionsV1Params(
		lcpwire.OpenAIChatCompletionsV1Params{Model: "model-1"},
	)
	if err != nil {
		t.Fatalf("EncodeOpenAIChatCompletionsV1Params: %v", err)
	}
	termsHash, err := protocolcompat.ComputeTermsHash(lcp.Terms{
		ProtocolVersion: lcpwire.ProtocolVersionV03,
		JobID:           jobID,
		PriceMsat:       123,
		QuoteExpiry:     quoteExpiry,
	}, protocolcompat.TermsCommit{
		Method:                 "openai.chat_completions.v1",
		Request:                requestJSON,
		RequestContentType:     requestContentTypeJSON,
		RequestContentEncoding: contentEncodingIdentity,
		Params:                 paramsBytes,
	})
	if err != nil {
		t.Fatalf("ComputeTermsHash: %v", err)
	}

	quote := &lcpdv1.Quote{
		ProtocolVersion: uint32(lcpwire.ProtocolVersionV03),
		CallId:          append([]byte(nil), jobID[:]...),
		PriceMsat:       123,
		QuoteExpiry:     timestamppb.New(time.Unix(int64(quoteExpiry), 0)),
		TermsHash:       append([]byte(nil), termsHash[:]...),
		PaymentRequest:  "lnbc1dummy",
	}

	if putErr := jobs.PutQuote(peerID, openaiChatCall(t, "hello", "model-1"), quote); putErr != nil {
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

	expiry := uint64(clock.Now().Unix()) + 300

	resultBytes := []byte("ok")
	resultSum := sha256.Sum256(resultBytes)
	resultHash := lcp.Hash32(resultSum)
	resultLen := uint64(len(resultBytes))

	resultContentType := "text/plain"
	resultContentEncoding := contentEncodingIdentity

	var resultStreamID lcp.Hash32
	resultStreamID[0] = 0x22

	beginPayload, err := lcpwire.EncodeStreamBegin(lcpwire.StreamBegin{
		Envelope: lcpwire.CallEnvelope{
			ProtocolVersion: lcpwire.ProtocolVersionV03,
			CallID:          jobID,
			MsgID:           lcpwire.MsgID{},
			Expiry:          expiry,
		},
		StreamID:        resultStreamID,
		Kind:            lcpwire.StreamKindResponse,
		TotalLen:        &resultLen,
		SHA256:          &resultHash,
		ContentType:     resultContentType,
		ContentEncoding: resultContentEncoding,
	})
	if err != nil {
		t.Fatalf("EncodeStreamBegin: %v", err)
	}

	chunkPayload, _, err := lcpwire.EncodeStreamChunk(lcpwire.StreamChunk{
		Envelope: lcpwire.CallEnvelope{
			ProtocolVersion: lcpwire.ProtocolVersionV03,
			CallID:          jobID,
			Expiry:          expiry,
		},
		StreamID: resultStreamID,
		Seq:      0,
		Data:     resultBytes,
	})
	if err != nil {
		t.Fatalf("EncodeStreamChunk: %v", err)
	}

	endPayload, err := lcpwire.EncodeStreamEnd(lcpwire.StreamEnd{
		Envelope: lcpwire.CallEnvelope{
			ProtocolVersion: lcpwire.ProtocolVersionV03,
			CallID:          jobID,
			MsgID:           lcpwire.MsgID{},
			Expiry:          expiry,
		},
		StreamID: resultStreamID,
		TotalLen: resultLen,
		SHA256:   resultHash,
	})
	if err != nil {
		t.Fatalf("EncodeStreamEnd: %v", err)
	}

	terminalPayload, err := lcpwire.EncodeComplete(lcpwire.Complete{
		Envelope: lcpwire.CallEnvelope{
			ProtocolVersion: lcpwire.ProtocolVersionV03,
			CallID:          jobID,
			MsgID:           lcpwire.MsgID{},
			Expiry:          expiry,
		},
		Status: lcpwire.CompleteStatusOK,
		OK: &lcpwire.CompleteOK{
			ResponseStreamID:        resultStreamID,
			ResponseHash:            resultHash,
			ResponseLen:             resultLen,
			ResponseContentType:     resultContentType,
			ResponseContentEncoding: resultContentEncoding,
		},
	})
	if err != nil {
		t.Fatalf("EncodeComplete: %v", err)
	}

	waiter.HandleInboundCustomMessage(
		context.Background(),
		inbound(peerID, lcpwire.MessageTypeStreamBegin, beginPayload),
	)
	waiter.HandleInboundCustomMessage(
		context.Background(),
		inbound(peerID, lcpwire.MessageTypeStreamChunk, chunkPayload),
	)
	waiter.HandleInboundCustomMessage(context.Background(), inbound(peerID, lcpwire.MessageTypeStreamEnd, endPayload))
	waiter.HandleInboundCustomMessage(context.Background(), inbound(peerID, lcpwire.MessageTypeComplete, terminalPayload))

	svc := mustNewService(t, Params{
		Clock:         clock,
		JobStore:      jobs,
		Waiter:        waiter,
		LightningRPC:  ln,
		PeerDirectory: peerdirectory.New(),
	})

	resp, err := svc.AcceptAndExecute(context.Background(), &lcpdv1.AcceptAndExecuteRequest{
		PeerId:     peerID,
		CallId:     append([]byte(nil), jobID[:]...),
		PayInvoice: true,
	})
	if err != nil {
		t.Fatalf("AcceptAndExecute: %v", err)
	}

	if got, want := resp.GetComplete().GetStatus(), lcpdv1.Complete_STATUS_OK; got != want {
		t.Fatalf("status mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
	if diff := cmp.Diff(resultBytes, resp.GetComplete().GetResponseBytes()); diff != "" {
		t.Fatalf("response_bytes mismatch (-want +got):\n%s", diff)
	}
	if got, want := resp.GetComplete().GetResponseContentType(), resultContentType; got != want {
		t.Fatalf("response_content_type mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
	if got, want := resp.GetComplete().GetResponseContentEncoding(), resultContentEncoding; got != want {
		t.Fatalf("response_content_encoding mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
}

//nolint:gocognit,cyclop // End-to-end streaming scenario needs full setup for correctness.
func TestAcceptAndExecuteStream_ForwardsResultStream(t *testing.T) {
	t.Parallel()

	peerID := "02" + strings.Repeat("f", 64)
	clock := fakeClock{now: time.Unix(1_700_000_000, 0)}
	jobs := requesterjobstore.NewWithClock(clock.Now)
	waiter := requesterwait.New(nil, nil)

	var jobID lcp.JobID
	for i := range jobID {
		jobID[i] = 0x33
	}

	quoteExpiry := uint64(clock.Now().Unix()) + 60
	requestJSON := openaiChatCompletionsRequestJSON("stream me", "model-stream")
	paramsBytes, err := lcpwire.EncodeOpenAIChatCompletionsV1Params(
		lcpwire.OpenAIChatCompletionsV1Params{Model: "model-stream"},
	)
	if err != nil {
		t.Fatalf("EncodeOpenAIChatCompletionsV1Params: %v", err)
	}
	termsHash, err := protocolcompat.ComputeTermsHash(lcp.Terms{
		ProtocolVersion: lcpwire.ProtocolVersionV03,
		JobID:           jobID,
		PriceMsat:       123,
		QuoteExpiry:     quoteExpiry,
	}, protocolcompat.TermsCommit{
		Method:                 "openai.chat_completions.v1",
		Request:                requestJSON,
		RequestContentType:     requestContentTypeJSON,
		RequestContentEncoding: contentEncodingIdentity,
		Params:                 paramsBytes,
	})
	if err != nil {
		t.Fatalf("ComputeTermsHash: %v", err)
	}

	quote := &lcpdv1.Quote{
		ProtocolVersion: uint32(lcpwire.ProtocolVersionV03),
		CallId:          append([]byte(nil), jobID[:]...),
		PriceMsat:       123,
		QuoteExpiry:     timestamppb.New(time.Unix(int64(quoteExpiry), 0)),
		TermsHash:       append([]byte(nil), termsHash[:]...),
		PaymentRequest:  "lnbc1stream",
	}

	if putErr := jobs.PutQuote(peerID, openaiChatCall(t, "stream me", "model-stream"), quote); putErr != nil {
		t.Fatalf("PutQuote: %v", putErr)
	}

	payCalled := make(chan struct{})
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
			select {
			case <-payCalled:
			default:
				close(payCalled)
			}
			var preimage lcp.Hash32
			preimage[0] = 0xaa
			return preimage, nil
		},
	}

	expiry := uint64(clock.Now().Unix()) + 300

	resultBytes := []byte("streamed-bytes")
	resultSum := sha256.Sum256(resultBytes)
	resultHash := lcp.Hash32(resultSum)
	resultLen := uint64(len(resultBytes))

	resultContentType := "text/event-stream; charset=utf-8"
	resultContentEncoding := contentEncodingIdentity

	var resultStreamID lcp.Hash32
	resultStreamID[0] = 0x44

	beginPayload, err := lcpwire.EncodeStreamBegin(lcpwire.StreamBegin{
		Envelope: lcpwire.CallEnvelope{
			ProtocolVersion: lcpwire.ProtocolVersionV03,
			CallID:          jobID,
			MsgID:           lcpwire.MsgID{},
			Expiry:          expiry,
		},
		StreamID:        resultStreamID,
		Kind:            lcpwire.StreamKindResponse,
		ContentType:     resultContentType,
		ContentEncoding: resultContentEncoding,
	})
	if err != nil {
		t.Fatalf("EncodeStreamBegin: %v", err)
	}

	chunkPayload, _, err := lcpwire.EncodeStreamChunk(lcpwire.StreamChunk{
		Envelope: lcpwire.CallEnvelope{
			ProtocolVersion: lcpwire.ProtocolVersionV03,
			CallID:          jobID,
			Expiry:          expiry,
		},
		StreamID: resultStreamID,
		Seq:      0,
		Data:     resultBytes,
	})
	if err != nil {
		t.Fatalf("EncodeStreamChunk: %v", err)
	}

	endPayload, err := lcpwire.EncodeStreamEnd(lcpwire.StreamEnd{
		Envelope: lcpwire.CallEnvelope{
			ProtocolVersion: lcpwire.ProtocolVersionV03,
			CallID:          jobID,
			MsgID:           lcpwire.MsgID{},
			Expiry:          expiry,
		},
		StreamID: resultStreamID,
		TotalLen: resultLen,
		SHA256:   resultHash,
	})
	if err != nil {
		t.Fatalf("EncodeStreamEnd: %v", err)
	}

	terminalPayload, err := lcpwire.EncodeComplete(lcpwire.Complete{
		Envelope: lcpwire.CallEnvelope{
			ProtocolVersion: lcpwire.ProtocolVersionV03,
			CallID:          jobID,
			MsgID:           lcpwire.MsgID{},
			Expiry:          expiry,
		},
		Status: lcpwire.CompleteStatusOK,
		OK: &lcpwire.CompleteOK{
			ResponseStreamID:        resultStreamID,
			ResponseHash:            resultHash,
			ResponseLen:             resultLen,
			ResponseContentType:     resultContentType,
			ResponseContentEncoding: resultContentEncoding,
		},
	})
	if err != nil {
		t.Fatalf("EncodeComplete: %v", err)
	}

	svc := mustNewService(t, Params{
		Clock:         clock,
		JobStore:      jobs,
		Waiter:        waiter,
		LightningRPC:  ln,
		PeerDirectory: peerdirectory.New(),
	})

	stream := &recordingStreamServer{ctx: context.Background()}
	errCh := make(chan error, 1)
	go func() {
		errCh <- svc.AcceptAndExecuteStream(
			&lcpdv1.AcceptAndExecuteStreamRequest{
				PeerId:     peerID,
				CallId:     append([]byte(nil), jobID[:]...),
				PayInvoice: true,
			},
			stream,
		)
	}()

	select {
	case <-payCalled:
	case <-time.After(time.Second):
		t.Fatalf("PayInvoice was not called")
	}

	waiter.HandleInboundCustomMessage(
		context.Background(),
		inbound(peerID, lcpwire.MessageTypeStreamBegin, beginPayload),
	)
	waiter.HandleInboundCustomMessage(
		context.Background(),
		inbound(peerID, lcpwire.MessageTypeStreamChunk, chunkPayload),
	)
	waiter.HandleInboundCustomMessage(
		context.Background(),
		inbound(peerID, lcpwire.MessageTypeStreamEnd, endPayload),
	)
	waiter.HandleInboundCustomMessage(
		context.Background(),
		inbound(peerID, lcpwire.MessageTypeComplete, terminalPayload),
	)

	select {
	case streamErr := <-errCh:
		if streamErr != nil {
			t.Fatalf("AcceptAndExecuteStream: %v", streamErr)
		}
	case <-time.After(time.Second):
		t.Fatalf("AcceptAndExecuteStream did not return")
	}

	if got, want := len(stream.sent), 4; got != want {
		t.Fatalf("sent stream response count mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}

	begin := stream.sent[0].GetResponseBegin()
	if begin == nil {
		t.Fatalf("expected first event to be response_begin")
	}
	if begin.GetContentType() != resultContentType || begin.GetContentEncoding() != resultContentEncoding {
		t.Fatalf(
			"response_begin content type/encoding mismatch (-want +got):\n%s",
			cmp.Diff(
				resultContentType+"/"+resultContentEncoding,
				begin.GetContentType()+"/"+begin.GetContentEncoding(),
			),
		)
	}

	chunk := stream.sent[1].GetResponseChunk()
	if chunk == nil {
		t.Fatalf("expected second event to be response_chunk")
	}
	if diff := cmp.Diff(resultBytes, chunk.GetData()); diff != "" {
		t.Fatalf("chunk data mismatch (-want +got):\n%s", diff)
	}

	end := stream.sent[2].GetResponseEnd()
	if end == nil {
		t.Fatalf("expected third event to be response_end")
	}
	if end.GetResponseLen() != resultLen {
		t.Fatalf("response_end len mismatch (-want +got):\n%s", cmp.Diff(resultLen, end.GetResponseLen()))
	}
	if diff := cmp.Diff(resultHash[:], end.GetResponseHash()); diff != "" {
		t.Fatalf("response_end hash mismatch (-want +got):\n%s", diff)
	}

	result := stream.sent[3].GetComplete()
	if result == nil {
		t.Fatalf("expected terminal complete event")
	}
	if got, want := result.GetStatus(), lcpdv1.Complete_STATUS_OK; got != want {
		t.Fatalf("complete status mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
	if diff := cmp.Diff(resultHash[:], result.GetResponseHash()); diff != "" {
		t.Fatalf("complete response_hash mismatch (-want +got):\n%s", diff)
	}
	if got := result.GetResponseLen(); got != resultLen {
		t.Fatalf("complete response_len mismatch (-want +got):\n%s", cmp.Diff(resultLen, got))
	}
	if got := result.GetResponseContentType(); got != resultContentType {
		t.Fatalf("complete response_content_type mismatch (-want +got):\n%s", cmp.Diff(resultContentType, got))
	}
	if got := result.GetResponseContentEncoding(); got != resultContentEncoding {
		t.Fatalf("complete response_content_encoding mismatch (-want +got):\n%s", cmp.Diff(resultContentEncoding, got))
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
	requestJSON := openaiChatCompletionsRequestJSON("hello", "model-1")
	paramsBytes, err := lcpwire.EncodeOpenAIChatCompletionsV1Params(
		lcpwire.OpenAIChatCompletionsV1Params{Model: "model-1"},
	)
	if err != nil {
		t.Fatalf("EncodeOpenAIChatCompletionsV1Params: %v", err)
	}
	termsHash, err := protocolcompat.ComputeTermsHash(lcp.Terms{
		ProtocolVersion: lcpwire.ProtocolVersionV03,
		JobID:           jobID,
		PriceMsat:       123,
		QuoteExpiry:     quoteExpiry,
	}, protocolcompat.TermsCommit{
		Method:                 "openai.chat_completions.v1",
		Request:                requestJSON,
		RequestContentType:     requestContentTypeJSON,
		RequestContentEncoding: contentEncodingIdentity,
		Params:                 paramsBytes,
	})
	if err != nil {
		t.Fatalf("ComputeTermsHash: %v", err)
	}

	quote := &lcpdv1.Quote{
		ProtocolVersion: uint32(lcpwire.ProtocolVersionV03),
		CallId:          append([]byte(nil), jobID[:]...),
		PriceMsat:       123,
		QuoteExpiry:     timestamppb.New(time.Unix(int64(quoteExpiry), 0)),
		TermsHash:       append([]byte(nil), termsHash[:]...),
		PaymentRequest:  "lnbc1dummy",
	}
	if putErr := jobs.PutQuote(peerID, openaiChatCall(t, "hello", "model-1"), quote); putErr != nil {
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
		Waiter:        requesterwait.New(nil, nil),
		LightningRPC:  ln,
		PeerDirectory: peerdirectory.New(),
	})

	_, err = svc.AcceptAndExecute(context.Background(), &lcpdv1.AcceptAndExecuteRequest{
		PeerId:     peerID,
		CallId:     append([]byte(nil), jobID[:]...),
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
	requestJSON := openaiChatCompletionsRequestJSON("hello", "model-1")
	paramsBytes, err := lcpwire.EncodeOpenAIChatCompletionsV1Params(
		lcpwire.OpenAIChatCompletionsV1Params{Model: "model-1"},
	)
	if err != nil {
		t.Fatalf("EncodeOpenAIChatCompletionsV1Params: %v", err)
	}
	termsHash, err := protocolcompat.ComputeTermsHash(lcp.Terms{
		ProtocolVersion: lcpwire.ProtocolVersionV03,
		JobID:           jobID,
		PriceMsat:       123,
		QuoteExpiry:     quoteExpiry,
	}, protocolcompat.TermsCommit{
		Method:                 "openai.chat_completions.v1",
		Request:                requestJSON,
		RequestContentType:     requestContentTypeJSON,
		RequestContentEncoding: contentEncodingIdentity,
		Params:                 paramsBytes,
	})
	if err != nil {
		t.Fatalf("ComputeTermsHash: %v", err)
	}

	quote := &lcpdv1.Quote{
		ProtocolVersion: uint32(lcpwire.ProtocolVersionV03),
		CallId:          append([]byte(nil), jobID[:]...),
		PriceMsat:       123,
		QuoteExpiry:     timestamppb.New(time.Unix(int64(quoteExpiry), 0)),
		TermsHash:       append([]byte(nil), termsHash[:]...),
		PaymentRequest:  "lnbc1dummy",
	}
	if putErr := jobs.PutQuote(peerID, openaiChatCall(t, "hello", "model-1"), quote); putErr != nil {
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
		Waiter:        requesterwait.New(nil, nil),
		LightningRPC:  ln,
		PeerDirectory: peerdirectory.New(),
	})

	_, err = svc.AcceptAndExecute(context.Background(), &lcpdv1.AcceptAndExecuteRequest{
		PeerId:     peerID,
		CallId:     append([]byte(nil), jobID[:]...),
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
	requestJSON := openaiChatCompletionsRequestJSON("hello", "model-1")
	paramsBytes, err := lcpwire.EncodeOpenAIChatCompletionsV1Params(
		lcpwire.OpenAIChatCompletionsV1Params{Model: "model-1"},
	)
	if err != nil {
		t.Fatalf("EncodeOpenAIChatCompletionsV1Params: %v", err)
	}
	termsHash, err := protocolcompat.ComputeTermsHash(lcp.Terms{
		ProtocolVersion: lcpwire.ProtocolVersionV03,
		JobID:           jobID,
		PriceMsat:       123,
		QuoteExpiry:     quoteExpiry,
	}, protocolcompat.TermsCommit{
		Method:                 "openai.chat_completions.v1",
		Request:                requestJSON,
		RequestContentType:     requestContentTypeJSON,
		RequestContentEncoding: contentEncodingIdentity,
		Params:                 paramsBytes,
	})
	if err != nil {
		t.Fatalf("ComputeTermsHash: %v", err)
	}

	quote := &lcpdv1.Quote{
		ProtocolVersion: uint32(lcpwire.ProtocolVersionV03),
		CallId:          append([]byte(nil), jobID[:]...),
		PriceMsat:       123,
		QuoteExpiry:     timestamppb.New(time.Unix(int64(quoteExpiry), 0)),
		TermsHash:       append([]byte(nil), termsHash[:]...),
		PaymentRequest:  "lnbc1dummy",
	}
	if putErr := jobs.PutQuote(peerID, openaiChatCall(t, "hello", "model-1"), quote); putErr != nil {
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
		Waiter:        requesterwait.New(nil, nil),
		LightningRPC:  ln,
		PeerDirectory: peerdirectory.New(),
	})

	_, err = svc.AcceptAndExecute(context.Background(), &lcpdv1.AcceptAndExecuteRequest{
		PeerId:     peerID,
		CallId:     append([]byte(nil), jobID[:]...),
		PayInvoice: true,
	})
	assertStatusCode(t, err, codes.FailedPrecondition)
}

func TestAcceptAndExecute_RejectsPayInvoiceFalse(t *testing.T) {
	t.Parallel()

	svc := mustNewService(t, Params{
		PeerDirectory: peerdirectory.New(),
		JobStore:      requesterjobstore.New(),
		Waiter:        requesterwait.New(nil, nil),
		LightningRPC:  &fakeLightning{},
	})

	_, err := svc.AcceptAndExecute(context.Background(), &lcpdv1.AcceptAndExecuteRequest{
		PeerId:     "02" + strings.Repeat("f", 64),
		CallId:     make([]byte, 32),
		PayInvoice: false,
	})
	assertStatusCode(t, err, codes.InvalidArgument)
}

func TestAcceptAndExecute_JobNotFound(t *testing.T) {
	t.Parallel()

	svc := mustNewService(t, Params{
		PeerDirectory: peerdirectory.New(),
		JobStore:      requesterjobstore.New(),
		Waiter:        requesterwait.New(nil, nil),
		LightningRPC:  &fakeLightning{},
	})

	_, err := svc.AcceptAndExecute(context.Background(), &lcpdv1.AcceptAndExecuteRequest{
		PeerId:     "02" + strings.Repeat("f", 64),
		CallId:     make([]byte, 32),
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
		Waiter:        requesterwait.New(nil, nil),
	})

	resp, err := svc.CancelJob(context.Background(), &lcpdv1.CancelJobRequest{
		PeerId: peerID,
		CallId: append([]byte(nil), jobID[:]...),
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

func openaiChatCompletionsRequestJSON(prompt, model string) []byte {
	return fmt.Appendf(
		nil,
		`{"model":%q,"messages":[{"role":"user","content":%q}]}`,
		model,
		prompt,
	)
}

func openaiChatCall(t *testing.T, prompt, model string) *lcpdv1.CallSpec {
	t.Helper()

	paramsBytes, err := lcpwire.EncodeOpenAIChatCompletionsV1Params(
		lcpwire.OpenAIChatCompletionsV1Params{Model: model},
	)
	if err != nil {
		t.Fatalf("EncodeOpenAIChatCompletionsV1Params: %v", err)
	}

	requestJSON := openaiChatCompletionsRequestJSON(prompt, model)

	return &lcpdv1.CallSpec{
		Method:                 "openai.chat_completions.v1",
		Params:                 paramsBytes,
		RequestBytes:           requestJSON,
		RequestContentType:     requestContentTypeJSON,
		RequestContentEncoding: contentEncodingIdentity,
	}
}

func newQuoteRespondingFakeMessenger(
	peerID string,
	clock fakeClock,
	waiter *requesterwait.Waiter,
	inputBytes []byte,
	priceMsat uint64,
	paymentRequest string,
) *fakeMessenger {
	return &fakeMessenger{
		sendFn: func(_ context.Context, msg sentMsg) error {
			switch msg.msgType {
			case lcpwire.MessageTypeCall:
				return respondToCall(
					peerID,
					clock,
					waiter,
					msg.payload,
					inputBytes,
					priceMsat,
					paymentRequest,
				)
			case lcpwire.MessageTypeStreamBegin,
				lcpwire.MessageTypeStreamChunk,
				lcpwire.MessageTypeStreamEnd:
				// Ignore the input stream; this fake provider is only validating
				// call/quote hashing for the test.
				return nil
			case lcpwire.MessageTypeManifest,
				lcpwire.MessageTypeQuote,
				lcpwire.MessageTypeComplete,
				lcpwire.MessageTypeCancel,
				lcpwire.MessageTypeError:
				return errors.New("unexpected outbound message type")
			default:
				return errors.New("unexpected outbound message type")
			}
		},
	}
}

func respondToCall(
	peerID string,
	clock fakeClock,
	waiter *requesterwait.Waiter,
	payload []byte,
	inputBytes []byte,
	priceMsat uint64,
	paymentRequest string,
) error {
	req, err := lcpwire.DecodeCall(payload)
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
		JobID:           req.Envelope.CallID,
		PriceMsat:       priceMsat,
		QuoteExpiry:     quoteExpiry,
	}, protocolcompat.TermsCommit{
		Method:                 req.Method,
		Request:                inputBytes,
		RequestContentType:     requestContentTypeJSON,
		RequestContentEncoding: contentEncodingIdentity,
		Params:                 paramsBytes,
	})
	if err != nil {
		return err
	}

	respPayload, err := lcpwire.EncodeQuote(lcpwire.Quote{
		Envelope: lcpwire.CallEnvelope{
			ProtocolVersion: req.Envelope.ProtocolVersion,
			CallID:          req.Envelope.CallID,
			MsgID:           lcpwire.MsgID{},
			Expiry:          req.Envelope.Expiry,
		},
		PriceMsat:      priceMsat,
		QuoteExpiry:    quoteExpiry,
		TermsHash:      termsHash,
		PaymentRequest: paymentRequest,
	})
	if err != nil {
		return err
	}

	waiter.HandleInboundCustomMessage(
		context.Background(),
		inbound(peerID, lcpwire.MessageTypeQuote, respPayload),
	)
	return nil
}

func assertRequestQuoteResponse(
	t *testing.T,
	resp *lcpdv1.RequestQuoteResponse,
	peerID string,
	wantPriceMsat uint64,
	wantPaymentRequest string,
) {
	t.Helper()

	if got, want := resp.GetPeerId(), peerID; got != want {
		t.Fatalf("peer_id mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}

	quote := resp.GetQuote()
	if quote == nil {
		t.Fatalf("RequestQuote: quote is nil")
	}
	if got, want := quote.GetPriceMsat(), wantPriceMsat; got != want {
		t.Fatalf("price_msat mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
	if got, want := quote.GetPaymentRequest(), wantPaymentRequest; got != want {
		t.Fatalf("payment_request mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
}

func assertQuoteStored(
	t *testing.T,
	jobs *requesterjobstore.Store,
	peerID string,
	quote *lcpdv1.Quote,
	wantPaymentRequest string,
) {
	t.Helper()

	if quote == nil {
		t.Fatalf("quote is nil")
	}
	storedJobID, err := toJobID(quote.GetCallId())
	if err != nil {
		t.Fatalf("toJobID: %v", err)
	}
	storedQuote, err := jobs.GetQuote(peerID, storedJobID)
	if err != nil {
		t.Fatalf("jobs.GetQuote: %v", err)
	}
	if got, want := storedQuote.GetPaymentRequest(), wantPaymentRequest; got != want {
		t.Fatalf("stored payment_request mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
}

func assertSentQuoteRequestAndInputStream(t *testing.T, msgs []sentMsg, inputBytes []byte) {
	t.Helper()

	wantTypes := []lcpwire.MessageType{
		lcpwire.MessageTypeCall,
		lcpwire.MessageTypeStreamBegin,
		lcpwire.MessageTypeStreamChunk,
		lcpwire.MessageTypeStreamEnd,
	}
	if got, want := len(msgs), len(wantTypes); got != want {
		t.Fatalf("sent message count mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
	for i, wantType := range wantTypes {
		if got := msgs[i].msgType; got != wantType {
			t.Fatalf("msg[%d] type mismatch (-want +got):\n%s", i, cmp.Diff(wantType, got))
		}
	}

	wireReq := mustDecodeCall(t, msgs[0].payload)
	begin := mustDecodeStreamBegin(t, msgs[1].payload)
	if got, want := begin.Kind, lcpwire.StreamKindRequest; got != want {
		t.Fatalf("stream_begin.kind mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}

	inputSum := sha256.Sum256(inputBytes)
	wantInputHash := lcp.Hash32(inputSum)
	wantInputLen := uint64(len(inputBytes))

	if got, want := begin.Envelope.CallID, wireReq.Envelope.CallID; got != want {
		t.Fatalf("stream_begin.job_id mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
	if begin.TotalLen == nil || *begin.TotalLen != wantInputLen {
		t.Fatalf("stream_begin.total_len mismatch (-want +got):\n%s", cmp.Diff(wantInputLen, begin.TotalLen))
	}
	if begin.SHA256 == nil || *begin.SHA256 != wantInputHash {
		t.Fatalf("stream_begin.sha256 mismatch (-want +got):\n%s", cmp.Diff(wantInputHash, begin.SHA256))
	}
	if got, want := begin.ContentType, requestContentTypeJSON; got != want {
		t.Fatalf("stream_begin.content_type mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
	if got, want := begin.ContentEncoding, contentEncodingIdentity; got != want {
		t.Fatalf("stream_begin.content_encoding mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}

	chunk := mustDecodeStreamChunk(t, msgs[2].payload)
	if got, want := chunk.Envelope.CallID, wireReq.Envelope.CallID; got != want {
		t.Fatalf("stream_chunk.job_id mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
	if got, want := chunk.StreamID, begin.StreamID; got != want {
		t.Fatalf("stream_chunk.stream_id mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
	if got, want := chunk.Seq, uint32(0); got != want {
		t.Fatalf("stream_chunk.seq mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
	if diff := cmp.Diff(inputBytes, chunk.Data); diff != "" {
		t.Fatalf("stream_chunk.data mismatch (-want +got):\n%s", diff)
	}

	end := mustDecodeStreamEnd(t, msgs[3].payload)
	if got, want := end.Envelope.CallID, wireReq.Envelope.CallID; got != want {
		t.Fatalf("stream_end.job_id mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
	if got, want := end.StreamID, begin.StreamID; got != want {
		t.Fatalf("stream_end.stream_id mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
	if got, want := end.TotalLen, wantInputLen; got != want {
		t.Fatalf("stream_end.total_len mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
	if got, want := end.SHA256, wantInputHash; got != want {
		t.Fatalf("stream_end.sha256 mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
}

func mustDecodeCall(t *testing.T, payload []byte) lcpwire.Call {
	t.Helper()

	req, err := lcpwire.DecodeCall(payload)
	if err != nil {
		t.Fatalf("DecodeCall: %v", err)
	}
	return req
}

func mustDecodeStreamBegin(t *testing.T, payload []byte) lcpwire.StreamBegin {
	t.Helper()

	begin, err := lcpwire.DecodeStreamBegin(payload)
	if err != nil {
		t.Fatalf("DecodeStreamBegin: %v", err)
	}
	return begin
}

func mustDecodeStreamChunk(t *testing.T, payload []byte) lcpwire.StreamChunk {
	t.Helper()

	chunk, err := lcpwire.DecodeStreamChunk(payload)
	if err != nil {
		t.Fatalf("DecodeStreamChunk: %v", err)
	}
	return chunk
}

func mustDecodeStreamEnd(t *testing.T, payload []byte) lcpwire.StreamEnd {
	t.Helper()

	end, err := lcpwire.DecodeStreamEnd(payload)
	if err != nil {
		t.Fatalf("DecodeStreamEnd: %v", err)
	}
	return end
}

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
