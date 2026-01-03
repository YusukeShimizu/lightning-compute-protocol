package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"slices"
	"strings"
	"time"

	"github.com/bruwbird/lcp/go-lcpd/internal/computebackend"
	openaibackend "github.com/bruwbird/lcp/go-lcpd/internal/computebackend/openai"
	"github.com/bruwbird/lcp/go-lcpd/internal/grpcserver"
	"github.com/bruwbird/lcp/go-lcpd/internal/grpcservice/lcpd"
	"github.com/bruwbird/lcp/go-lcpd/internal/inbounddispatch"
	"github.com/bruwbird/lcp/go-lcpd/internal/jobstore"
	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
	"github.com/bruwbird/lcp/go-lcpd/internal/lightningrpc"
	"github.com/bruwbird/lcp/go-lcpd/internal/llm"
	"github.com/bruwbird/lcp/go-lcpd/internal/lndpeermsg"
	"github.com/bruwbird/lcp/go-lcpd/internal/peerdirectory"
	"github.com/bruwbird/lcp/go-lcpd/internal/provider"
	"github.com/bruwbird/lcp/go-lcpd/internal/replaystore"
	"github.com/bruwbird/lcp/go-lcpd/internal/requesterjobstore"
	"github.com/bruwbird/lcp/go-lcpd/internal/requesterwait"
	"github.com/sethvargo/go-envconfig"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"google.golang.org/grpc"
	"gopkg.in/yaml.v3"
)

const (
	exitOK    = 0
	exitError = 1

	defaultStartupTimeout  = 20 * time.Second
	defaultShutdownTimeout = 3 * time.Second

	defaultMaxPayloadBytes = uint32(16384)
	defaultMaxStreamBytes  = uint64(4_194_304)
	defaultMaxJobBytes     = uint64(8_388_608)
)

type grpcListener struct{ net.Listener }

type config struct {
	grpcAddr       string
	startupTimeout time.Duration
	lnd            *lndpeermsg.Config
}

type envConfig struct {
	LogLevel         string `env:"LCPD_LOG_LEVEL"`
	Backend          string `env:"LCPD_BACKEND"`
	OpenAIAPIKey     string `env:"LCPD_OPENAI_API_KEY"`
	OpenAIAPIKeyV0   string `env:"OPENAI_API_KEY"`
	OpenAIBaseURL    string `env:"LCPD_OPENAI_BASE_URL"`
	DeterministicB64 string `env:"LCPD_DETERMINISTIC_OUTPUT_BASE64"`

	ProviderConfigPath string `env:"LCPD_PROVIDER_CONFIG_PATH"`
}

func main() {
	os.Exit(realMain())
}

func realMain() int {
	cfg, err := parseFlags(os.Args[1:], os.Stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		fmt.Fprintln(os.Stderr, err)
		return exitError
	}

	env, err := parseEnvConfig(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitError
	}

	baseLogger, err := newLogger(env.LogLevel)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitError
	}
	defer func() { _ = baseLogger.Sync() }()

	err = run(cfg, env, baseLogger)
	if err != nil {
		baseLogger.Error("lcpd-grpcd failed", zap.Error(err))
		return exitError
	}
	return exitOK
}

func parseEnvConfig(ctx context.Context) (envConfig, error) {
	var env envConfig
	if err := envconfig.Process(ctx, &env); err != nil {
		return envConfig{}, fmt.Errorf("process env: %w", err)
	}
	env.LogLevel = strings.TrimSpace(env.LogLevel)
	env.Backend = strings.TrimSpace(env.Backend)
	env.OpenAIAPIKey = strings.TrimSpace(env.OpenAIAPIKey)
	env.OpenAIAPIKeyV0 = strings.TrimSpace(env.OpenAIAPIKeyV0)
	env.OpenAIBaseURL = strings.TrimSpace(env.OpenAIBaseURL)
	env.DeterministicB64 = strings.TrimSpace(env.DeterministicB64)
	env.ProviderConfigPath = strings.TrimSpace(env.ProviderConfigPath)
	if env.ProviderConfigPath == "" {
		if info, err := os.Stat("config.yaml"); err == nil && !info.IsDir() {
			env.ProviderConfigPath = "config.yaml"
		}
	}
	return env, nil
}

