package lndpeermsg

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
)

const (
	defaultDialTimeout      = 15 * time.Second
	defaultReconnectBackoff = 1 * time.Second
)

type macaroonCredential struct {
	macaroonHex string
}

func (m macaroonCredential) GetRequestMetadata(
	context.Context,
	...string,
) (map[string]string, error) {
	if m.macaroonHex == "" {
		return map[string]string{}, nil
	}
	return map[string]string{"macaroon": m.macaroonHex}, nil
}

func (macaroonCredential) RequireTransportSecurity() bool { return true }

func dial(ctx context.Context, cfg Config) (*grpc.ClientConn, error) {
	if stringsTrim(cfg.RPCAddr) == "" {
		return nil, errors.New("lnd rpc_addr is required")
	}

	host, _, splitErr := net.SplitHostPort(cfg.RPCAddr)
	if splitErr != nil {
		return nil, fmt.Errorf("parse lnd rpc_addr: %w", splitErr)
	}

	var rootCAs *x509.CertPool
	if stringsTrim(cfg.TLSCertPath) != "" {
		pem, err := os.ReadFile(cfg.TLSCertPath)
		if err != nil {
			return nil, fmt.Errorf("read lnd tls cert: %w", err)
		}

		cp := x509.NewCertPool()
		if !cp.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("append lnd tls cert to pool: %s", cfg.TLSCertPath)
		}
		rootCAs = cp
	}

	creds := credentials.NewTLS(&tls.Config{
		MinVersion: tls.VersionTLS12,
		// When TLSCertPath is empty, RootCAs is nil (system roots).
		RootCAs:    rootCAs,
		ServerName: host,
	})

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
	}

	if stringsTrim(cfg.AdminMacaroonPath) != "" {
		raw, readErr := os.ReadFile(cfg.AdminMacaroonPath)
		if readErr != nil {
			return nil, fmt.Errorf("read lnd macaroon: %w", readErr)
		}
		opts = append(opts, grpc.WithPerRPCCredentials(macaroonCredential{
			macaroonHex: hex.EncodeToString(raw),
		}))
	}

	conn, err := grpc.NewClient(cfg.RPCAddr, opts...)
	if err != nil {
		return nil, fmt.Errorf("grpc new client (%s): %w", cfg.RPCAddr, err)
	}

	dialCtx, cancel := withTimeoutIfNone(ctx, defaultDialTimeout)
	defer cancel()

	// grpc.NewClient performs no I/O; wait for the underlying connection to become ready.
	conn.Connect()
	for {
		state := conn.GetState()
		if state == connectivity.Ready {
			return conn, nil
		}
		if !conn.WaitForStateChange(dialCtx, state) {
			_ = conn.Close()
			return nil, fmt.Errorf("grpc connect lnd (%s): %w", cfg.RPCAddr, dialCtx.Err())
		}
	}
}

func withTimeoutIfNone(
	ctx context.Context,
	timeout time.Duration,
) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

func stringsTrim(s string) string {
	return strings.TrimSpace(s)
}

// Dial creates a gRPC client connection to lnd using the provided Config.
func Dial(ctx context.Context, cfg Config) (*grpc.ClientConn, error) {
	return dial(ctx, cfg)
}
