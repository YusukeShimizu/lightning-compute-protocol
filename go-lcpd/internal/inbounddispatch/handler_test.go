//nolint:testpackage // Tests assert dispatch wiring via unexported Handler fields.
package inbounddispatch

import (
	"context"
	"sync"
	"testing"

	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
	"github.com/bruwbird/lcp/go-lcpd/internal/peermsg"
	"github.com/google/go-cmp/cmp"
)

type recordingHandler struct {
	mu   sync.Mutex
	msgs []peermsg.InboundCustomMessage
}

func (r *recordingHandler) HandleInboundCustomMessage(
	_ context.Context,
	msg peermsg.InboundCustomMessage,
) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, msg)
}

func (r *recordingHandler) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.msgs)
}

func TestHandler_DispatchesPerSpec(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		msgType       uint16
		wantProvider  int
		wantRequester int
	}{
		{
			name:          "quote_request -> provider",
			msgType:       uint16(lcpwire.MessageTypeQuoteRequest),
			wantProvider:  1,
			wantRequester: 0,
		},
		{
			name:          "cancel -> provider",
			msgType:       uint16(lcpwire.MessageTypeCancel),
			wantProvider:  1,
			wantRequester: 0,
		},
		{
			name:          "quote_response -> requester",
			msgType:       uint16(lcpwire.MessageTypeQuoteResponse),
			wantProvider:  0,
			wantRequester: 1,
		},
		{
			name:          "result -> requester",
			msgType:       uint16(lcpwire.MessageTypeResult),
			wantProvider:  0,
			wantRequester: 1,
		},
		{
			name:          "error -> requester",
			msgType:       uint16(lcpwire.MessageTypeError),
			wantProvider:  0,
			wantRequester: 1,
		},
		{
			name:          "manifest -> nobody (handled upstream)",
			msgType:       uint16(lcpwire.MessageTypeManifest),
			wantProvider:  0,
			wantRequester: 0,
		},
		{
			name:          "unknown odd -> nobody",
			msgType:       uint16(lcpwire.MessageTypeError + 2),
			wantProvider:  0,
			wantRequester: 0,
		},
		{
			name:          "unknown even -> nobody (disconnect handled upstream)",
			msgType:       uint16(lcpwire.MessageTypeManifest + 1),
			wantProvider:  0,
			wantRequester: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			provider := &recordingHandler{}
			requester := &recordingHandler{}
			h := &Handler{
				provider:  provider,
				requester: requester,
			}

			h.HandleInboundCustomMessage(context.Background(), peermsg.InboundCustomMessage{
				PeerPubKey: "peer",
				MsgType:    tc.msgType,
				Payload:    []byte("payload"),
			})

			if diff := cmp.Diff(tc.wantProvider, provider.count()); diff != "" {
				t.Fatalf("provider call count mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantRequester, requester.count()); diff != "" {
				t.Fatalf("requester call count mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
