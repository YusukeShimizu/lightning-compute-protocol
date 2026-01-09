package lightningnode

import (
	"context"

	"github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"
	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
)

type disabledBackend struct{}

func NewDisabled() Backend {
	return disabledBackend{}
}

func (disabledBackend) Close() error { return nil }

func (disabledBackend) GetNodeInfo(context.Context) (NodeInfo, error) {
	return NodeInfo{}, ErrNotConfigured
}

func (disabledBackend) ListPeers(context.Context) ([]Peer, error) {
	return nil, ErrNotConfigured
}

func (disabledBackend) SubscribePeerEvents(context.Context) (PeerEventStream, error) {
	return nil, ErrNotConfigured
}

func (disabledBackend) SubscribeCustomMessages(context.Context) (CustomMessageStream, error) {
	return nil, ErrNotConfigured
}

func (disabledBackend) SendCustomMessage(
	context.Context,
	string,
	lcpwire.MessageType,
	[]byte,
) error {
	return ErrNotConfigured
}

func (disabledBackend) DisconnectPeer(context.Context, string) error {
	return ErrNotConfigured
}

func (disabledBackend) CreateInvoice(
	context.Context,
	CreateInvoiceRequest,
) (CreateInvoiceResponse, error) {
	return CreateInvoiceResponse{}, ErrNotConfigured
}

func (disabledBackend) WaitInvoiceSettled(
	context.Context,
	lcp.Hash32,
) (InvoiceSettlementState, error) {
	return InvoiceSettlementStateUnspecified, ErrNotConfigured
}

func (disabledBackend) DecodeInvoice(context.Context, string) (DecodedInvoice, error) {
	return DecodedInvoice{}, ErrNotConfigured
}

func (disabledBackend) PayInvoice(context.Context, string) (lcp.Hash32, error) {
	return lcp.Hash32{}, ErrNotConfigured
}
