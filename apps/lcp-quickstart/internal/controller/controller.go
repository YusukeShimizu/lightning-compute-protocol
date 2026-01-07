package controller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bruwbird/lcp/apps/lcp-quickstart/internal/build"
	"github.com/bruwbird/lcp/apps/lcp-quickstart/internal/lncli"
	"github.com/bruwbird/lcp/apps/lcp-quickstart/internal/model"
	"github.com/bruwbird/lcp/apps/lcp-quickstart/internal/openai"
	"github.com/bruwbird/lcp/apps/lcp-quickstart/internal/proc"
	"github.com/bruwbird/lcp/apps/lcp-quickstart/internal/state"
	"github.com/bruwbird/lcp/apps/lcp-quickstart/internal/workspace"
)

const (
	ComponentLND         = "lnd"
	ComponentLCPDGRPCD   = "lcpd-grpcd"
	ComponentOpenAIServe = "openai-serve"
)

const (
	defaultLNDGRPCAddr     = "localhost:10009"
	defaultLCPDGRPCAddr    = "127.0.0.1:50051"
	defaultOpenAIHTTPAddr  = "127.0.0.1:8080"
	defaultOpenAIAPIKeys   = "lcp-dev"
	defaultMainnetMaxPrice = uint64(100_000)
)

const (
	lncliTimeout       = 10 * time.Second
	openChannelTimeout = 2 * time.Minute
	buildTimeout       = 2 * time.Minute
	healthzTimeout     = 3 * time.Second
)

type Controller struct {
	Workspace workspace.Workspace
	RepoRoot  string
	LNCLI     lncli.Client
}

type UpOptions struct {
	Network model.Network

	Provider string

	StartLND    bool
	OpenChannel bool
	ChannelSats int64

	IUnderstandMainnet        bool
	IUnderstandExposingOpenAI bool

	LCPDGRPCAddr string

	LNDRPCAddr           string
	LNDTLSCertPath       string
	LNDAdminMacaroonPath string

	OpenAIHTTPAddr     string
	OpenAIAPIKeysCSV   string
	OpenAIMaxPriceMsat uint64
	OpenAIMaxPriceSet  bool
}

