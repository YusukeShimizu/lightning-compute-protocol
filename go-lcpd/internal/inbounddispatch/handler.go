package inbounddispatch

import (
	"context"

	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
	"github.com/bruwbird/lcp/go-lcpd/internal/peermsg"
	"github.com/bruwbird/lcp/go-lcpd/internal/provider"
	"github.com/bruwbird/lcp/go-lcpd/internal/requesterwait"
)

type Handler struct {
	provider  peermsg.InboundMessageHandler
	requester peermsg.InboundMessageHandler
}

func New(providerHandler *provider.Handler, requesterWaiter *requesterwait.Waiter) *Handler {
	return &Handler{
		provider:  providerHandler,
		requester: requesterWaiter,
	}
}

func (h *Handler) HandleInboundCustomMessage(
	ctx context.Context,
	msg peermsg.InboundCustomMessage,
) {
	if h == nil {
		return
	}

	// Note: lndpeermsg.PeerMessaging already handles `lcp_manifest` and unknown-even
	// disconnect behavior via its Router. This handler only wires job-scope
	// messages to the correct subsystem per spec.
	switch lcpwire.MessageType(msg.MsgType) {
	case lcpwire.MessageTypeQuoteRequest,
		lcpwire.MessageTypeCancel:
		if h.provider != nil {
			h.provider.HandleInboundCustomMessage(ctx, msg)
		}
	case lcpwire.MessageTypeQuoteResponse,
		lcpwire.MessageTypeResult,
		lcpwire.MessageTypeError:
		if h.requester != nil {
			h.requester.HandleInboundCustomMessage(ctx, msg)
		}
	case lcpwire.MessageTypeStreamBegin,
		lcpwire.MessageTypeStreamChunk,
		lcpwire.MessageTypeStreamEnd:
		// Stream messages are dispatched to both sides. Each subsystem is
		// responsible for ignoring streams that do not match its expected state.
		if h.provider != nil {
			h.provider.HandleInboundCustomMessage(ctx, msg)
		}
		if h.requester != nil {
			h.requester.HandleInboundCustomMessage(ctx, msg)
		}
	case lcpwire.MessageTypeManifest:
		return
	default:
		// Ignore unknown types. Unknown-even disconnection is handled upstream.
		return
	}
}

var _ peermsg.InboundMessageHandler = (*Handler)(nil)
