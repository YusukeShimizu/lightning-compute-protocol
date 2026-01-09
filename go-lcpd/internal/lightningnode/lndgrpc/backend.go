package lndgrpc

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"
	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
	"github.com/bruwbird/lcp/go-lcpd/internal/lightningnode"
	"github.com/bruwbird/lcp/go-lcpd/internal/lnd/lnrpc"
	"github.com/bruwbird/lcp/go-lcpd/internal/lnd/routerrpc"
	"github.com/bruwbird/lcp/go-lcpd/internal/lndpeermsg"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

const (
	defaultPaymentTimeoutSeconds = int32(30)
	defaultFeeLimitSat           = int64(10_000)
	maxInt64                     = int64(^uint64(0) >> 1)
)

// Backend implements lightningnode.Backend using lnd's gRPC APIs.
type Backend struct {
	conn      *grpc.ClientConn
	lightning lnrpc.LightningClient
	router    routerrpc.RouterClient
	logger    *zap.SugaredLogger
}

// New dials lnd using the provided config and returns a Lightning node backend.
func New(ctx context.Context, cfg lndpeermsg.Config, logger *zap.SugaredLogger) (*Backend, error) {
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}

	conn, err := lndpeermsg.Dial(ctx, cfg)
	if err != nil {
		return nil, err
	}

	return &Backend{
		conn:      conn,
		lightning: lnrpc.NewLightningClient(conn),
		router:    routerrpc.NewRouterClient(conn),
		logger:    logger.With("component", "lightningnode.lndgrpc"),
	}, nil
}

func (b *Backend) Close() error {
	if b == nil || b.conn == nil {
		return nil
	}
	return b.conn.Close()
}

func (b *Backend) GetNodeInfo(ctx context.Context) (lightningnode.NodeInfo, error) {
	if b == nil || b.lightning == nil {
		return lightningnode.NodeInfo{}, lightningnode.ErrNotConfigured
	}

	resp, err := b.lightning.GetInfo(ctx, &lnrpc.GetInfoRequest{})
	if err != nil {
		return lightningnode.NodeInfo{}, fmt.Errorf("GetInfo: %w", err)
	}

	identity := strings.TrimSpace(resp.GetIdentityPubkey())
	if identity == "" {
		return lightningnode.NodeInfo{}, fmt.Errorf(
			"%w: identity_pubkey is empty",
			lightningnode.ErrInvalidRequest,
		)
	}
	return lightningnode.NodeInfo{IdentityPubKey: identity}, nil
}

func (b *Backend) ListPeers(ctx context.Context) ([]lightningnode.Peer, error) {
	if b == nil || b.lightning == nil {
		return nil, lightningnode.ErrNotConfigured
	}

	resp, err := b.lightning.ListPeers(ctx, &lnrpc.ListPeersRequest{})
	if err != nil {
		return nil, fmt.Errorf("ListPeers: %w", err)
	}

	raw := resp.GetPeers()
	out := make([]lightningnode.Peer, 0, len(raw))
	for _, peer := range raw {
		if peer == nil {
			continue
		}
		pubKey := strings.TrimSpace(peer.GetPubKey())
		if pubKey == "" {
			continue
		}
		out = append(out, lightningnode.Peer{
			PubKey:  pubKey,
			Address: strings.TrimSpace(peer.GetAddress()),
		})
	}
	return out, nil
}

func (b *Backend) SubscribePeerEvents(
	ctx context.Context,
) (lightningnode.PeerEventStream, error) {
	if b == nil || b.lightning == nil {
		return nil, lightningnode.ErrNotConfigured
	}

	stream, err := b.lightning.SubscribePeerEvents(ctx, &lnrpc.PeerEventSubscription{})
	if err != nil {
		return nil, fmt.Errorf("SubscribePeerEvents: %w", err)
	}
	return peerEventStream{stream: stream}, nil
}

func (b *Backend) SubscribeCustomMessages(
	ctx context.Context,
) (lightningnode.CustomMessageStream, error) {
	if b == nil || b.lightning == nil {
		return nil, lightningnode.ErrNotConfigured
	}

	stream, err := b.lightning.SubscribeCustomMessages(ctx, &lnrpc.SubscribeCustomMessagesRequest{})
	if err != nil {
		return nil, fmt.Errorf("SubscribeCustomMessages: %w", err)
	}
	return customMessageStream{stream: stream}, nil
}

