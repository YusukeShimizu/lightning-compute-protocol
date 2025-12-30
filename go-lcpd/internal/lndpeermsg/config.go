package lndpeermsg

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sethvargo/go-envconfig"
)

var ErrLNDConnectionNotConfigured = errors.New("lnd connection not configured")

const (
	// defaultManifestResendInterval is a best-effort reliability improvement for
	// manifest exchange across daemon startup ordering (lnd does not buffer
	// inbound custom messages unless the receiver is subscribed).
	defaultManifestResendInterval = 30 * time.Second
)

type Config struct {
	RPCAddr           string
	TLSCertPath       string
	AdminMacaroonPath string

	// ManifestResendInterval controls periodic best-effort re-sends of the local
	// manifest to connected peers. Nil means "use default", 0 disables.
	ManifestResendInterval *time.Duration
}

type envConfig struct {
	RPCAddr             string `env:"LCPD_LND_RPC_ADDR"`
	TLSCertPath         string `env:"LCPD_LND_TLS_CERT_PATH"`
	AdminMacaroonPath   string `env:"LCPD_LND_ADMIN_MACAROON_PATH"`
	AdminMacaroonPathV0 string `env:"LCPD_LND_MACAROON_PATH"`

	ManifestResendInterval *time.Duration `env:"LCPD_LND_MANIFEST_RESEND_INTERVAL"`
}

func NewConfigFromEnv(ctx context.Context) (*Config, error) {
	var env envConfig
	if err := envconfig.Process(ctx, &env); err != nil {
		return nil, fmt.Errorf("process env: %w", err)
	}

	rpcAddr := strings.TrimSpace(env.RPCAddr)
	tlsCertPath := strings.TrimSpace(env.TLSCertPath)
	if rpcAddr == "" {
		return nil, ErrLNDConnectionNotConfigured
	}

	macaroonPath := strings.TrimSpace(env.AdminMacaroonPath)
	if macaroonPath == "" {
		macaroonPath = strings.TrimSpace(env.AdminMacaroonPathV0)
	}

	return &Config{
		RPCAddr:                rpcAddr,
		TLSCertPath:            tlsCertPath,
		AdminMacaroonPath:      macaroonPath,
		ManifestResendInterval: env.ManifestResendInterval,
	}, nil
}
