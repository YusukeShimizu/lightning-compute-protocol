package requesterwait_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"
	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
	"github.com/bruwbird/lcp/go-lcpd/internal/lndpeermsg"
	"github.com/bruwbird/lcp/go-lcpd/internal/requesterwait"
	"github.com/google/go-cmp/cmp"
)

func TestWaitQuoteResponse_DeliversQuote(t *testing.T) {
	t.Parallel()

	waiter := requesterwait.New(nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	jobID := newJobID(0x11)
	msgID := newMsgID(0x22)
	quote := lcpwire.Quote{
		Envelope: lcpwire.CallEnvelope{
			ProtocolVersion: lcpwire.ProtocolVersionV02,
			CallID:          jobID,
			MsgID:           msgID,
			Expiry:          123,
		},
		PriceMsat:      1000,
		QuoteExpiry:    9999,
		TermsHash:      newHash(0xaa),
		PaymentRequest: "bolt11",
	}
	payload, err := lcpwire.EncodeQuote(quote)
	if err != nil {
		t.Fatalf("encode quote response: %v", err)
	}

	done := make(chan requesterwait.QuoteOutcome, 1)
	go func() {
		out, waitErr := waiter.WaitQuoteResponse(ctx, "peer-a", jobID)
		if waitErr != nil {
			t.Errorf("WaitQuoteResponse error: %v", waitErr)
			return
		}
		done <- out
	}()

	waiter.HandleInboundCustomMessage(context.Background(), lndpeermsg.InboundCustomMessage{
		PeerPubKey: "peer-a",
		MsgType:    uint16(lcpwire.MessageTypeQuote),
		Payload:    payload,
	})

	select {
	case out := <-done:
		if out.Quote == nil {
			t.Fatalf("expected QuoteResponse, got nil")
		}
		if diff := cmp.Diff(quote, *out.Quote); diff != "" {
			t.Fatalf("QuoteResponse mismatch (-want +got):\n%s", diff)
		}
	case <-ctx.Done():
		t.Fatalf("wait timed out: %v", ctx.Err())
	}
}

func TestWaitQuoteResponse_DeliversError(t *testing.T) {
	t.Parallel()

	waiter := requesterwait.New(nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	jobID := newJobID(0x33)
	msgID := newMsgID(0x44)
	message := "oops"
	errMsg := lcpwire.Error{
		Envelope: lcpwire.CallEnvelope{
			ProtocolVersion: lcpwire.ProtocolVersionV02,
			CallID:          jobID,
			MsgID:           msgID,
			Expiry:          456,
		},
		Code:    lcpwire.ErrorCodeUnsupportedMethod,
		Message: &message,
	}
	payload, err := lcpwire.EncodeError(errMsg)
	if err != nil {
		t.Fatalf("encode error: %v", err)
	}

	done := make(chan requesterwait.QuoteOutcome, 1)
	go func() {
		out, waitErr := waiter.WaitQuoteResponse(ctx, "peer-b", jobID)
		if waitErr != nil {
			t.Errorf("WaitQuoteResponse error: %v", waitErr)
			return
		}
		done <- out
	}()

	waiter.HandleInboundCustomMessage(context.Background(), lndpeermsg.InboundCustomMessage{
		PeerPubKey: "peer-b",
		MsgType:    uint16(lcpwire.MessageTypeError),
		Payload:    payload,
	})

	select {
	case out := <-done:
		if out.Error == nil {
			t.Fatalf("expected error outcome, got nil")
		}
		if diff := cmp.Diff(errMsg, *out.Error); diff != "" {
			t.Fatalf("Error mismatch (-want +got):\n%s", diff)
		}
	case <-ctx.Done():
		t.Fatalf("wait timed out: %v", ctx.Err())
	}
}

func TestWaitResult_ReceivesPendingFailedResult(t *testing.T) {
	t.Parallel()

	waiter := requesterwait.New(nil, nil)

	jobID := newJobID(0x55)
	msgID := newMsgID(0x66)
	msg := "failed"
	complete := lcpwire.Complete{
		Envelope: lcpwire.CallEnvelope{
			ProtocolVersion: lcpwire.ProtocolVersionV02,
			CallID:          jobID,
			MsgID:           msgID,
			Expiry:          777,
		},
		Status:  lcpwire.CompleteStatusFailed,
		Message: &msg,
	}
	payload, err := lcpwire.EncodeComplete(complete)
	if err != nil {
		t.Fatalf("encode complete: %v", err)
	}

	// Deliver complete before waiting.
	waiter.HandleInboundCustomMessage(context.Background(), lndpeermsg.InboundCustomMessage{
		PeerPubKey: "peer-c",
		MsgType:    uint16(lcpwire.MessageTypeComplete),
		Payload:    payload,
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	out, waitErr := waiter.WaitResult(ctx, "peer-c", jobID)
	if waitErr != nil {
		t.Fatalf("WaitResult: %v", waitErr)
	}
	if out.Complete == nil {
		t.Fatalf("expected complete, got nil")
	}
	if diff := cmp.Diff(complete, *out.Complete); diff != "" {
		t.Fatalf("Result mismatch (-want +got):\n%s", diff)
	}
	if len(out.ResponseBytes) != 0 {
		t.Fatalf("expected empty result_bytes for failed status, got %d bytes", len(out.ResponseBytes))
	}
}

func TestWaitResult_CancelThenRetry(t *testing.T) {
	t.Parallel()

	waiter := requesterwait.New(nil, nil)
	jobID := newJobID(0x77)

	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := waiter.WaitResult(cancelCtx, "peer-d", jobID); !errors.Is(
		err,
		requesterwait.ErrWaitCancelled,
	) {
		t.Fatalf("expected context cancelled error, got %v", err)
	}

	msgID := newMsgID(0x88)
	msg := "cancelled"
	complete := lcpwire.Complete{
		Envelope: lcpwire.CallEnvelope{
			ProtocolVersion: lcpwire.ProtocolVersionV02,
			CallID:          jobID,
			MsgID:           msgID,
			Expiry:          1234,
		},
		Status:  lcpwire.CompleteStatusCancelled,
		Message: &msg,
	}
	payload, err := lcpwire.EncodeComplete(complete)
	if err != nil {
		t.Fatalf("encode complete: %v", err)
	}

	waiter.HandleInboundCustomMessage(context.Background(), lndpeermsg.InboundCustomMessage{
		PeerPubKey: "peer-d",
		MsgType:    uint16(lcpwire.MessageTypeComplete),
		Payload:    payload,
	})

	ctx, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()

	out, waitErr := waiter.WaitResult(ctx, "peer-d", jobID)
	if waitErr != nil {
		t.Fatalf("WaitResult retry: %v", waitErr)
	}
	if diff := cmp.Diff(complete, *out.Complete); diff != "" {
		t.Fatalf("Result mismatch (-want +got):\n%s", diff)
	}
}

func TestWaitQuoteResponse_IgnoresDifferentPeer(t *testing.T) {
	t.Parallel()

	waiter := requesterwait.New(nil, nil)
	jobID := newJobID(0x99)
	msgID := newMsgID(0xaa)
	quote := lcpwire.Quote{
		Envelope: lcpwire.CallEnvelope{
			ProtocolVersion: lcpwire.ProtocolVersionV02,
			CallID:          jobID,
			MsgID:           msgID,
			Expiry:          42,
		},
		PriceMsat:      1,
		QuoteExpiry:    2,
		TermsHash:      newHash(0xbb),
		PaymentRequest: "bolt",
	}
	payload, err := lcpwire.EncodeQuote(quote)
	if err != nil {
		t.Fatalf("encode quote response: %v", err)
	}

	waiter.HandleInboundCustomMessage(context.Background(), lndpeermsg.InboundCustomMessage{
		PeerPubKey: "peer-other",
		MsgType:    uint16(lcpwire.MessageTypeQuote),
		Payload:    payload,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, waitErr := waiter.WaitQuoteResponse(ctx, "peer-self", jobID)
	if !errors.Is(waitErr, requesterwait.ErrWaitCancelled) {
		t.Fatalf("expected context cancellation, got %v", waitErr)
	}
}

func newJobID(fill byte) lcp.JobID {
	var id lcp.JobID
	for i := range id {
		id[i] = fill
	}
	return id
}

func newMsgID(fill byte) lcpwire.MsgID {
	var id lcpwire.MsgID
	for i := range id {
		id[i] = fill
	}
	return id
}

func newHash(fill byte) lcp.Hash32 {
	var h lcp.Hash32
	for i := range h {
		h[i] = fill
	}
	return h
}