func parseFlags(args []string, out io.Writer) (config, error) {
	fs := flag.NewFlagSet("lcpd-grpcd", flag.ContinueOnError)
	fs.SetOutput(out)

	grpcAddr := fs.String("grpc_addr", "127.0.0.1:50051", "gRPC listen address")
	startupTimeout := fs.Duration(
		"startup_timeout",
		defaultStartupTimeout,
		"fx startup timeout (e.g. 20s)",
	)

	lndRPCAddr := fs.String(
		"lnd_rpc_addr",
		"",
		"lnd gRPC address host:port (or env LCPD_LND_RPC_ADDR)",
	)
	lndTLSCertPath := fs.String(
		"lnd_tls_cert_path",
		"",
		"path to lnd tls.cert (or env LCPD_LND_TLS_CERT_PATH)",
	)
	lndAdminMacaroonPath := fs.String(
		"lnd_admin_macaroon_path",
		"",
		"path to lnd admin.macaroon (or env LCPD_LND_ADMIN_MACAROON_PATH)",
	)
	lndMacaroonPath := fs.String("lnd_macaroon_path", "", "alias of -lnd_admin_macaroon_path")

	if err := fs.Parse(args); err != nil {
		return config{}, err
	}

	timeout := *startupTimeout
	if timeout <= 0 {
		return config{}, fmt.Errorf("startup_timeout must be > 0, got %s", timeout)
	}

	lndCfg, err := lndConfigFromFlagsOrEnv(
		*lndRPCAddr,
		*lndTLSCertPath,
		*lndAdminMacaroonPath,
		*lndMacaroonPath,
	)
	if err != nil {
		if errors.Is(err, lndpeermsg.ErrLNDConnectionNotConfigured) {
			lndCfg = nil
		} else {
			return config{}, err
		}
	}

	return config{
		grpcAddr:       *grpcAddr,
		startupTimeout: timeout,
		lnd:            lndCfg,
	}, nil
}

func run(cfg config, env envConfig, baseLogger *zap.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	logger := baseLogger.Sugar().With("component", "lcpd-grpcd")

	grpcLis, err := listenGRPC(ctx, cfg.grpcAddr)
	if err != nil {
		return err
	}
	defer func() { _ = grpcLis.Close() }()

	backend, err := computeBackendFromEnvConfig(env)
	if err != nil {
		return err
	}

	providerCfg, llmPolicy, localManifest, err := providerRuntime(logger, env, backend, cfg.lnd)
	if err != nil {
		return err
	}

	logger.Infow("compute backend", "type", fmt.Sprintf("%T", backend))
	logger.Infow("llm policy", "max_output_tokens", llmPolicy.Policy().MaxOutputTokens)

	app := fx.New(
		fxOptions(
			baseLogger,
			logger,
			backend,
			grpcLis,
			cfg.lnd,
			localManifest,
			providerCfg,
			llmPolicy,
		)...)
	return startAndWait(ctx, app, cfg.startupTimeout, logger, cfg.grpcAddr)
}

func listenGRPC(ctx context.Context, grpcAddr string) (net.Listener, error) {
	listenCfg := &net.ListenConfig{}
	grpcLis, err := listenCfg.Listen(ctx, "tcp", grpcAddr)
	if err != nil {
		return nil, fmt.Errorf("grpc listen %s: %w", grpcAddr, err)
	}
	return grpcLis, nil
}

func providerRuntime(
	logger *zap.SugaredLogger,
	env envConfig,
	backend computebackend.Backend,
	lndCfg *lndpeermsg.Config,
) (provider.Config, llm.ExecutionPolicyProvider, *lcpwire.Manifest, error) {
	providerYAML, err := loadProviderYAML(env.ProviderConfigPath)
	if err != nil {
		return provider.Config{}, nil, nil, err
	}

	maxOutputTokens := llm.DefaultMaxOutputTokens
	if providerYAML.LLM.MaxOutputTokens != nil {
		maxOutputTokens = *providerYAML.LLM.MaxOutputTokens
	}

	llmPolicy, err := llmPolicyFromConfig(maxOutputTokens)
	if err != nil {
		return provider.Config{}, nil, nil, err
	}

	chatProfiles, err := resolveProviderLLMChatProfiles(logger, providerYAML)
	if err != nil {
		return provider.Config{}, nil, nil, err
	}

	providerCfg := provider.Config{
		Enabled:         resolveProviderEnabled(providerYAML.Enabled),
		QuoteTTLSeconds: resolveQuoteTTL(providerYAML.QuoteTTLSeconds),
		Pricing: provider.PricingConfig{
			InFlightSurge: provider.InFlightSurgeConfig{
				Threshold:        providerYAML.Pricing.InFlightSurge.Threshold,
				PerJobBps:        providerYAML.Pricing.InFlightSurge.PerJobBps,
				MaxMultiplierBps: providerYAML.Pricing.InFlightSurge.MaxMultiplierBps,
			},
		},
		LLMChatProfiles: chatProfiles,
	}
	if cfgErr := validateProviderConfig(logger, providerCfg, backend, lndCfg); cfgErr != nil {
		return provider.Config{}, nil, nil, cfgErr
	}

	return providerCfg, llmPolicy, localManifestForProvider(providerCfg), nil
}

