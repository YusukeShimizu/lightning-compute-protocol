package lightningrpc

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"
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

var (
	ErrNotConfigured  = errors.New("lightning rpc not configured")
	ErrPaymentFailed  = errors.New("payment failed")
	ErrInvalidRequest = errors.New("invalid request")
)

type lightningClient interface {
	GetInfo(
		ctx context.Context,
		in *lnrpc.GetInfoRequest,
		opts ...grpc.CallOption,
	) (*lnrpc.GetInfoResponse, error)
	DecodePayReq(
		ctx context.Context,
		in *lnrpc.PayReqString,
		opts ...grpc.CallOption,
	) (*lnrpc.PayReq, error)
}

type routerClient interface {
	SendPaymentV2(
		ctx context.Context,
		in *routerrpc.SendPaymentRequest,
		opts ...grpc.CallOption,
	) (routerrpc.Router_SendPaymentV2Client, error)
}

// Client provides the minimal Lightning RPC operations required by the Requester.
type Client struct {
	conn      *grpc.ClientConn
	lightning lightningClient
	router    routerClient
	logger    *zap.SugaredLogger
}

type Info struct {
	IdentityPubKey string
}

type PaymentRequestInfo struct {
	DescriptionHash lcp.Hash32
	PayeePubKey     string
	AmountMsat      int64
	TimestampUnix   int64
	ExpirySeconds   int64
}

func New(ctx context.Context, cfg *lndpeermsg.Config, logger *zap.SugaredLogger) (*Client, error) {
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}

	if cfg == nil {
		return &Client{logger: logger.With("component", "lightningrpc")}, nil
	}

	conn, err := lndpeermsg.Dial(ctx, *cfg)
	if err != nil {
		return nil, err
	}

	return &Client{
		conn:      conn,
		lightning: lnrpc.NewLightningClient(conn),
		router:    routerrpc.NewRouterClient(conn),
		logger:    logger.With("component", "lightningrpc"),
	}, nil
}

func (c *Client) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

func (c *Client) GetInfo(ctx context.Context) (Info, error) {
	if c.lightning == nil {
		return Info{}, ErrNotConfigured
	}

	resp, err := c.lightning.GetInfo(ctx, &lnrpc.GetInfoRequest{})
	if err != nil {
		return Info{}, fmt.Errorf("GetInfo: %w", err)
	}
	identity := strings.TrimSpace(resp.GetIdentityPubkey())
	if identity == "" {
		return Info{}, fmt.Errorf("%w: identity_pubkey is empty", ErrInvalidRequest)
	}

	return Info{IdentityPubKey: identity}, nil
}

