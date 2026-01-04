package lcpmsgrouter_test

import (
	"testing"

	"github.com/bruwbird/lcp/go-lcpd/internal/lcpmsgrouter"
	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
	"github.com/google/go-cmp/cmp"
)

func TestRouter_Route_KnownMessages(t *testing.T) {
	t.Parallel()

	router := lcpmsgrouter.New()

	tests := []struct {
		name    string
		msgType lcpwire.MessageType
		want    lcpmsgrouter.RouteDecision
	}{
		{
			name:    "manifest",
			msgType: lcpwire.MessageTypeManifest,
			want: lcpmsgrouter.RouteDecision{
				Action: lcpmsgrouter.RouteActionDispatchManifest,
				Reason: "lcp_manifest",
			},
		},
		{
			name:    "quote_request",
			msgType: lcpwire.MessageTypeQuoteRequest,
			want: lcpmsgrouter.RouteDecision{
				Action: lcpmsgrouter.RouteActionDispatchQuoteRequest,
				Reason: "lcp_quote_request",
			},
		},
		{
			name:    "quote_response",
			msgType: lcpwire.MessageTypeQuoteResponse,
			want: lcpmsgrouter.RouteDecision{
				Action: lcpmsgrouter.RouteActionDispatchQuoteResponse,
				Reason: "lcp_quote_response",
			},
		},
		{
			name:    "stream_begin",
			msgType: lcpwire.MessageTypeStreamBegin,
			want: lcpmsgrouter.RouteDecision{
				Action: lcpmsgrouter.RouteActionDispatchStreamBegin,
				Reason: "lcp_stream_begin",
			},
		},
		{
			name:    "stream_chunk",
			msgType: lcpwire.MessageTypeStreamChunk,
			want: lcpmsgrouter.RouteDecision{
				Action: lcpmsgrouter.RouteActionDispatchStreamChunk,
				Reason: "lcp_stream_chunk",
			},
		},
		{
			name:    "stream_end",
			msgType: lcpwire.MessageTypeStreamEnd,
			want: lcpmsgrouter.RouteDecision{
				Action: lcpmsgrouter.RouteActionDispatchStreamEnd,
				Reason: "lcp_stream_end",
			},
		},
		{
			name:    "result",
			msgType: lcpwire.MessageTypeResult,
			want: lcpmsgrouter.RouteDecision{
				Action: lcpmsgrouter.RouteActionDispatchResult,
				Reason: "lcp_result",
			},
		},
		{
			name:    "cancel",
			msgType: lcpwire.MessageTypeCancel,
			want: lcpmsgrouter.RouteDecision{
				Action: lcpmsgrouter.RouteActionDispatchCancel,
				Reason: "lcp_cancel",
			},
		},
		{
			name:    "error",
			msgType: lcpwire.MessageTypeError,
			want: lcpmsgrouter.RouteDecision{
				Action: lcpmsgrouter.RouteActionDispatchError,
				Reason: "lcp_error",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := router.Route(lcpmsgrouter.CustomMessage{
				PeerPubKey: "peer",
				MsgType:    uint16(tt.msgType),
				Payload:    []byte("payload"),
			})
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Fatalf("Route mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestRouter_Route_UnknownEvenDisconnects(t *testing.T) {
	t.Parallel()

	router := lcpmsgrouter.New()

	got := router.Route(lcpmsgrouter.CustomMessage{
		PeerPubKey: "peer",
		MsgType:    uint16(lcpwire.MessageTypeManifest + 1), // even
		Payload:    []byte("payload"),
	})

	want := lcpmsgrouter.RouteDecision{
		Action: lcpmsgrouter.RouteActionDisconnect,
		Reason: "unknown_even_msg_type",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Route mismatch (-want +got):\n%s", diff)
	}
}

func TestRouter_Route_UnknownOddIgnores(t *testing.T) {
	t.Parallel()

	router := lcpmsgrouter.New()

	got := router.Route(lcpmsgrouter.CustomMessage{
		PeerPubKey: "peer",
		MsgType:    uint16(lcpwire.MessageTypeError + 2), // odd unknown
		Payload:    []byte("payload"),
	})

	want := lcpmsgrouter.RouteDecision{
		Action: lcpmsgrouter.RouteActionIgnore,
		Reason: "unknown_odd_msg_type",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Route mismatch (-want +got):\n%s", diff)
	}
}