func validateProviderConfig(
	logger *zap.SugaredLogger,
	providerCfg provider.Config,
	backend computebackend.Backend,
	lndCfg *lndpeermsg.Config,
) error {
	if !providerCfg.Enabled {
		return nil
	}

	if lndCfg == nil {
		return errors.New(
			"provider enabled but lnd connection is not configured (set LCPD_LND_RPC_ADDR)",
		)
	}
	if _, ok := backend.(*computebackend.Disabled); ok {
		return errors.New(
			"provider enabled but compute backend is disabled (set LCPD_BACKEND or disable provider via LCPD_PROVIDER_ENABLED=0)",
		)
	}
	if providerCfg.Pricing.InFlightSurge.PerJobBps > 0 &&
		providerCfg.Pricing.InFlightSurge.MaxMultiplierBps != 0 &&
		providerCfg.Pricing.InFlightSurge.MaxMultiplierBps < 10_000 {
		return fmt.Errorf(
			"pricing.in_flight_surge.max_multiplier_bps must be >= 10000, got %d",
			providerCfg.Pricing.InFlightSurge.MaxMultiplierBps,
		)
	}
	if len(providerCfg.LLMChatProfiles) == 0 {
		logger.Warnw(
			"provider enabled but no supported profiles configured; lcp_manifest will not advertise profiles",
			"hint",
			"set llm.chat_profiles in the provider YAML",
		)
	}
	return nil
}

func fxOptions(
	baseLogger *zap.Logger,
	logger *zap.SugaredLogger,
	backend computebackend.Backend,
	grpcLis net.Listener,
	lndCfg *lndpeermsg.Config,
	localManifest *lcpwire.Manifest,
	providerCfg provider.Config,
	llmPolicy llm.ExecutionPolicyProvider,
) []fx.Option {
	return append(
		baseFXOptions(baseLogger, logger, backend, grpcLis, lndCfg),
		fx.Supply(localManifest),
		fx.Supply(providerCfg),
		fx.Provide(func() llm.ExecutionPolicyProvider { return llmPolicy }),
		fx.Provide(func() llm.UsageEstimator { return llm.NewApproxUsageEstimator() }),
	)
}

func startAndWait(
	ctx context.Context,
	app *fx.App,
	startupTimeout time.Duration,
	logger *zap.SugaredLogger,
	grpcAddr string,
) error {
	startCtx, startCancel := context.WithTimeout(ctx, startupTimeout)
	defer startCancel()

	if err := app.Start(startCtx); err != nil {
		return fmt.Errorf("fx start: %w", err)
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), defaultShutdownTimeout)
		defer stopCancel()
		_ = app.Stop(stopCtx)
	}()

	logger.Infow("lcpd gRPC listening", "addr", grpcAddr)
	logger.Info("ready")
	<-ctx.Done()

	if errors.Is(ctx.Err(), context.Canceled) {
		return nil
	}
	return ctx.Err()
}

type providerYAMLConfig struct {
	Enabled                  *bool             `yaml:"enabled"`
	QuoteTTLSeconds          *uint64           `yaml:"quote_ttl_seconds"`
	SupportedLLMChatProfiles []string          `yaml:"supported_llm_chat_profiles"` // deprecated (use llm.chat_profiles)
	Pricing                  pricingYAMLConfig `yaml:"pricing"`
	LLM                      llmYAMLConfig     `yaml:"llm"`
}