func (b *Backend) SendCustomMessage(
	ctx context.Context,
	peerPubKey string,
	msgType lcpwire.MessageType,
	payload []byte,
) error {
	if b == nil || b.lightning == nil {
		return lightningnode.ErrNotConfigured
	}
	if strings.TrimSpace(peerPubKey) == "" {
		return fmt.Errorf("%w: peer_pub_key is required", lightningnode.ErrInvalidRequest)
	}

	peerBytes, err := hex.DecodeString(peerPubKey)
	if err != nil {
		return fmt.Errorf(
			"%w: decode peer_pub_key: %s",
			lightningnode.ErrInvalidRequest,
			err.Error(),
		)
	}

	_, err = b.lightning.SendCustomMessage(ctx, &lnrpc.SendCustomMessageRequest{
		Peer: peerBytes,
		Type: uint32(msgType),
		Data: payload,
	})
	if err != nil {
		return fmt.Errorf("SendCustomMessage: %w", err)
	}
	return nil
}

func (b *Backend) DisconnectPeer(ctx context.Context, peerPubKey string) error {
	if b == nil || b.lightning == nil {
		return lightningnode.ErrNotConfigured
	}
	if strings.TrimSpace(peerPubKey) == "" {
		return fmt.Errorf("%w: peer_pub_key is required", lightningnode.ErrInvalidRequest)
	}

	_, err := b.lightning.DisconnectPeer(ctx, &lnrpc.DisconnectPeerRequest{PubKey: peerPubKey})
	if err != nil {
		return fmt.Errorf("DisconnectPeer: %w", err)
	}
	return nil
}

func (b *Backend) CreateInvoice(
	ctx context.Context,
	req lightningnode.CreateInvoiceRequest,
) (lightningnode.CreateInvoiceResponse, error) {
	if b == nil || b.lightning == nil {
		return lightningnode.CreateInvoiceResponse{}, lightningnode.ErrNotConfigured
	}

	if req.AmountMsat == 0 {
		return lightningnode.CreateInvoiceResponse{}, fmt.Errorf(
			"%w: amount_msat must be > 0",
			lightningnode.ErrInvalidRequest,
		)
	}
	if req.ExpirySeconds > uint64(maxInt64) {
		return lightningnode.CreateInvoiceResponse{}, fmt.Errorf(
			"%w: expiry_seconds overflows int64: %d",
			lightningnode.ErrInvalidRequest,
			req.ExpirySeconds,
		)
	}

	invoice := &lnrpc.Invoice{
		DescriptionHash: req.DescriptionHash[:],
		Expiry:          int64(req.ExpirySeconds),
	}
	if err := setInvoiceAmountMsat(invoice, req.AmountMsat); err != nil {
		return lightningnode.CreateInvoiceResponse{}, err
	}

	resp, err := b.lightning.AddInvoice(ctx, invoice)
	if err != nil {
		return lightningnode.CreateInvoiceResponse{}, fmt.Errorf("AddInvoice: %w", err)
	}

	var paymentHash lcp.Hash32
	if len(resp.GetRHash()) != len(paymentHash) {
		return lightningnode.CreateInvoiceResponse{}, fmt.Errorf(
			"%w: unexpected r_hash length: %d",
			lightningnode.ErrInvalidRequest,
			len(resp.GetRHash()),
		)
	}
	copy(paymentHash[:], resp.GetRHash())

	return lightningnode.CreateInvoiceResponse{
		PaymentRequest: resp.GetPaymentRequest(),
		PaymentHash:    paymentHash,
	}, nil
}

func setInvoiceAmountMsat(invoice *lnrpc.Invoice, amountMsat uint64) error {
	const msatPerSat = uint64(1000)

	if amountMsat%msatPerSat == 0 {
		sats := amountMsat / msatPerSat
		if sats > uint64(maxInt64) {
			return fmt.Errorf(
				"%w: amount_msat overflows int64 sats: %d",
				lightningnode.ErrInvalidRequest,
				amountMsat,
			)
		}
		invoice.Value = int64(sats)
		return nil
	}

	if amountMsat > uint64(maxInt64) {
		return fmt.Errorf(
			"%w: amount_msat overflows int64: %d",
			lightningnode.ErrInvalidRequest,
			amountMsat,
		)
	}
	invoice.ValueMsat = int64(amountMsat)
	return nil
}