func (c Controller) Up(ctx context.Context, opts UpOptions) error {
	if opts.Network == model.NetworkMainnet && !opts.IUnderstandMainnet {
		return errors.New("mainnet requires --i-understand-mainnet")
	}
	if strings.TrimSpace(c.RepoRoot) == "" {
		return errors.New("repo root not set; run from within the repo or pass --repo-root")
	}
	if err := c.Workspace.Ensure(); err != nil {
		return err
	}

	providerNode, providerPeerID, err := c.resolveProvider(opts.Network, opts.Provider)
	if err != nil {
		return err
	}

	_, _ = state.Update(c.Workspace.StatePath, func(st model.State) model.State {
		st.EnsureDefaults()
		st.Network = opts.Network
		st.ProviderNode = providerNode
		st.ProviderPeerID = providerPeerID
		st.Readiness = model.Readiness{}
		for k, v := range st.Components {
			v.LastError = ""
			st.Components[k] = v
		}
		return st
	})

	infoJSON, err := c.ensureLNDReady(ctx, opts.Network, opts.StartLND)
	if err != nil {
		c.recordComponentError(ComponentLND, err.Error())
		walletState := c.LNCLI.DetectWalletState(ctx, opts.Network)
		switch walletState {
		case lncli.WalletUninitialized:
			return fmt.Errorf("lnd wallet is uninitialized; run: %s create", lncliCommandPrefix(opts.Network))
		case lncli.WalletLocked:
			return fmt.Errorf("lnd wallet is locked; run: %s unlock", lncliCommandPrefix(opts.Network))
		default:
			if strings.Contains(strings.ToLower(err.Error()), "lncli not found") {
				return err
			}
			if !opts.StartLND {
				return fmt.Errorf("lnd not ready; start lnd or re-run with --start-lnd: %w", err)
			}
			return err
		}
	}
	if err := c.LNCLI.ValidateNetwork(opts.Network, infoJSON); err != nil {
		return err
	}
	c.recordReadiness(func(r *model.Readiness) { r.LNDReady = true })

	ctxConnect, cancelConnect := context.WithTimeout(ctx, lncliTimeout)
	defer cancelConnect()
	if err := c.LNCLI.ConnectPeer(ctxConnect, opts.Network, providerNode); err != nil {
		c.recordComponentError(ComponentLND, err.Error())
		return err
	}
	c.recordReadiness(func(r *model.Readiness) { r.ProviderConnected = true })

	ctxLiquidity, cancelLiquidity := context.WithTimeout(ctx, lncliTimeout)
	defer cancelLiquidity()
	hasLiquidity, err := c.LNCLI.HasOutboundLiquidity(ctxLiquidity, opts.Network, providerPeerID, 1)
	if err != nil {
		c.recordComponentError(ComponentLND, err.Error())
		return err
	}
	if !hasLiquidity {
		if !opts.OpenChannel {
			return errors.New("channel/route required; rerun with --open-channel --channel-sats <sats>")
		}
		if opts.ChannelSats <= 0 {
			return errors.New("--channel-sats is required with --open-channel")
		}
		ctxChan, cancelChan := context.WithTimeout(ctx, openChannelTimeout)
		defer cancelChan()
		if err := c.LNCLI.OpenChannel(ctxChan, opts.Network, providerPeerID, opts.ChannelSats); err != nil {
			c.recordComponentError(ComponentLND, err.Error())
			return err
		}
		return errors.New("channel opening initiated; wait for confirmations then rerun `up`")
	}
	c.recordReadiness(func(r *model.Readiness) { r.ChannelReady = true })

	lcpdGRPCAddr := defaultIfBlank(opts.LCPDGRPCAddr, defaultLCPDGRPCAddr)
	openAIHTTPAddr := defaultIfBlank(opts.OpenAIHTTPAddr, defaultOpenAIHTTPAddr)
	openAIAPIKeys := strings.TrimSpace(opts.OpenAIAPIKeysCSV)

	if err := requireLoopbackListenAddr("lcpd-grpc-addr", lcpdGRPCAddr); err != nil {
		return err
	}
	if err := validateOpenAIServeHTTPAddr(openAIHTTPAddr, openAIAPIKeys, opts.IUnderstandExposingOpenAI); err != nil {
		return err
	}

	openAIMaxPrice := opts.OpenAIMaxPriceMsat
	if !opts.OpenAIMaxPriceSet {
		if opts.Network == model.NetworkMainnet {
			openAIMaxPrice = defaultMainnetMaxPrice
		} else {
			openAIMaxPrice = 0
		}
	}

	lndRPCAddr := defaultIfBlank(opts.LNDRPCAddr, defaultLNDGRPCAddr)
	lndTLSCert := expandTilde(defaultIfBlank(opts.LNDTLSCertPath, defaultLndTLSCertPath()))
	lndMacaroon := expandTilde(defaultIfBlank(opts.LNDAdminMacaroonPath, defaultLndAdminMacaroonPath(opts.Network)))

	if err := c.ensureLCPDGRPCD(ctx, lcpdGRPCAddr, lndRPCAddr, lndTLSCert, lndMacaroon); err != nil {
		c.recordComponentError(ComponentLCPDGRPCD, err.Error())
		return err
	}
	c.recordReadiness(func(r *model.Readiness) { r.LCPDReady = true })

	if err := c.ensureOpenAIServe(ctx, openAIHTTPAddr, lcpdGRPCAddr, openAIAPIKeys, providerPeerID, openAIMaxPrice); err != nil {
		c.recordComponentError(ComponentOpenAIServe, err.Error())
		return err
	}

	ctxHealth, cancelHealth := context.WithTimeout(ctx, healthzTimeout)
	defer cancelHealth()
	if err := openai.Healthz(ctxHealth, openAIHTTPAddr); err != nil {
		c.recordComponentError(ComponentOpenAIServe, err.Error())
		return fmt.Errorf("openai-serve healthz failed: %w", err)
	}
	c.recordReadiness(func(r *model.Readiness) { r.OpenAIReady = true })

	return nil
}

