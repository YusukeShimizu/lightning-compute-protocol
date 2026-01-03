package config

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/sethvargo/go-envconfig"
)

type Config struct {
	HTTPAddr     string
	LCPDGRPCAddr string

	APIKeys map[string]struct{} // if empty, auth is disabled

	DefaultPeerID string
	ModelMap      map[string]string // model -> peer_id

	ModelAllowlist      map[string]struct{} // if non-empty, validate model names against this
	AllowUnlistedModels bool

	MaxPriceMsat uint64

	MaxPromptBytes int // if >0, reject prompts larger than this

	TimeoutQuote   time.Duration
	TimeoutExecute time.Duration

	LogLevel string
}

type envConfig struct {
	HTTPAddr     string `env:"OPENAI_SERVE_HTTP_ADDR"`
	LCPDGRPCAddr string `env:"OPENAI_SERVE_LCPD_GRPC_ADDR"`
	LogLevel     string `env:"OPENAI_SERVE_LOG_LEVEL"`

	APIKeys string `env:"OPENAI_SERVE_API_KEYS"`

	DefaultPeerID string `env:"OPENAI_SERVE_DEFAULT_PEER_ID"`
	ModelMap      string `env:"OPENAI_SERVE_MODEL_MAP"`

	ModelAllowlist      string `env:"OPENAI_SERVE_MODEL_ALLOWLIST"`
	AllowUnlistedModels bool   `env:"OPENAI_SERVE_ALLOW_UNLISTED_MODELS"`

	MaxPriceMsat uint64 `env:"OPENAI_SERVE_MAX_PRICE_MSAT"`

	MaxPromptBytes *int `env:"OPENAI_SERVE_MAX_PROMPT_BYTES"`

	TimeoutQuote   time.Duration `env:"OPENAI_SERVE_TIMEOUT_QUOTE"`
	TimeoutExecute time.Duration `env:"OPENAI_SERVE_TIMEOUT_EXECUTE"`
}

const (
	defaultHTTPAddr     = "127.0.0.1:8080"
	defaultLCPDGRPCAddr = "127.0.0.1:50051"
	defaultLogLevel     = "info"

	defaultMaxPromptBytes = 60000

	defaultTimeoutQuote   = 5 * time.Second
	defaultTimeoutExecute = 120 * time.Second

	peerIDHexLen  = 66
	peerIDByteLen = peerIDHexLen / 2
)

func LoadFromEnv(ctx context.Context) (Config, error) {
	cfg := Config{
		HTTPAddr:     defaultHTTPAddr,
		LCPDGRPCAddr: defaultLCPDGRPCAddr,
		LogLevel:     defaultLogLevel,

		APIKeys: nil,

		DefaultPeerID:  "",
		ModelMap:       nil,
		ModelAllowlist: nil,

		AllowUnlistedModels: false,
		MaxPriceMsat:        0,
		MaxPromptBytes:      defaultMaxPromptBytes,

		TimeoutQuote:   defaultTimeoutQuote,
		TimeoutExecute: defaultTimeoutExecute,
	}

	var env envConfig
	if err := envconfig.Process(ctx, &env); err != nil {
		return Config{}, fmt.Errorf("process env: %w", err)
	}

	if v := strings.TrimSpace(env.HTTPAddr); v != "" {
		cfg.HTTPAddr = v
	}
	if v := strings.TrimSpace(env.LCPDGRPCAddr); v != "" {
		cfg.LCPDGRPCAddr = v
	}
	if v := strings.TrimSpace(env.LogLevel); v != "" {
		cfg.LogLevel = v
	}

	if v := strings.TrimSpace(env.APIKeys); v != "" {
		cfg.APIKeys = parseCSVSet(v)
	}

	if v := strings.TrimSpace(env.DefaultPeerID); v != "" {
		if err := validatePeerID(v); err != nil {
			return Config{}, fmt.Errorf("OPENAI_SERVE_DEFAULT_PEER_ID: %w", err)
		}
		cfg.DefaultPeerID = v
	}

	if v := strings.TrimSpace(env.ModelMap); v != "" {
		m, err := parseModelMap(v)
		if err != nil {
			return Config{}, fmt.Errorf("OPENAI_SERVE_MODEL_MAP: %w", err)
		}
		cfg.ModelMap = m
	}

	if v := strings.TrimSpace(env.ModelAllowlist); v != "" {
		cfg.ModelAllowlist = parseCSVSet(v)
	}

	cfg.AllowUnlistedModels = env.AllowUnlistedModels
	cfg.MaxPriceMsat = env.MaxPriceMsat

	if env.MaxPromptBytes != nil {
		cfg.MaxPromptBytes = *env.MaxPromptBytes
	}

	if env.TimeoutQuote > 0 {
		cfg.TimeoutQuote = env.TimeoutQuote
	}
	if env.TimeoutExecute > 0 {
		cfg.TimeoutExecute = env.TimeoutExecute
	}

	if err := validateConfig(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func validateConfig(cfg Config) error {
	if strings.TrimSpace(cfg.HTTPAddr) == "" {
		return errors.New("OPENAI_SERVE_HTTP_ADDR is required")
	}
	if strings.TrimSpace(cfg.LCPDGRPCAddr) == "" {
		return errors.New("OPENAI_SERVE_LCPD_GRPC_ADDR is required")
	}

	// Best-effort validation. `net.Listen` will be the real authority, but
	// catching obvious errors early is helpful.
	if _, _, err := net.SplitHostPort(cfg.HTTPAddr); err != nil {
		return fmt.Errorf("OPENAI_SERVE_HTTP_ADDR must be host:port: %w", err)
	}

	if cfg.TimeoutQuote <= 0 {
		return errors.New("OPENAI_SERVE_TIMEOUT_QUOTE must be > 0")
	}
	if cfg.TimeoutExecute <= 0 {
		return errors.New("OPENAI_SERVE_TIMEOUT_EXECUTE must be > 0")
	}
	if cfg.MaxPromptBytes < 0 {
		return errors.New("OPENAI_SERVE_MAX_PROMPT_BYTES must be >= 0")
	}
	return nil
}

func parseCSVSet(s string) map[string]struct{} {
	out := make(map[string]struct{})
	for part := range strings.SplitSeq(s, ",") {
		v := strings.TrimSpace(part)
		if v == "" {
			continue
		}
		out[v] = struct{}{}
	}
	return out
}

func parseModelMap(s string) (map[string]string, error) {
	out := make(map[string]string)
	for part := range strings.SplitSeq(s, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		model, peer, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("expected 'model=peer_id' entries separated by ';': %q", part)
		}
		model = strings.TrimSpace(model)
		peer = strings.TrimSpace(peer)
		if model == "" {
			return nil, fmt.Errorf("empty model in entry %q", part)
		}
		if err := validatePeerID(peer); err != nil {
			return nil, fmt.Errorf("peer_id for model %q: %w", model, err)
		}
		out[model] = peer
	}
	return out, nil
}

func validatePeerID(peerID string) error {
	peerID = strings.TrimSpace(peerID)
	if peerID == "" {
		return errors.New("peer_id is empty")
	}
	if len(peerID) != peerIDHexLen {
		return fmt.Errorf("peer_id must be %d hex chars (got %d)", peerIDHexLen, len(peerID))
	}
	decoded, err := hex.DecodeString(peerID)
	if err != nil {
		return errors.New("peer_id must be hex")
	}
	if len(decoded) != peerIDByteLen {
		return fmt.Errorf("peer_id must be %d bytes (got %d)", peerIDByteLen, len(decoded))
	}
	return nil
}
