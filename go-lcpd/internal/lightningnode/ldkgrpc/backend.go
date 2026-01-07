package ldkgrpc

import (
	"context"

	"github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"
	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
	"github.com/bruwbird/lcp/go-lcpd/internal/lightningnode"
)

// Backend is a placeholder lightningnode.Backend for the future ldk-lcp-node gRPC client.
//
// It intentionally returns lightningnode.ErrNotImplemented for all operations.
type Backend struct{}

func New() *Backend { return &Backend{} }

func (*Backend) Close() error { return nil }

func (*Backend) GetNodeInfo(context.Context) (lightningnode.NodeInfo, error) {
	return lightningnode.NodeInfo{}, lightningnode.ErrNotImplemented
}

func (*Backend) ListPeers(context.Context) ([]lightningnode.Peer, error) {
	return nil, lightningnode.ErrNotImplemented
}

func (*Backend) SubscribePeerEvents(context.Context) (lightningnode.PeerEventStream, error) {
	return nil, lightningnode.ErrNotImplemented
}

func (*Backend) SubscribeCustomMessages(
	context.Context,
) (lightningnode.CustomMessageStream, error) {
	return nil, lightningnode.ErrNotImplemented
}

func (*Backend) SendCustomMessage(
	context.Context,
	string,
	lcpwire.MessageType,
	[]byte,
) error {
	return lightningnode.ErrNotImplemented
}

func (*Backend) DisconnectPeer(context.Context, string) error {
	return lightningnode.ErrNotImplemented
}

func (*Backend) CreateInvoice(
	context.Context,
	lightningnode.CreateInvoiceRequest,
) (lightningnode.CreateInvoiceResponse, error) {
	return lightningnode.CreateInvoiceResponse{}, lightningnode.ErrNotImplemented
}

func (*Backend) WaitInvoiceSettled(
	context.Context,
	lcp.Hash32,
) (lightningnode.InvoiceSettlementState, error) {
	return lightningnode.InvoiceSettlementStateUnspecified, lightningnode.ErrNotImplemented
}

func (*Backend) DecodeInvoice(context.Context, string) (lightningnode.DecodedInvoice, error) {
	return lightningnode.DecodedInvoice{}, lightningnode.ErrNotImplemented
}

func (*Backend) PayInvoice(context.Context, string) (lcp.Hash32, error) {
	return lcp.Hash32{}, lightningnode.ErrNotImplemented
}

var _ lightningnode.Backend = (*Backend)(nil)