func (c Controller) Down(ctx context.Context, network model.Network) error {
	_ = ctx
	if err := c.Workspace.Ensure(); err != nil {
		return err
	}

	var errs []error

	if err := proc.Stop(c.Workspace.PIDPath(ComponentOpenAIServe)); err != nil {
		errs = append(errs, err)
	}
	if err := proc.Stop(c.Workspace.PIDPath(ComponentLCPDGRPCD)); err != nil {
		errs = append(errs, err)
	}

	st, err := state.Read(c.Workspace.StatePath)
	if err == nil && st.Components != nil {
		if lnd, ok := st.Components[ComponentLND]; ok && lnd.Managed {
			if err := proc.Stop(c.Workspace.PIDPath(ComponentLND)); err != nil {
				errs = append(errs, err)
			}
		}
	}

	_, _ = state.Update(c.Workspace.StatePath, func(st model.State) model.State {
		st.EnsureDefaults()
		st.Network = network
		for _, component := range []string{ComponentOpenAIServe, ComponentLCPDGRPCD, ComponentLND} {
			cs := st.Components[component]
			cs.Running = false
			st.Components[component] = cs
		}
		st.Readiness.OpenAIReady = false
		st.Readiness.LCPDReady = false
		return st
	})

	return errors.Join(errs...)
}

func (c Controller) Status(ctx context.Context, network model.Network) (model.State, error) {
	if err := c.Workspace.Ensure(); err != nil {
		return model.State{}, err
	}

	st, err := state.Read(c.Workspace.StatePath)
	if err != nil {
		return model.State{}, err
	}
	st.EnsureDefaults()
	st.Network = network

	var lndErr error
	ctxInfo, cancelInfo := context.WithTimeout(ctx, lncliTimeout)
	defer cancelInfo()
	infoJSON, err := c.LNCLI.GetInfo(ctxInfo, network)
	if err != nil {
		lndErr = err
		st.Readiness.LNDReady = false
	} else if err := c.LNCLI.ValidateNetwork(network, infoJSON); err != nil {
		lndErr = err
		st.Readiness.LNDReady = false
	} else {
		st.Readiness.LNDReady = true
	}

	if st.Readiness.LNDReady {
		if strings.TrimSpace(string(st.ProviderPeerID)) != "" {
			ctxPeers, cancelPeers := context.WithTimeout(ctx, lncliTimeout)
			connected, err := c.LNCLI.IsPeerConnected(ctxPeers, network, st.ProviderPeerID)
			cancelPeers()
			if err == nil {
				st.Readiness.ProviderConnected = connected
			}

			ctxLiquidity, cancelLiquidity := context.WithTimeout(ctx, lncliTimeout)
			hasLiquidity, err := c.LNCLI.HasOutboundLiquidity(ctxLiquidity, network, st.ProviderPeerID, 1)
			cancelLiquidity()
			if err == nil {
				st.Readiness.ChannelReady = hasLiquidity
			}
		}
	}

	openAIRunning, openAIPid := proc.IsRunning(c.Workspace.PIDPath(ComponentOpenAIServe))
	lcpdRunning, lcpdPid := proc.IsRunning(c.Workspace.PIDPath(ComponentLCPDGRPCD))
	lndManagedRunning, lndPid := proc.IsRunning(c.Workspace.PIDPath(ComponentLND))

	st.Components[ComponentOpenAIServe] = model.ComponentState{
		Running: openAIRunning,
		PID:     openAIPid,
		Addr:    st.Components[ComponentOpenAIServe].Addr,
		Managed: true,
	}
	st.Components[ComponentLCPDGRPCD] = model.ComponentState{
		Running: lcpdRunning,
		PID:     lcpdPid,
		Addr:    st.Components[ComponentLCPDGRPCD].Addr,
		Managed: true,
	}

	lndState := st.Components[ComponentLND]
	switch {
	case lndManagedRunning:
		lndState.Running = true
		lndState.PID = lndPid
		lndState.Managed = true
	case st.Readiness.LNDReady:
		lndState.Running = true
		lndState.PID = 0
		lndState.Managed = false
	default:
		lndState.Running = false
		lndState.PID = 0
		lndState.Managed = false
	}
	st.Components[ComponentLND] = lndState

	if openAIRunning && strings.TrimSpace(st.Components[ComponentOpenAIServe].Addr) != "" {
		ctxHealth, cancelHealth := context.WithTimeout(ctx, healthzTimeout)
		defer cancelHealth()
		if err := openai.Healthz(ctxHealth, st.Components[ComponentOpenAIServe].Addr); err == nil {
			st.Readiness.OpenAIReady = true
		}
	}
	st.Readiness.LCPDReady = lcpdRunning

	if lndErr != nil {
		st.Components[ComponentLND] = model.ComponentState{
			Running: lndState.Running,
			PID:     lndState.PID,
			Managed: lndState.Managed,
			LastError: func() string {
				return lndErr.Error()
			}(),
		}
	}

	_, _ = state.Update(c.Workspace.StatePath, func(_ model.State) model.State { return st })
	return st, nil
}

