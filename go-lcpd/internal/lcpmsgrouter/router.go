package lcpmsgrouter

import "github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"

type RouteAction string

const (
	RouteActionDispatchManifest      RouteAction = "dispatch_manifest"
	RouteActionDispatchQuoteRequest  RouteAction = "dispatch_quote_request"
	RouteActionDispatchQuoteResponse RouteAction = "dispatch_quote_response"
	RouteActionDispatchStreamBegin   RouteAction = "dispatch_stream_begin"
	RouteActionDispatchStreamChunk   RouteAction = "dispatch_stream_chunk"
	RouteActionDispatchStreamEnd     RouteAction = "dispatch_stream_end"
	RouteActionDispatchResult        RouteAction = "dispatch_result"
	RouteActionDispatchCancel        RouteAction = "dispatch_cancel"
	RouteActionDispatchError         RouteAction = "dispatch_error"
	RouteActionIgnore                RouteAction = "ignore"
	RouteActionDisconnect            RouteAction = "disconnect"
)

type RouteDecision struct {
	Action RouteAction
	Reason string
}

type CustomMessage struct {
	PeerPubKey string
	MsgType    uint16
	Payload    []byte
}

type Router interface {
	Route(CustomMessage) RouteDecision
}

type router struct{}

func New() Router {
	return router{}
}

func (router) Route(msg CustomMessage) RouteDecision {
	switch lcpwire.MessageType(msg.MsgType) {
	case lcpwire.MessageTypeManifest:
		return RouteDecision{
			Action: RouteActionDispatchManifest,
			Reason: "lcp_manifest",
		}
	case lcpwire.MessageTypeQuoteRequest:
		return RouteDecision{
			Action: RouteActionDispatchQuoteRequest,
			Reason: "lcp_quote_request",
		}
	case lcpwire.MessageTypeQuoteResponse:
		return RouteDecision{
			Action: RouteActionDispatchQuoteResponse,
			Reason: "lcp_quote_response",
		}
	case lcpwire.MessageTypeStreamBegin:
		return RouteDecision{
			Action: RouteActionDispatchStreamBegin,
			Reason: "lcp_stream_begin",
		}
	case lcpwire.MessageTypeStreamChunk:
		return RouteDecision{
			Action: RouteActionDispatchStreamChunk,
			Reason: "lcp_stream_chunk",
		}
	case lcpwire.MessageTypeStreamEnd:
		return RouteDecision{
			Action: RouteActionDispatchStreamEnd,
			Reason: "lcp_stream_end",
		}
	case lcpwire.MessageTypeResult:
		return RouteDecision{
			Action: RouteActionDispatchResult,
			Reason: "lcp_result",
		}
	case lcpwire.MessageTypeCancel:
		return RouteDecision{
			Action: RouteActionDispatchCancel,
			Reason: "lcp_cancel",
		}
	case lcpwire.MessageTypeError:
		return RouteDecision{
			Action: RouteActionDispatchError,
			Reason: "lcp_error",
		}
	}

	if msg.MsgType%2 == 0 {
		return RouteDecision{
			Action: RouteActionDisconnect,
			Reason: "unknown_even_msg_type",
		}
	}

	return RouteDecision{
		Action: RouteActionIgnore,
		Reason: "unknown_odd_msg_type",
	}
}

var _ Router = router{}