type pricingYAMLConfig struct {
	InFlightSurge inFlightSurgeYAMLConfig `yaml:"in_flight_surge"`
}

type inFlightSurgeYAMLConfig struct {
	Threshold        uint32 `yaml:"threshold"`
	PerJobBps        uint32 `yaml:"per_job_bps"`
	MaxMultiplierBps uint32 `yaml:"max_multiplier_bps"`
}

type llmYAMLConfig struct {
	MaxOutputTokens *uint32                        `yaml:"max_output_tokens"`
	PriceTable      map[string]priceTableEntryYAML `yaml:"price_table"` // deprecated (use llm.chat_profiles.*.price)
	ChatProfiles    map[string]llmChatProfileYAML  `yaml:"chat_profiles"`
}

type llmChatProfileYAML struct {
	Profile         string               `yaml:"profile"`
	BackendModel    string               `yaml:"backend_model"`
	MaxOutputTokens *uint32              `yaml:"max_output_tokens"`
	Price           *priceTableEntryYAML `yaml:"price"`
	OpenAI          openAIProfileYAML    `yaml:"openai"`
}

type openAIProfileYAML struct {
	Temperature      *float64 `yaml:"temperature"`
	TopP             *float64 `yaml:"top_p"`
	Stop             []string `yaml:"stop"`
	PresencePenalty  *float64 `yaml:"presence_penalty"`
	FrequencyPenalty *float64 `yaml:"frequency_penalty"`
	Seed             *int64   `yaml:"seed"`
}

type priceTableEntryYAML struct {
	InputMsatPerMTok       uint64  `yaml:"input_msat_per_mtok"`
	CachedInputMsatPerMTok *uint64 `yaml:"cached_input_msat_per_mtok"`
	OutputMsatPerMTok      uint64  `yaml:"output_msat_per_mtok"`
}

func loadProviderYAML(path string) (providerYAMLConfig, error) {
	if strings.TrimSpace(path) == "" {
		return providerYAMLConfig{}, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return providerYAMLConfig{}, fmt.Errorf("read provider config %q: %w", path, err)
	}

	var cfg providerYAMLConfig
	unmarshalErr := yaml.Unmarshal(data, &cfg)
	if unmarshalErr != nil {
		return providerYAMLConfig{}, fmt.Errorf("parse provider config %q: %w", path, unmarshalErr)
	}

	return cfg, nil
}

func (c providerYAMLConfig) priceTable() (llm.PriceTable, error) {
	if len(c.LLM.PriceTable) == 0 {
		return llm.PriceTable{}, nil
	}

	table := make(llm.PriceTable, len(c.LLM.PriceTable))
	for model, entry := range c.LLM.PriceTable {
		if entry.InputMsatPerMTok == 0 || entry.OutputMsatPerMTok == 0 {
			return nil, fmt.Errorf("llm.price_table[%s] must set input/output prices", model)
		}
		table[model] = llm.PriceTableEntry{
			Model:                  model,
			InputMsatPerMTok:       entry.InputMsatPerMTok,
			CachedInputMsatPerMTok: entry.CachedInputMsatPerMTok,
			OutputMsatPerMTok:      entry.OutputMsatPerMTok,
		}
	}
	return table, nil
}

func resolveProviderLLMChatProfiles(
	logger *zap.SugaredLogger,
	cfg providerYAMLConfig,
) (map[string]provider.LLMChatProfile, error) {
	defaultPriceTable := llm.DefaultPriceTable()

	if len(cfg.LLM.ChatProfiles) > 0 {
		if logger != nil && len(cfg.SupportedLLMChatProfiles) > 0 {
			logger.Warnw(
				"provider YAML uses deprecated supported_llm_chat_profiles; ignoring because llm.chat_profiles is set",
				"hint", "remove supported_llm_chat_profiles and configure llm.chat_profiles only",
			)
		}

		legacyPriceTable, err := cfg.priceTable()
		if err != nil {
			return nil, err
		}
		if logger != nil && len(legacyPriceTable) > 0 {
			logger.Warnw(
				"provider YAML uses deprecated llm.price_table; using it as a fallback for llm.chat_profiles.*.price",
				"hint", "move pricing under llm.chat_profiles.*.price",
			)
		}

		return chatProfilesFromYAML(cfg.LLM.ChatProfiles, legacyPriceTable, defaultPriceTable)
	}

	if len(cfg.SupportedLLMChatProfiles) == 0 {
		if len(cfg.LLM.PriceTable) > 0 {
			return nil, errors.New(
				"llm.price_table is set but no supported profiles are configured; migrate to llm.chat_profiles",
			)
		}
		return map[string]provider.LLMChatProfile{}, nil
	}

	legacyPriceTable, err := cfg.priceTable()
	if err != nil {
		return nil, err
	}
	if logger != nil {
		logger.Warnw(
			"provider YAML uses deprecated supported_llm_chat_profiles/llm.price_table; migrate to llm.chat_profiles",
			"hint", "define llm.chat_profiles.<profile>.price and remove supported_llm_chat_profiles",
		)
	}

	return chatProfilesFromLegacy(cfg.SupportedLLMChatProfiles, legacyPriceTable, defaultPriceTable)
}

