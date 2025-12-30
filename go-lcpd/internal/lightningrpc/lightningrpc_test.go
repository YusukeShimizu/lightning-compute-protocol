//nolint:testpackage // Tests use internal fakes by wiring unexported Client fields.
package lightningrpc

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"testing"

	"github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"
	"github.com/bruwbird/lcp/go-lcpd/internal/lnd/lnrpc"
	"github.com/bruwbird/lcp/go-lcpd/internal/lnd/routerrpc"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

func TestGetInfo(t *testing.T) {
	t.Parallel()

	client := &Client{
		lightning: &fakeLightning{
			getInfoResp: &lnrpc.GetInfoResponse{IdentityPubkey: "node-123"},
		},
	}

	info, err := client.GetInfo(context.Background())
	if err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	if info.IdentityPubKey != "node-123" {
		t.Fatalf("IdentityPubKey mismatch: got %q", info.IdentityPubKey)
	}
}

func TestGetInfo_NotConfigured(t *testing.T) {
	t.Parallel()

	client := &Client{}
	if _, err := client.GetInfo(context.Background()); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("expected ErrNotConfigured, got %v", err)
	}
}

func TestDecodePaymentRequest(t *testing.T) {
	t.Parallel()

	var desc lcp.Hash32
	for i := range desc {
		desc[i] = 0x11
	}

	client := &Client{
		lightning: &fakeLightning{
			decodeResp: &lnrpc.PayReq{
				DescriptionHash: hex.EncodeToString(desc[:]),
				Destination:     "peer-xyz",
				NumMsat:         123000,
				Timestamp:       1700000000,
				Expiry:          60,
			},
		},
	}

	info, err := client.DecodePaymentRequest(context.Background(), "lnbc1...")
	if err != nil {
		t.Fatalf("DecodePaymentRequest: %v", err)
	}
	if diff := cmp.Diff(desc, info.DescriptionHash); diff != "" {
		t.Fatalf("description_hash mismatch (-want +got):\n%s", diff)
	}
	if info.PayeePubKey != "peer-xyz" {
		t.Fatalf("payee mismatch: got %q", info.PayeePubKey)
	}
	if got, want := info.AmountMsat, int64(123000); got != want {
		t.Fatalf("amount_msat mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
	if got, want := info.TimestampUnix, int64(1700000000); got != want {
		t.Fatalf("timestamp mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
	if got, want := info.ExpirySeconds, int64(60); got != want {
		t.Fatalf("expiry_seconds mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
}

func TestDecodePaymentRequest_InvalidHash(t *testing.T) {
	t.Parallel()

	client := &Client{
		lightning: &fakeLightning{
			decodeResp: &lnrpc.PayReq{
				DescriptionHash: "0102",
				Destination:     "peer-xyz",
			},
		},
	}

	if _, err := client.DecodePaymentRequest(context.Background(), "lnbc1..."); err == nil {
		t.Fatalf("expected error for invalid description_hash length")
	}
}

func TestPayInvoice_Success(t *testing.T) {
	t.Parallel()

	var preimage lcp.Hash32
	for i := range preimage {
		preimage[i] = 0xaa
	}

	stream := &fakeSendPaymentStream{
		updates: []recvResult{
			{payment: &lnrpc.Payment{Status: lnrpc.Payment_IN_FLIGHT}},
			{payment: &lnrpc.Payment{
				Status:          lnrpc.Payment_SUCCEEDED,
				PaymentPreimage: string(preimage[:]),
			}},
			{err: io.EOF},
		},
	}

	client := &Client{
		router: &fakeRouter{stream: stream},
	}

	got, err := client.PayInvoice(context.Background(), "lnbc1...")
	if err != nil {
		t.Fatalf("PayInvoice: %v", err)
	}
	if diff := cmp.Diff(preimage, got); diff != "" {
		t.Fatalf("preimage mismatch (-want +got):\n%s", diff)
	}
}

func TestPayInvoice_Success_HexPreimage(t *testing.T) {
	t.Parallel()

	var preimage lcp.Hash32
	for i := range preimage {
		preimage[i] = 0xaa
	}
	preimageHex := hex.EncodeToString(preimage[:])

	stream := &fakeSendPaymentStream{
		updates: []recvResult{
			{payment: &lnrpc.Payment{Status: lnrpc.Payment_IN_FLIGHT}},
			{payment: &lnrpc.Payment{
				Status:          lnrpc.Payment_SUCCEEDED,
				PaymentPreimage: preimageHex,
			}},
			{err: io.EOF},
		},
	}

	client := &Client{
		router: &fakeRouter{stream: stream},
	}

	got, err := client.PayInvoice(context.Background(), "lnbc1...")
	if err != nil {
		t.Fatalf("PayInvoice: %v", err)
	}
	if diff := cmp.Diff(preimage, got); diff != "" {
		t.Fatalf("preimage mismatch (-want +got):\n%s", diff)
	}
}

func TestPayInvoice_Failed(t *testing.T) {
	t.Parallel()

	stream := &fakeSendPaymentStream{
		updates: []recvResult{
			{payment: &lnrpc.Payment{
				Status:        lnrpc.Payment_FAILED,
				FailureReason: lnrpc.PaymentFailureReason_FAILURE_REASON_TIMEOUT,
			}},
			{err: io.EOF},
		},
	}

	client := &Client{
		router: &fakeRouter{stream: stream},
	}

	if _, err := client.PayInvoice(context.Background(), "lnbc1..."); !errors.Is(
		err,
		ErrPaymentFailed,
	) {
		t.Fatalf("expected ErrPaymentFailed, got %v", err)
	}
}

func TestPayInvoice_NoFinalStatus(t *testing.T) {
	t.Parallel()

	stream := &fakeSendPaymentStream{
		updates: []recvResult{
			{payment: &lnrpc.Payment{Status: lnrpc.Payment_IN_FLIGHT}},
			{err: io.EOF},
		},
	}

	client := &Client{
		router: &fakeRouter{stream: stream},
	}

	if _, err := client.PayInvoice(context.Background(), "lnbc1..."); err == nil {
		t.Fatalf("expected error for missing final status")
	}
}

type fakeLightning struct {
	getInfoResp *lnrpc.GetInfoResponse
	getInfoErr  error

	decodeResp *lnrpc.PayReq
	decodeErr  error
}

func (f *fakeLightning) GetInfo(
	_ context.Context,
	_ *lnrpc.GetInfoRequest,
	_ ...grpc.CallOption,
) (*lnrpc.GetInfoResponse, error) {
	return f.getInfoResp, f.getInfoErr
}

func (f *fakeLightning) DecodePayReq(
	_ context.Context,
	_ *lnrpc.PayReqString,
	_ ...grpc.CallOption,
) (*lnrpc.PayReq, error) {
	return f.decodeResp, f.decodeErr
}

type fakeRouter struct {
	stream routerrpc.Router_SendPaymentV2Client
	err    error
	req    *routerrpc.SendPaymentRequest
}

func (f *fakeRouter) SendPaymentV2(
	_ context.Context,
	req *routerrpc.SendPaymentRequest,
	_ ...grpc.CallOption,
) (routerrpc.Router_SendPaymentV2Client, error) {
	f.req = req
	return f.stream, f.err
}

type recvResult struct {
	payment *lnrpc.Payment
	err     error
}

type fakeSendPaymentStream struct {
	updates []recvResult
	idx     int
}

func (f *fakeSendPaymentStream) Recv() (*lnrpc.Payment, error) {
	if f.idx >= len(f.updates) {
		return nil, io.EOF
	}
	u := f.updates[f.idx]
	f.idx++
	if u.err != nil {
		return nil, u.err
	}
	return u.payment, nil
}

func (f *fakeSendPaymentStream) Header() (metadata.MD, error) { return metadata.MD{}, nil }
func (f *fakeSendPaymentStream) Trailer() metadata.MD         { return metadata.MD{} }
func (f *fakeSendPaymentStream) CloseSend() error             { return nil }
func (f *fakeSendPaymentStream) Context() context.Context     { return context.Background() }
func (f *fakeSendPaymentStream) SendMsg(any) error            { return nil }
func (f *fakeSendPaymentStream) RecvMsg(any) error            { return nil }
