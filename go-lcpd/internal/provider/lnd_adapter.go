package provider

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"
	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
	"github.com/bruwbird/lcp/go-lcpd/internal/lnd/lnrpc"
	"github.com/bruwbird/lcp/go-lcpd/internal/lndpeermsg"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

const maxInt64 = int64(^uint64(0) >> 1)

type LNDAdapter struct {
	conn   *grpc.ClientConn
	client lnrpc.LightningClient
	logger *zap.SugaredLogger
}

func NewLNDAdapter(
	ctx context.Context,
	cfg *lndpeermsg.Config,
	logger *zap.SugaredLogger,
) (*LNDAdapter, error) {
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	if cfg == nil {
		return &LNDAdapter{logger: logger.With("component", "provider.lnd")}, nil
	}

	conn, err := lndpeermsg.Dial(ctx, *cfg)
	if err != nil {
		return nil, err
	}

	return &LNDAdapter{
		conn:   conn,
		client: lnrpc.NewLightningClient(conn),
		logger: logger.With("component", "provider.lnd"),
	}, nil
}

func (a *LNDAdapter) Close() error {
	if a.conn != nil {
		return a.conn.Close()
	}
	return nil
}

func (a *LNDAdapter) SendCustomMessage(
	ctx context.Context,
	peerPubKey string,
	msgType lcpwire.MessageType,
	payload []byte,
) error {
	if a.client == nil {
		return errors.New("lnd messenger not configured")
	}

	peerBytes, err := hex.DecodeString(peerPubKey)
	if err != nil {
		return fmt.Errorf("decode peer pubkey: %w", err)
	}

	_, err = a.client.SendCustomMessage(ctx, &lnrpc.SendCustomMessageRequest{
		Peer: peerBytes,
		Type: uint32(msgType),
		Data: payload,
	})
	if err != nil {
		return fmt.Errorf("SendCustomMessage: %w", err)
	}
	return nil
}

func (a *LNDAdapter) CreateInvoice(ctx context.Context, req InvoiceRequest) (InvoiceResult, error) {
	if a.client == nil {
		return InvoiceResult{}, errors.New("lnd invoice creator not configured")
	}

	if req.ExpirySeconds > uint64(maxInt64) {
		return InvoiceResult{}, fmt.Errorf("expiry_seconds overflows int64: %d", req.ExpirySeconds)
	}
	expirySeconds := int64(req.ExpirySeconds)

	invoice := &lnrpc.Invoice{
		DescriptionHash: req.DescriptionHash[:],
		Expiry:          expirySeconds,
	}
	if err := setInvoicePriceMsat(invoice, req.PriceMsat); err != nil {
		return InvoiceResult{}, err
	}

	resp, err := a.client.AddInvoice(ctx, invoice)
	if err != nil {
		return InvoiceResult{}, fmt.Errorf("AddInvoice: %w", err)
	}

	var paymentHash lcp.Hash32
	if len(resp.GetRHash()) != len(paymentHash) {
		return InvoiceResult{}, fmt.Errorf("unexpected r_hash length: %d", len(resp.GetRHash()))
	}
	copy(paymentHash[:], resp.GetRHash())

	return InvoiceResult{
		PaymentRequest: resp.GetPaymentRequest(),
		PaymentHash:    paymentHash,
		AddIndex:       resp.GetAddIndex(),
	}, nil
}

func setInvoicePriceMsat(invoice *lnrpc.Invoice, priceMsat uint64) error {
	if priceMsat == 0 {
		return errors.New("price_msat must be > 0 (amount-less invoice is not allowed)")
	}

	const msatPerSat = uint64(1000)

	// Prefer the satoshi value field when possible for maximum compatibility with
	// older lnd versions that might ignore value_msat.
	if priceMsat%msatPerSat == 0 {
		sats := priceMsat / msatPerSat
		if sats > uint64(maxInt64) {
			return fmt.Errorf("price_msat overflows int64 sats: %d", priceMsat)
		}
		invoice.Value = int64(sats)
		return nil
	}

	if priceMsat > uint64(maxInt64) {
		return fmt.Errorf("price_msat overflows int64: %d", priceMsat)
	}
	invoice.ValueMsat = int64(priceMsat)
	return nil
}

func (a *LNDAdapter) WaitForSettlement(
	ctx context.Context,
	paymentHash lcp.Hash32,
	addIndex uint64,
) error {
	if a.client == nil {
		return errors.New("lnd invoice subscriber not configured")
	}

	startIndex := uint64(0)
	if addIndex > 0 {
		// SubscribeInvoices replays invoices with add_index > startIndex.
		// Use addIndex-1 so the created invoice is included in the catch-up window.
		startIndex = addIndex - 1
	}

	sub, err := a.client.SubscribeInvoices(ctx, &lnrpc.InvoiceSubscription{
		AddIndex: startIndex,
	})
	if err != nil {
		return fmt.Errorf("SubscribeInvoices: %w", err)
	}

	for {
		invoice, recvErr := sub.Recv()
		if recvErr != nil {
			if errors.Is(recvErr, context.Canceled) ||
				errors.Is(recvErr, context.DeadlineExceeded) {
				return ctx.Err()
			}
			return fmt.Errorf("SubscribeInvoices recv: %w", recvErr)
		}
		if invoice == nil {
			continue
		}

		if !bytes.Equal(invoice.GetRHash(), paymentHash[:]) {
			continue
		}

		switch invoice.GetState() {
		case lnrpc.Invoice_OPEN, lnrpc.Invoice_ACCEPTED:
			continue
		case lnrpc.Invoice_SETTLED:
			return nil
		case lnrpc.Invoice_CANCELED:
			return errors.New("invoice canceled")
		}
	}
}