func chatProfilesFromYAML(
	cfg map[string]llmChatProfileYAML,
	legacyPriceTable llm.PriceTable,
	defaultPriceTable llm.PriceTable,
) (map[string]provider.LLMChatProfile, error) {
	profiles := make(map[string]provider.LLMChatProfile, len(cfg))

	for key, p := range cfg {
		profile := strings.TrimSpace(p.Profile)
		if profile == "" {
			profile = strings.TrimSpace(key)
		}
		if profile == "" {
			return nil, errors.New("llm.chat_profiles key/profile must be non-empty")
		}
		if _, exists := profiles[profile]; exists {
			return nil, fmt.Errorf("llm.chat_profiles defines duplicate profile %q", profile)
		}

		backendModel := strings.TrimSpace(p.BackendModel)
		if backendModel == "" {
			backendModel = profile
		}

		maxOutputTokens := p.MaxOutputTokens
		if maxOutputTokens != nil && *maxOutputTokens == 0 {
			return nil, fmt.Errorf("llm.chat_profiles[%s].max_output_tokens must be > 0", profile)
		}

		price, err := resolveProfilePrice(profile, p.Price, legacyPriceTable, defaultPriceTable)
		if err != nil {
			return nil, err
		}

		profiles[profile] = provider.LLMChatProfile{
			BackendModel:    backendModel,
			MaxOutputTokens: maxOutputTokens,
			Price:           price,
			OpenAI: provider.OpenAIChatParams{
				Temperature:      p.OpenAI.Temperature,
				TopP:             p.OpenAI.TopP,
				Stop:             append([]string(nil), p.OpenAI.Stop...),
				PresencePenalty:  p.OpenAI.PresencePenalty,
				FrequencyPenalty: p.OpenAI.FrequencyPenalty,
				Seed:             p.OpenAI.Seed,
			},
		}
	}

	return profiles, nil
}

func chatProfilesFromLegacy(
	supportedProfiles []string,
	legacyPriceTable llm.PriceTable,
	defaultPriceTable llm.PriceTable,
) (map[string]provider.LLMChatProfile, error) {
	profiles := make(map[string]provider.LLMChatProfile, len(supportedProfiles))

	for _, raw := range supportedProfiles {
		profile := strings.TrimSpace(raw)
		if profile == "" {
			return nil, errors.New("supported_llm_chat_profiles contains an empty profile")
		}
		if _, exists := profiles[profile]; exists {
			return nil, fmt.Errorf("supported_llm_chat_profiles contains duplicate profile %q", profile)
		}

		price, err := resolveProfilePrice(profile, nil, legacyPriceTable, defaultPriceTable)
		if err != nil {
			return nil, err
		}

		profiles[profile] = provider.LLMChatProfile{
			BackendModel: profile,
			Price:        price,
		}
	}

	return profiles, nil
}