func (c Controller) Reset(ctx context.Context, network model.Network, force bool) error {
	_ = ctx
	if !force {
		return errors.New("reset is destructive; re-run with --force")
	}
	_ = c.Down(context.Background(), network)
	return removeAllGuarded(c.Workspace.DataDir)
}

func (c Controller) ensureLNDReady(ctx context.Context, network model.Network, startLND bool) ([]byte, error) {
	ctxInfo, cancelInfo := context.WithTimeout(ctx, lncliTimeout)
	defer cancelInfo()
	info, err := c.LNCLI.GetInfo(ctxInfo, network)
	if err == nil {
		return info, nil
	}
	if !startLND {
		return nil, err
	}

	// Best-effort "managed lnd": start it only if our pid file isn't already running.
	if running, pid := proc.IsRunning(c.Workspace.PIDPath(ComponentLND)); running {
		_, _ = state.Update(c.Workspace.StatePath, func(st model.State) model.State {
			st.EnsureDefaults()
			st.Components[ComponentLND] = model.ComponentState{Running: true, PID: pid, Managed: true}
			return st
		})
		return nil, err
	}

	args := []string{"--bitcoin.active"}
	if network == model.NetworkTestnet {
		args = append(args, "--bitcoin.testnet")
	}

	lndCmd := "lnd"
	if candidate := filepath.Join(c.Workspace.BinDir, "lnd"); fileExists(candidate) {
		lndCmd = candidate
	}

	pid, startErr := proc.Start(proc.Spec{
		Name:    ComponentLND,
		Command: lndCmd,
		Args:    args,
		LogPath: c.Workspace.LogPath(ComponentLND),
		PIDPath: c.Workspace.PIDPath(ComponentLND),
	})
	if startErr != nil {
		return nil, fmt.Errorf("failed to start lnd: %w", startErr)
	}

	_, _ = state.Update(c.Workspace.StatePath, func(st model.State) model.State {
		st.EnsureDefaults()
		st.Components[ComponentLND] = model.ComponentState{Running: true, PID: pid, Managed: true}
		return st
	})

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		ctxTry, cancelTry := context.WithTimeout(ctx, 3*time.Second)
		info, err := c.LNCLI.GetInfo(ctxTry, network)
		cancelTry()
		if err == nil {
			return info, nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return nil, fmt.Errorf("lnd did not become ready (see %s)", c.Workspace.LogPath(ComponentLND))
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func (c Controller) ensureLCPDGRPCD(ctx context.Context, grpcAddr, lndRPCAddr, lndTLSCertPath, lndAdminMacaroonPath string) error {
	pidPath := c.Workspace.PIDPath(ComponentLCPDGRPCD)
	if running, pid := proc.IsRunning(pidPath); running {
		_, _ = state.Update(c.Workspace.StatePath, func(st model.State) model.State {
			st.EnsureDefaults()
			st.Components[ComponentLCPDGRPCD] = model.ComponentState{
				Running: true,
				PID:     pid,
				Addr:    grpcAddr,
				Managed: true,
			}
			return st
		})
		return nil
	}

	binPath := filepath.Join(c.Workspace.BinDir, ComponentLCPDGRPCD)
	ctxBuild, cancelBuild := context.WithTimeout(ctx, buildTimeout)
	defer cancelBuild()
	if err := build.GoBinary(ctxBuild, filepath.Join(c.RepoRoot, "go-lcpd"), "./tools/lcpd-grpcd", binPath); err != nil {
		return err
	}

	pid, err := proc.Start(proc.Spec{
		Name:    ComponentLCPDGRPCD,
		Command: binPath,
		Args: []string{
			"-grpc_addr=" + grpcAddr,
			"-lnd_rpc_addr=" + lndRPCAddr,
			"-lnd_tls_cert_path=" + lndTLSCertPath,
			"-lnd_admin_macaroon_path=" + lndAdminMacaroonPath,
		},
		LogPath: c.Workspace.LogPath(ComponentLCPDGRPCD),
		PIDPath: pidPath,
	})
	if err != nil {
		return fmt.Errorf("failed to start lcpd-grpcd (see %s): %w", c.Workspace.LogPath(ComponentLCPDGRPCD), err)
	}

	_, _ = state.Update(c.Workspace.StatePath, func(st model.State) model.State {
		st.EnsureDefaults()
		st.Components[ComponentLCPDGRPCD] = model.ComponentState{
			Running: true,
			PID:     pid,
			Addr:    grpcAddr,
			Managed: true,
		}
		return st
	})
	return nil
}

func (c Controller) ensureOpenAIServe(ctx context.Context, httpAddr, lcpdGRPCAddr, apiKeys string, peerID model.ProviderPeerID, maxPriceMsat uint64) error {
	pidPath := c.Workspace.PIDPath(ComponentOpenAIServe)
	if running, pid := proc.IsRunning(pidPath); running {
		_, _ = state.Update(c.Workspace.StatePath, func(st model.State) model.State {
			st.EnsureDefaults()
			st.Components[ComponentOpenAIServe] = model.ComponentState{
				Running: true,
				PID:     pid,
				Addr:    httpAddr,
				Managed: true,
			}
			return st
		})
		return nil
	}

	binPath := filepath.Join(c.Workspace.BinDir, ComponentOpenAIServe)
	ctxBuild, cancelBuild := context.WithTimeout(ctx, buildTimeout)
	defer cancelBuild()
	if err := build.GoBinary(ctxBuild, filepath.Join(c.RepoRoot, "apps", "openai-serve"), "./cmd/openai-serve", binPath); err != nil {
		return err
	}

	pid, err := proc.Start(proc.Spec{
		Name:    ComponentOpenAIServe,
		Command: binPath,
		Env: []string{
			"OPENAI_SERVE_HTTP_ADDR=" + httpAddr,
			"OPENAI_SERVE_LCPD_GRPC_ADDR=" + lcpdGRPCAddr,
			"OPENAI_SERVE_API_KEYS=" + apiKeys,
			"OPENAI_SERVE_DEFAULT_PEER_ID=" + string(peerID),
			"OPENAI_SERVE_MAX_PRICE_MSAT=" + fmt.Sprintf("%d", maxPriceMsat),
		},
		LogPath: c.Workspace.LogPath(ComponentOpenAIServe),
		PIDPath: pidPath,
	})
	if err != nil {
		return fmt.Errorf("failed to start openai-serve (see %s): %w", c.Workspace.LogPath(ComponentOpenAIServe), err)
	}

	_, _ = state.Update(c.Workspace.StatePath, func(st model.State) model.State {
		st.EnsureDefaults()
		st.Components[ComponentOpenAIServe] = model.ComponentState{
			Running: true,
			PID:     pid,
			Addr:    httpAddr,
			Managed: true,
		}
		return st
	})
	return nil
}

func (c Controller) resolveProvider(network model.Network, raw string) (model.ProviderNode, model.ProviderPeerID, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		if network == model.NetworkMainnet {
			raw = string(model.DefaultMainnetProviderNode)
		} else {
			return "", "", errors.New("testnet requires --provider <pubkey>@<host:port>")
		}
	}
	return model.ParseProviderNode(raw)
}

func (c Controller) recordReadiness(mutate func(*model.Readiness)) {
	_, _ = state.Update(c.Workspace.StatePath, func(st model.State) model.State {
		st.EnsureDefaults()
		mutate(&st.Readiness)
		return st
	})
}

func (c Controller) recordComponentError(component, msg string) {
	_, _ = state.Update(c.Workspace.StatePath, func(st model.State) model.State {
		st.EnsureDefaults()
		cs := st.Components[component]
		cs.LastError = strings.TrimSpace(msg)
		st.Components[component] = cs
		return st
	})
}

func defaultLndTLSCertPath() string {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return "~/.lnd/tls.cert"
	}
	return filepath.Join(home, ".lnd", "tls.cert")
}