func (b *Backend) WaitInvoiceSettled(
	ctx context.Context,
	paymentHash lcp.Hash32,
) (lightningnode.InvoiceSettlementState, error) {
	if b == nil || b.lightning == nil {
		return lightningnode.InvoiceSettlementStateUnspecified, lightningnode.ErrNotConfigured
	}

	sub, err := b.lightning.SubscribeInvoices(ctx, &lnrpc.InvoiceSubscription{AddIndex: 0})
	if err != nil {
		return lightningnode.InvoiceSettlementStateUnspecified, fmt.Errorf(
			"SubscribeInvoices: %w",
			err,
		)
	}

	for {
		invoice, recvErr := sub.Recv()
		if recvErr != nil {
			if errors.Is(recvErr, context.Canceled) ||
				errors.Is(recvErr, context.DeadlineExceeded) {
				return lightningnode.InvoiceSettlementStateUnspecified, ctx.Err()
			}
			return lightningnode.InvoiceSettlementStateUnspecified, fmt.Errorf(
				"SubscribeInvoices recv: %w",
				recvErr,
			)
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
			return lightningnode.InvoiceSettlementStateSettled, nil
		case lnrpc.Invoice_CANCELED:
			return lightningnode.InvoiceSettlementStateCanceled, nil
		default:
			return lightningnode.InvoiceSettlementStateUnspecified, fmt.Errorf(
				"%w: unknown invoice state: %s",
				lightningnode.ErrInvalidRequest,
				invoice.GetState().String(),
			)
		}
	}
}

func (b *Backend) DecodeInvoice(
	ctx context.Context,
	paymentRequest string,
) (lightningnode.DecodedInvoice, error) {
	if b == nil || b.lightning == nil {
		return lightningnode.DecodedInvoice{}, lightningnode.ErrNotConfigured
	}
	if strings.TrimSpace(paymentRequest) == "" {
		return lightningnode.DecodedInvoice{}, fmt.Errorf(
			"%w: payment_request is required",
			lightningnode.ErrInvalidRequest,
		)
	}

	resp, err := b.lightning.DecodePayReq(ctx, &lnrpc.PayReqString{PayReq: paymentRequest})
	if err != nil {
		return lightningnode.DecodedInvoice{}, fmt.Errorf("DecodePayReq: %w", err)
	}

	descriptionHashHex := strings.TrimSpace(resp.GetDescriptionHash())
	if descriptionHashHex == "" {
		return lightningnode.DecodedInvoice{}, fmt.Errorf(
			"%w: description_hash is empty",
			lightningnode.ErrInvalidRequest,
		)
	}

	descriptionHash, err := hex.DecodeString(descriptionHashHex)
	if err != nil {
		return lightningnode.DecodedInvoice{}, fmt.Errorf(
			"%w: decode description_hash: %s",
			lightningnode.ErrInvalidRequest,
			err.Error(),
		)
	}
	if len(descriptionHash) != lcp.Hash32Len {
		return lightningnode.DecodedInvoice{}, fmt.Errorf(
			"%w: description_hash must be %d bytes, got %d",
			lightningnode.ErrInvalidRequest,
			lcp.Hash32Len,
			len(descriptionHash),
		)
	}

	payee := strings.TrimSpace(resp.GetDestination())
	if payee == "" {
		return lightningnode.DecodedInvoice{}, fmt.Errorf(
			"%w: destination is empty",
			lightningnode.ErrInvalidRequest,
		)
	}

	numMsat := resp.GetNumMsat()
	numSats := resp.GetNumSatoshis()
	if numMsat == 0 && numSats > 0 {
		const msatPerSat = int64(1000)
		if numSats > (maxInt64 / msatPerSat) {
			return lightningnode.DecodedInvoice{}, fmt.Errorf(
				"%w: invoice amount overflows msat",
				lightningnode.ErrInvalidRequest,
			)
		}
		numMsat = numSats * msatPerSat
	}
	if numMsat < 0 {
		return lightningnode.DecodedInvoice{}, fmt.Errorf(
			"%w: invoice amount_msat is negative",
			lightningnode.ErrInvalidRequest,
		)
	}

	timestampUnix := resp.GetTimestamp()
	if timestampUnix < 0 {
		return lightningnode.DecodedInvoice{}, fmt.Errorf(
			"%w: invoice timestamp is negative",
			lightningnode.ErrInvalidRequest,
		)
	}

	expirySeconds := resp.GetExpiry()
	if expirySeconds < 0 {
		return lightningnode.DecodedInvoice{}, fmt.Errorf(
			"%w: invoice expiry_seconds is negative",
			lightningnode.ErrInvalidRequest,
		)
	}

	var desc lcp.Hash32
	copy(desc[:], descriptionHash)

	return lightningnode.DecodedInvoice{
		PayeePubKey:     payee,
		DescriptionHash: desc,
		AmountMsat:      numMsat,
		TimestampUnix:   timestampUnix,
		ExpirySeconds:   expirySeconds,
	}, nil
}