func resolveProfilePrice(
	profile string,
	explicit *priceTableEntryYAML,
	legacyPriceTable llm.PriceTable,
	defaultPriceTable llm.PriceTable,
) (llm.PriceTableEntry, error) {
	if explicit != nil {
		if explicit.InputMsatPerMTok == 0 || explicit.OutputMsatPerMTok == 0 {
			return llm.PriceTableEntry{}, fmt.Errorf(
				"llm.chat_profiles[%s].price must set input/output prices",
				profile,
			)
		}
		return llm.PriceTableEntry{
			Model:                  profile,
			InputMsatPerMTok:       explicit.InputMsatPerMTok,
			CachedInputMsatPerMTok: explicit.CachedInputMsatPerMTok,
			OutputMsatPerMTok:      explicit.OutputMsatPerMTok,
		}, nil
	}

	if legacyPriceTable != nil {
		if entry, ok := legacyPriceTable[profile]; ok {
			return entry, nil
		}
	}

	if defaultPriceTable != nil {
		if entry, ok := defaultPriceTable[profile]; ok {
			return entry, nil
		}
	}

	return llm.PriceTableEntry{}, fmt.Errorf(
		"no price configured for profile %q (set llm.chat_profiles.%s.price)",
		profile,
		profile,
	)
}

func resolveProviderEnabled(enabled *bool) bool {
	if enabled != nil {
		return *enabled
	}
	return false
}

func resolveQuoteTTL(ttl *uint64) uint64 {
	if ttl != nil {
		return *ttl
	}
	return 0
}

func localManifestForProvider(providerCfg provider.Config) *lcpwire.Manifest {
	manifest := &lcpwire.Manifest{
		ProtocolVersion: lcpwire.ProtocolVersionV02,
		MaxPayloadBytes: defaultMaxPayloadBytes,
		MaxStreamBytes:  defaultMaxStreamBytes,
		MaxJobBytes:     defaultMaxJobBytes,
	}

	if !providerCfg.Enabled || len(providerCfg.LLMChatProfiles) == 0 {
		return manifest
	}

	profiles := make([]string, 0, len(providerCfg.LLMChatProfiles))
	for profile := range providerCfg.LLMChatProfiles {
		profiles = append(profiles, profile)
	}
	slices.Sort(profiles)

	manifest.SupportedTasks = make(
		[]lcpwire.TaskTemplate,
		0,
		len(profiles),
	)
	for _, profile := range profiles {
		manifest.SupportedTasks = append(manifest.SupportedTasks, lcpwire.TaskTemplate{
			TaskKind:      "llm.chat",
			LLMChatParams: &lcpwire.LLMChatParams{Profile: profile},
		})
	}
	return manifest
}

func baseFXOptions(
	baseLogger *zap.Logger,
	logger *zap.SugaredLogger,
	backend computebackend.Backend,
	grpcLis net.Listener,
	lndCfg *lndpeermsg.Config,
) []fx.Option {
	commonProvides := fx.Provide(
		func(l grpcListener) net.Listener { return l.Listener },
		peerdirectory.New,
		lndpeermsg.New,
		lcpd.New,
		grpcserver.New,
		jobstore.New,
		requesterjobstore.New,
		requesterwait.New,
		func() *replaystore.Store { return replaystore.New(0) },
	)

	opts := []fx.Option{
		fx.Supply(baseLogger, logger),
		fx.Provide(func() computebackend.Backend { return backend }),
		fx.Supply(grpcListener{Listener: grpcLis}),
		commonProvides,
		fx.WithLogger(func() fxevent.Logger { return &fxevent.ZapLogger{Logger: baseLogger} }),
		fx.Invoke(func(*grpc.Server, *lndpeermsg.PeerMessaging) {}),
	}

	if lndCfg != nil {
		opts = append(opts,
			fx.Supply(lndCfg),
			fx.Provide(
				provider.DefaultValidator,
				provideLNDAdapter,
				provideProviderMessenger,
				provideProviderInvoiceCreator,
				providePeerMessenger,
				fx.Annotate(provideLightningRPC, fx.As(new(lcpd.LightningRPC))),
				provider.NewHandler,
				fx.Annotate(inbounddispatch.New, fx.As(new(lndpeermsg.InboundMessageHandler))),
			),
		)
	} else {
		opts = append(opts,
			fx.Provide(
				func() lndpeermsg.InboundMessageHandler {
					return lndpeermsg.InboundMessageHandlerFunc(func(context.Context, lndpeermsg.InboundCustomMessage) {})
				},
			),
		)
	}

	return opts
}

type lndAdapterParams struct {
	fx.In

	Lifecycle fx.Lifecycle
	Config    *lndpeermsg.Config `optional:"true"`
	Logger    *zap.SugaredLogger `optional:"true"`
}

