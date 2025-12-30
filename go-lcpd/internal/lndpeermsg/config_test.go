package lndpeermsg_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bruwbird/lcp/go-lcpd/internal/lndpeermsg"
	"github.com/google/go-cmp/cmp"
)

func TestNewConfigFromEnv_NotConfiguredWhenRPCAddrEmpty(t *testing.T) {
	t.Setenv("LCPD_LND_RPC_ADDR", "")
	t.Setenv("LCPD_LND_TLS_CERT_PATH", "/tmp/tls.cert")
	t.Setenv("LCPD_LND_MANIFEST_RESEND_INTERVAL", "0s")

	_, err := lndpeermsg.NewConfigFromEnv(context.Background())
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !errors.Is(err, lndpeermsg.ErrLNDConnectionNotConfigured) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewConfigFromEnv_AllowsEmptyTLSCertPath(t *testing.T) {
	t.Setenv("LCPD_LND_RPC_ADDR", "localhost:10009")
	t.Setenv("LCPD_LND_TLS_CERT_PATH", "")
	t.Setenv("LCPD_LND_ADMIN_MACAROON_PATH", "/tmp/admin.macaroon")
	t.Setenv("LCPD_LND_MANIFEST_RESEND_INTERVAL", "0s")

	got, err := lndpeermsg.NewConfigFromEnv(context.Background())
	if err != nil {
		t.Fatalf("NewConfigFromEnv: %v", err)
	}

	zero := time.Duration(0)
	want := &lndpeermsg.Config{
		RPCAddr:                "localhost:10009",
		TLSCertPath:            "",
		AdminMacaroonPath:      "/tmp/admin.macaroon",
		ManifestResendInterval: &zero,
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("config mismatch (-want +got):\n%s", diff)
	}
}

func TestNewConfigFromEnv_ParsesManifestIntervals(t *testing.T) {
	t.Setenv("LCPD_LND_RPC_ADDR", "localhost:10009")
	t.Setenv("LCPD_LND_TLS_CERT_PATH", "")
	t.Setenv("LCPD_LND_MANIFEST_RESEND_INTERVAL", "0s")

	got, err := lndpeermsg.NewConfigFromEnv(context.Background())
	if err != nil {
		t.Fatalf("NewConfigFromEnv: %v", err)
	}

	if got.ManifestResendInterval == nil {
		t.Fatalf("ManifestResendInterval is nil")
	}
	if diff := cmp.Diff(time.Duration(0), *got.ManifestResendInterval); diff != "" {
		t.Fatalf("ManifestResendInterval mismatch (-want +got):\n%s", diff)
	}
}
