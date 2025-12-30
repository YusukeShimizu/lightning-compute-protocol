package e2e_test

import (
	"context"
	"net"
	"testing"
	"time"

	lcpdv1 "github.com/bruwbird/lcp/go-lcpd/gen/go/lcpd/v1"
	"github.com/bruwbird/lcp/go-lcpd/itest/harness/lcpd"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

func TestE2E_GRPCSmoke(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)

	addr := freePort(t)

	_ = lcpd.Start(ctx, t, lcpd.RunConfig{
		GRPCAddr: addr,
		Backend:  "deterministic",
	})

	conn, err := grpc.NewClient(
		addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc new client: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	client := lcpdv1.NewLCPDServiceClient(conn)

	listResp, err := client.ListLCPPeers(ctx, &lcpdv1.ListLCPPeersRequest{})
	if err != nil {
		t.Fatalf("ListLCPPeers: %v", err)
	}
	if diff := cmp.Diff(0, len(listResp.GetPeers())); diff != "" {
		t.Fatalf("peers length mismatch (-want +got):\n%s", diff)
	}

	_, err = client.GetLocalInfo(ctx, &lcpdv1.GetLocalInfoRequest{})
	if err == nil {
		t.Fatalf("GetLocalInfo: expected error")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("GetLocalInfo: expected status error, got %T", err)
	}
	if diff := cmp.Diff(codes.Unavailable, st.Code()); diff != "" {
		t.Fatalf("GetLocalInfo status mismatch (-want +got):\n%s", diff)
	}
}

func freePort(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen :0: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}
