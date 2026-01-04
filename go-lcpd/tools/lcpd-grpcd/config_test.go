package main

import (
	"io"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func TestParseFlags_Defaults_NoLND(t *testing.T) {
	clearLNDEnv(t)

	got, err := parseFlags([]string{}, io.Discard)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}

	if diff := cmp.Diff("127.0.0.1:50051", got.grpcAddr); diff != "" {
		t.Fatalf("grpc_addr mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(20*time.Second, got.startupTimeout); diff != "" {
		t.Fatalf("startupTimeout mismatch (-want +got):\n%s", diff)
	}
	if got.lnd != nil {
		t.Fatalf("lnd config must be nil")
	}
}

func TestParseFlags_LNDFromEnv(t *testing.T) {
	clearLNDEnv(t)
	t.Setenv("LCPD_LND_RPC_ADDR", "localhost:10009")
	t.Setenv("LCPD_LND_TLS_CERT_PATH", "/tmp/tls.cert")
	t.Setenv("LCPD_LND_ADMIN_MACAROON_PATH", "/tmp/admin.macaroon")

	got, err := parseFlags([]string{}, io.Discard)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}

	if got.lnd == nil {
		t.Fatalf("lnd config is nil")
	}
	if got.lnd.RPCAddr != "localhost:10009" {
		t.Fatalf(
			"lnd rpc addr mismatch (-want +got):\n%s",
			cmp.Diff("localhost:10009", got.lnd.RPCAddr),
		)
	}
	if got.lnd.TLSCertPath != "/tmp/tls.cert" {
		t.Fatalf(
			"lnd tls cert path mismatch (-want +got):\n%s",
			cmp.Diff("/tmp/tls.cert", got.lnd.TLSCertPath),
		)
	}
	if got.lnd.AdminMacaroonPath != "/tmp/admin.macaroon" {
		t.Fatalf(
			"lnd macaroon path mismatch (-want +got):\n%s",
			cmp.Diff("/tmp/admin.macaroon", got.lnd.AdminMacaroonPath),
		)
	}
}

func TestParseFlags_LNDFromEnv_AllowsEmptyTLSCertPath(t *testing.T) {
	clearLNDEnv(t)
	t.Setenv("LCPD_LND_RPC_ADDR", "localhost:10009")
	t.Setenv("LCPD_LND_TLS_CERT_PATH", "")

	got, err := parseFlags([]string{}, io.Discard)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}

	if got.lnd == nil {
		t.Fatalf("lnd config is nil")
	}
	if got.lnd.RPCAddr != "localhost:10009" {
		t.Fatalf(
			"lnd rpc addr mismatch (-want +got):\n%s",
			cmp.Diff("localhost:10009", got.lnd.RPCAddr),
		)
	}
	if got.lnd.TLSCertPath != "" {
		t.Fatalf(
			"lnd tls cert path mismatch (-want +got):\n%s",
			cmp.Diff("", got.lnd.TLSCertPath),
		)
	}
}

func TestParseFlags_LNDFlagsOverrideEnv(t *testing.T) {
	clearLNDEnv(t)
	t.Setenv("LCPD_LND_RPC_ADDR", "localhost:10009")
	t.Setenv("LCPD_LND_TLS_CERT_PATH", "/tmp/env.cert")

	got, err := parseFlags([]string{
		"-lnd_rpc_addr=127.0.0.1:11009",
		"-lnd_tls_cert_path=/tmp/flag.cert",
		"-lnd_admin_macaroon_path=/tmp/flag.macaroon",
		"-startup_timeout=30s",
	}, io.Discard)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}

	if got.startupTimeout != 30*time.Second {
		t.Fatalf(
			"startup timeout mismatch (-want +got):\n%s",
			cmp.Diff(30*time.Second, got.startupTimeout),
		)
	}
	if got.lnd == nil {
		t.Fatalf("lnd config is nil")
	}
	if got.lnd.RPCAddr != "127.0.0.1:11009" {
		t.Fatalf(
			"lnd rpc addr mismatch (-want +got):\n%s",
			cmp.Diff("127.0.0.1:11009", got.lnd.RPCAddr),
		)
	}
	if got.lnd.TLSCertPath != "/tmp/flag.cert" {
		t.Fatalf(
			"lnd tls cert path mismatch (-want +got):\n%s",
			cmp.Diff("/tmp/flag.cert", got.lnd.TLSCertPath),
		)
	}
	if got.lnd.AdminMacaroonPath != "/tmp/flag.macaroon" {
		t.Fatalf(
			"lnd macaroon path mismatch (-want +got):\n%s",
			cmp.Diff("/tmp/flag.macaroon", got.lnd.AdminMacaroonPath),
		)
	}
}

func clearLNDEnv(t *testing.T) {
	t.Helper()
	t.Setenv("LCPD_LND_RPC_ADDR", "")
	t.Setenv("LCPD_LND_TLS_CERT_PATH", "")
	t.Setenv("LCPD_LND_ADMIN_MACAROON_PATH", "")
	t.Setenv("LCPD_LND_MACAROON_PATH", "")
	t.Setenv("LCPD_LND_MANIFEST_RESEND_INTERVAL", "0s")
}

func TestParseFlags_LNDFlagsRequireRPCAddr(t *testing.T) {
	t.Parallel()

	_, err := parseFlags([]string{"-lnd_tls_cert_path=/tmp/tls.cert"}, io.Discard)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
}