func provideLNDAdapter(p lndAdapterParams) (*provider.LNDAdapter, error) {
	adapter, err := provider.NewLNDAdapter(context.Background(), p.Config, p.Logger)
	if err != nil {
		return nil, err
	}

	p.Lifecycle.Append(fx.Hook{
		OnStop: func(_ context.Context) error {
			return adapter.Close()
		},
	})
	return adapter, nil
}

func provideProviderMessenger(adapter *provider.LNDAdapter) provider.Messenger {
	return adapter
}

func provideProviderInvoiceCreator(adapter *provider.LNDAdapter) provider.InvoiceCreator {
	return adapter
}

func providePeerMessenger(adapter *provider.LNDAdapter) lcpd.PeerMessenger {
	return adapter
}

type lightningRPCParams struct {
	fx.In

	Lifecycle fx.Lifecycle
	Config    *lndpeermsg.Config `optional:"true"`
	Logger    *zap.SugaredLogger `optional:"true"`
}

func provideLightningRPC(p lightningRPCParams) (*lightningrpc.Client, error) {
	client, err := lightningrpc.New(context.Background(), p.Config, p.Logger)
	if err != nil {
		return nil, err
	}

	p.Lifecycle.Append(fx.Hook{
		OnStop: func(_ context.Context) error {
			return client.Close()
		},
	})
	return client, nil
}

func parseLogLevel(s string) zapcore.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return zapcore.DebugLevel
	case "warn", "warning":
		return zapcore.WarnLevel
	case "error":
		return zapcore.ErrorLevel
	default:
		return zapcore.InfoLevel
	}
}

func newLogger(level string) (*zap.Logger, error) {
	cfg := zap.NewProductionConfig()
	cfg.Level = zap.NewAtomicLevelAt(parseLogLevel(level))
	cfg.Encoding = "console"
	cfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	return cfg.Build()
}

func computeBackendFromEnvConfig(env envConfig) (computebackend.Backend, error) {
	switch strings.ToLower(strings.TrimSpace(env.Backend)) {
	case "", "none", "disabled":
		return computebackend.NewDisabled(), nil
	case "openai":
		apiKey := strings.TrimSpace(env.OpenAIAPIKey)
		if apiKey == "" {
			apiKey = strings.TrimSpace(env.OpenAIAPIKeyV0)
		}
		return openaibackend.New(openaibackend.Config{
			APIKey:  apiKey,
			BaseURL: strings.TrimSpace(env.OpenAIBaseURL),
		})
	case "deterministic":
		if env.DeterministicB64 != "" {
			return computebackend.NewDeterministicFromBase64(env.DeterministicB64)
		}
		return computebackend.NewDeterministic([]byte("deterministic-output")), nil
	default:
		return nil, fmt.Errorf(
			"unknown LCPD_BACKEND: %q (supported: disabled, deterministic, openai)",
			env.Backend,
		)
	}
}

func llmPolicyFromConfig(maxOutputTokens uint32) (llm.ExecutionPolicyProvider, error) {
	if maxOutputTokens == 0 {
		maxOutputTokens = llm.DefaultMaxOutputTokens
	}
	return llm.NewFixedExecutionPolicy(maxOutputTokens)
}

func lndConfigFromFlagsOrEnv(
	lndRPCAddr string,
	lndTLSCertPath string,
	lndAdminMacaroonPath string,
	lndMacaroonPath string,
) (*lndpeermsg.Config, error) {
	rpcAddr := strings.TrimSpace(lndRPCAddr)
	tlsCertPath := strings.TrimSpace(lndTLSCertPath)
	adminMacaroonPath := strings.TrimSpace(lndAdminMacaroonPath)
	macaroonPath := strings.TrimSpace(lndMacaroonPath)

	switch {
	case rpcAddr == "" && tlsCertPath == "" && adminMacaroonPath == "" && macaroonPath == "":
		return lndpeermsg.NewConfigFromEnv(context.Background())
	case adminMacaroonPath == "" && macaroonPath != "":
		adminMacaroonPath = macaroonPath
	}

	if rpcAddr == "" {
		return nil, errors.New("lnd_rpc_addr is required when lnd flags are set")
	}

	return &lndpeermsg.Config{
		RPCAddr:           rpcAddr,
		TLSCertPath:       tlsCertPath,
		AdminMacaroonPath: adminMacaroonPath,
	}, nil
}