func (c *Client) DecodePaymentRequest(
	ctx context.Context,
	paymentRequest string,
) (PaymentRequestInfo, error) {
	if c.lightning == nil {
		return PaymentRequestInfo{}, ErrNotConfigured
	}
	if strings.TrimSpace(paymentRequest) == "" {
		return PaymentRequestInfo{}, fmt.Errorf(
			"%w: payment_request is required",
			ErrInvalidRequest,
		)
	}

	resp, err := c.lightning.DecodePayReq(ctx, &lnrpc.PayReqString{PayReq: paymentRequest})
	if err != nil {
		return PaymentRequestInfo{}, fmt.Errorf("DecodePayReq: %w", err)
	}

	descriptionHashHex := strings.TrimSpace(resp.GetDescriptionHash())
	if descriptionHashHex == "" {
		return PaymentRequestInfo{}, fmt.Errorf("%w: description_hash is empty", ErrInvalidRequest)
	}

	descriptionHash, err := hex.DecodeString(descriptionHashHex)
	if err != nil {
		return PaymentRequestInfo{}, fmt.Errorf(
			"%w: decode description_hash: %s",
			ErrInvalidRequest,
			err.Error(),
		)
	}
	if len(descriptionHash) != lcp.Hash32Len {
		return PaymentRequestInfo{}, fmt.Errorf(
			"%w: description_hash must be %d bytes, got %d",
			ErrInvalidRequest,
			lcp.Hash32Len,
			len(descriptionHash),
		)
	}

	payee := strings.TrimSpace(resp.GetDestination())
	if payee == "" {
		return PaymentRequestInfo{}, fmt.Errorf("%w: destination is empty", ErrInvalidRequest)
	}

	numMsat := resp.GetNumMsat()
	numSats := resp.GetNumSatoshis()
	if numMsat == 0 && numSats > 0 {
		const msatPerSat = int64(1000)
		if numSats > (maxInt64 / msatPerSat) {
			return PaymentRequestInfo{}, fmt.Errorf("%w: invoice amount overflows msat", ErrInvalidRequest)
		}
		numMsat = numSats * msatPerSat
	}
	if numMsat < 0 {
		return PaymentRequestInfo{}, fmt.Errorf("%w: invoice amount_msat is negative", ErrInvalidRequest)
	}

	timestampUnix := resp.GetTimestamp()
	if timestampUnix < 0 {
		return PaymentRequestInfo{}, fmt.Errorf("%w: invoice timestamp is negative", ErrInvalidRequest)
	}

	expirySeconds := resp.GetExpiry()
	if expirySeconds < 0 {
		return PaymentRequestInfo{}, fmt.Errorf("%w: invoice expiry_seconds is negative", ErrInvalidRequest)
	}

	var desc lcp.Hash32
	copy(desc[:], descriptionHash)

	return PaymentRequestInfo{
		DescriptionHash: desc,
		PayeePubKey:     payee,
		AmountMsat:      numMsat,
		TimestampUnix:   timestampUnix,
		ExpirySeconds:   expirySeconds,
	}, nil
}

func (c *Client) PayInvoice(
	ctx context.Context,
	paymentRequest string,
) (lcp.Hash32, error) {
	if c.router == nil {
		return lcp.Hash32{}, ErrNotConfigured
	}
	if strings.TrimSpace(paymentRequest) == "" {
		return lcp.Hash32{}, fmt.Errorf("%w: payment_request is required", ErrInvalidRequest)
	}

	stream, err := c.router.SendPaymentV2(ctx, &routerrpc.SendPaymentRequest{
		PaymentRequest:    paymentRequest,
		TimeoutSeconds:    defaultPaymentTimeoutSeconds,
		FeeLimitSat:       defaultFeeLimitSat,
		NoInflightUpdates: true,
	})
	if err != nil {
		return lcp.Hash32{}, fmt.Errorf("SendPaymentV2: %w", err)
	}

	var final *lnrpc.Payment

recvLoop:
	for {
		update, recvErr := stream.Recv()
		if recvErr != nil {
			if errors.Is(recvErr, io.EOF) {
				break
			}
			return lcp.Hash32{}, fmt.Errorf("SendPaymentV2 recv: %w", recvErr)
		}
		if update == nil {
			continue
		}

		status := update.GetStatus()
		if status == lnrpc.Payment_SUCCEEDED || status == lnrpc.Payment_FAILED {
			final = update
			break recvLoop
		}
	}

	if final == nil {
		return lcp.Hash32{}, fmt.Errorf("%w: no final payment status", ErrPaymentFailed)
	}

	if final.GetStatus() == lnrpc.Payment_FAILED {
		return lcp.Hash32{}, fmt.Errorf(
			"%w: %s",
			ErrPaymentFailed,
			final.GetFailureReason().String(),
		)
	}

	return decodePaymentPreimage(final.GetPaymentPreimage())
}

func decodePaymentPreimage(preimage string) (lcp.Hash32, error) {
	if preimage == "" {
		return lcp.Hash32{}, fmt.Errorf("%w: payment_preimage is empty", ErrInvalidRequest)
	}

	var preimageBytes []byte
	if len(preimage) == lcp.Hash32Len*2 {
		decoded, decodeErr := hex.DecodeString(preimage)
		if decodeErr != nil {
			return lcp.Hash32{}, fmt.Errorf(
				"%w: decode payment_preimage: %s",
				ErrInvalidRequest,
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
			ErrInvalidRequest,
			lcp.Hash32Len,
			len(preimageBytes),
		)
	}

	var out lcp.Hash32
	copy(out[:], preimageBytes)
	return out, nil
}