func (b *Backend) PayInvoice(ctx context.Context, paymentRequest string) (lcp.Hash32, error) {
	if b == nil || b.router == nil {
		return lcp.Hash32{}, lightningnode.ErrNotConfigured
	}
	if strings.TrimSpace(paymentRequest) == "" {
		return lcp.Hash32{}, fmt.Errorf(
			"%w: payment_request is required",
			lightningnode.ErrInvalidRequest,
		)
	}

	stream, err := b.router.SendPaymentV2(ctx, &routerrpc.SendPaymentRequest{
		PaymentRequest:    paymentRequest,
		TimeoutSeconds:    defaultPaymentTimeoutSeconds,
		FeeLimitSat:       defaultFeeLimitSat,
		NoInflightUpdates: true,
	})
	if err != nil {
		return lcp.Hash32{}, fmt.Errorf("SendPaymentV2: %w", err)
	}

	final, err := recvFinalPayment(ctx, stream)
	if err != nil {
		return lcp.Hash32{}, err
	}

	if final == nil {
		return lcp.Hash32{}, fmt.Errorf(
			"%w: no final payment status",
			lightningnode.ErrPaymentFailed,
		)
	}

	if final.GetStatus() == lnrpc.Payment_FAILED {
		return lcp.Hash32{}, fmt.Errorf(
			"%w: %s",
			lightningnode.ErrPaymentFailed,
			final.GetFailureReason().String(),
		)
	}

	preimage, err := decodePaymentPreimage(final.GetPaymentPreimage())
	if err != nil {
		return lcp.Hash32{}, err
	}
	if b.logger != nil {
		b.logger.Debugw("payment succeeded", "payment_hash", final.GetPaymentHash())
	}
	return preimage, nil
}

type paymentStream interface {
	Recv() (*lnrpc.Payment, error)
}

func recvFinalPayment(ctx context.Context, stream paymentStream) (*lnrpc.Payment, error) {
	for {
		update, err := stream.Recv()
		if err != nil {
			if errors.Is(err, context.Canceled) ||
				errors.Is(err, context.DeadlineExceeded) {
				return nil, ctx.Err()
			}
			if errors.Is(err, io.EOF) {
				return nil, fmt.Errorf(
					"%w: no final payment status",
					lightningnode.ErrPaymentFailed,
				)
			}
			return nil, fmt.Errorf("SendPaymentV2 recv: %w", err)
		}
		if update == nil {
			continue
		}

		status := update.GetStatus()
		if status == lnrpc.Payment_SUCCEEDED || status == lnrpc.Payment_FAILED {
			return update, nil
		}
	}
}

func decodePaymentPreimage(preimage string) (lcp.Hash32, error) {
	if preimage == "" {
		return lcp.Hash32{}, fmt.Errorf(
			"%w: payment_preimage is empty",
			lightningnode.ErrInvalidRequest,
		)
	}

	var preimageBytes []byte
	if len(preimage) == lcp.Hash32Len*2 {
		decoded, decodeErr := hex.DecodeString(preimage)
		if decodeErr != nil {
			return lcp.Hash32{}, fmt.Errorf(
				"%w: decode payment_preimage: %s",
				lightningnode.ErrInvalidRequest,
				decodeErr.Error(),
			)
		}
		preimageBytes = decoded
	} else {
		preimageBytes = []byte(preimage)
	}

	if len(preimageBytes) != lcp.Hash32Len {
		return lcp.Hash32{}, fmt.Errorf(
			"%w: payment_preimage must be %d bytes, got %d",
			lightningnode.ErrInvalidRequest,
			lcp.Hash32Len,
			len(preimageBytes),
		)
	}

	var out lcp.Hash32
	copy(out[:], preimageBytes)
	return out, nil
}

var _ lightningnode.Backend = (*Backend)(nil)
