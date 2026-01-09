package lightningnode

import (
	"context"
	"errors"

	"github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"
	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
)

var (
	// ErrNotConfigured indicates that a Lightning node backend was not configured.
	ErrNotConfigured = errors.New("lightning node not configured")

	// ErrNotImplemented indicates that a Lightning node backend does not implement
	// a required capability yet.
	ErrNotImplemented = errors.New("lightning node backend not implemented")

	// ErrPaymentFailed indicates that a payment attempt completed but failed.
	ErrPaymentFailed = errors.New("payment failed")

	// ErrInvalidRequest indicates that the caller supplied an invalid request.
	ErrInvalidRequest = errors.New("invalid request")
)

type NodeInfo struct {
	IdentityPubKey string
}

type Peer struct {
	PubKey  string
	Address string
}

type PeerEventType uint8

const (
	PeerEventTypeUnspecified PeerEventType = 0
	PeerEventTypeOnline      PeerEventType = 1
	PeerEventTypeOffline     PeerEventType = 2
)

type PeerEvent struct {
	PubKey string
	Type   PeerEventType
}

type PeerEventStream interface {
	Recv() (*PeerEvent, error)
}

type CustomMessage struct {
	PeerPubKey string
	MsgType    uint32
	Payload    []byte
}

type CustomMessageStream interface {
	Recv() (*CustomMessage, error)
}

type InvoiceSettlementState uint8

const (
	InvoiceSettlementStateUnspecified InvoiceSettlementState = 0
	InvoiceSettlementStateSettled     InvoiceSettlementState = 1
	InvoiceSettlementStateCanceled    InvoiceSettlementState = 2
	InvoiceSettlementStateExpired     InvoiceSettlementState = 3
)

type CreateInvoiceRequest struct {
	DescriptionHash lcp.Hash32
	AmountMsat      uint64
	ExpirySeconds   uint64
}

type CreateInvoiceResponse struct {
	PaymentRequest string
	PaymentHash    lcp.Hash32
}

type DecodedInvoice struct {
	PayeePubKey     string
	DescriptionHash lcp.Hash32
	AmountMsat      int64
	TimestampUnix   int64
	ExpirySeconds   int64
}

// PeerMessenger sends raw BOLT #1 custom messages to a peer.
type PeerMessenger interface {
	SendCustomMessage(
		ctx context.Context,
		peerPubKey string,
		msgType lcpwire.MessageType,
		payload []byte,
	) error
}

// PeerSubscriber exposes peer connectivity and inbound custom messages.
type PeerSubscriber interface {
	ListPeers(ctx context.Context) ([]Peer, error)
	SubscribePeerEvents(ctx context.Context) (PeerEventStream, error)
	SubscribeCustomMessages(ctx context.Context) (CustomMessageStream, error)
	DisconnectPeer(ctx context.Context, peerPubKey string) error
}

// Requester provides the Lightning operations used by go-lcpd when acting as
// an LCP requester (invoice decoding + payment).
type Requester interface {
	GetNodeInfo(ctx context.Context) (NodeInfo, error)
	DecodeInvoice(ctx context.Context, paymentRequest string) (DecodedInvoice, error)
	PayInvoice(ctx context.Context, paymentRequest string) (lcp.Hash32, error)
}

// Invoicer provides the Lightning operations used by go-lcpd when acting as an
// LCP provider (invoice creation + settlement detection).
type Invoicer interface {
	CreateInvoice(ctx context.Context, req CreateInvoiceRequest) (CreateInvoiceResponse, error)
	WaitInvoiceSettled(ctx context.Context, paymentHash lcp.Hash32) (InvoiceSettlementState, error)
}

// Backend provides the minimal Lightning node operations required by go-lcpd.
//
// Backends may be swapped (e.g. lnd gRPC, ldk gRPC) as long as they preserve
// the semantics expected by go-lcpd:
// - Custom messages are raw bytes (no LCP parsing in the backend).
// - BOLT11 invoices can be decoded, paid, created, and awaited for settlement.
type Backend interface {
	Close() error

	PeerMessenger
	PeerSubscriber
	Requester
	Invoicer
}