func defaultLndAdminMacaroonPath(network model.Network) string {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return "~/.lnd/data/chain/bitcoin/" + string(network) + "/admin.macaroon"
	}
	return filepath.Join(home, ".lnd", "data", "chain", "bitcoin", string(network), "admin.macaroon")
}

func defaultIfBlank(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return strings.TrimSpace(s)
}

func lncliCommandPrefix(network model.Network) string {
	return "lcp-quickstart " + string(network) + " lncli"
}

func requireLoopbackListenAddr(flagName, addr string) error {
	host, _, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return fmt.Errorf("--%s must be host:port (got %q): %w", flagName, addr, err)
	}
	if !isLoopbackHost(host) {
		return fmt.Errorf("--%s must bind to loopback (got %q)", flagName, addr)
	}
	return nil
}

func validateOpenAIServeHTTPAddr(addr, apiKeys string, iUnderstandExposingOpenAI bool) error {
	host, _, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return fmt.Errorf("--openai-http-addr must be host:port (got %q): %w", addr, err)
	}
	if isLoopbackHost(host) {
		return nil
	}
	if !iUnderstandExposingOpenAI {
		return fmt.Errorf("refusing to bind openai-serve to non-loopback addr %q; pass --i-understand-exposing-openai to override", addr)
	}
	if strings.TrimSpace(apiKeys) == "" {
		return errors.New("refusing to expose openai-serve without API keys; set --openai-api-keys or bind to 127.0.0.1")
	}
	return nil
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(host)
	switch {
	case host == "":
		return false
	case strings.EqualFold(host, "localhost"):
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func removeAllGuarded(path string) error {
	cleaned := filepath.Clean(expandTilde(strings.TrimSpace(path)))
	if cleaned == "" || cleaned == "." || cleaned == string(filepath.Separator) {
		return fmt.Errorf("refusing to remove unsafe path %q", path)
	}
	return os.RemoveAll(cleaned)
}

func expandTilde(path string) string {
	if path == "" || path[0] != '~' {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return path
	}
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	return path
}
