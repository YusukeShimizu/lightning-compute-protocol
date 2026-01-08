package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bruwbird/lcp/apps/lcp-quickstart/internal/controller"
	"github.com/bruwbird/lcp/apps/lcp-quickstart/internal/deps"
	"github.com/bruwbird/lcp/apps/lcp-quickstart/internal/lncli"
	"github.com/bruwbird/lcp/apps/lcp-quickstart/internal/model"
	"github.com/bruwbird/lcp/apps/lcp-quickstart/internal/reporoot"
	"github.com/bruwbird/lcp/apps/lcp-quickstart/internal/workspace"
)

const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

func Main(args []string) int {
	return MainWithIO(args, os.Stdout, os.Stderr)
}

func MainWithIO(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("lcp-quickstart", flag.ContinueOnError)
	fs.SetOutput(stderr)

	home := fs.String("home", "", "workspace directory (default: ~/.lcp-quickstart)")
	repoRoot := fs.String("repo-root", "", "repo root (auto-detected from cwd if empty)")
	lncliPath := fs.String("lncli", "lncli", "path to lncli binary")
	autoInstall := fs.Bool("auto-install", true, "auto-install missing lnd/lncli into workspace/bin")
	version := fs.Bool("version", false, "print version and exit")

	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return exitOK
		}
		fprintln(stderr, err)
		return exitUsage
	}

	if *version {
		fprintln(stdout, "lcp-quickstart (dev)")
		return exitOK
	}

	rest := fs.Args()
	if len(rest) < 2 {
		printUsage(stderr)
		return exitUsage
	}

	network, err := model.ParseNetwork(rest[0])
	if err != nil {
		fprintln(stderr, err)
		return exitUsage
	}
	cmd := strings.ToLower(strings.TrimSpace(rest[1]))
	cmdArgs := rest[2:]

	ws := workspace.Resolve(*home)

	root := strings.TrimSpace(*repoRoot)
	if root == "" && cmd == "up" {
		cwd, err := os.Getwd()
		if err != nil {
			fprintln(stderr, err)
			return exitError
		}
		root, err = reporoot.Detect(cwd)
		if err != nil {
			fprintln(stderr, err)
			return exitError
		}
	}

	ctl := controller.Controller{
		Workspace: ws,
		RepoRoot:  root,
		LNCLI:     lncli.Client{Path: *lncliPath},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	switch cmd {
	case "up":
		return runUp(ctx, ctl, network, *autoInstall, cmdArgs, stdout, stderr)
	case "down":
		return runDown(ctx, ctl, network, cmdArgs, stdout, stderr)
	case "status":
		return runStatus(ctx, ctl, network, *autoInstall, cmdArgs, stdout, stderr)
	case "lncli":
		return runLNCLI(ctx, ctl, network, *autoInstall, cmdArgs, stdout, stderr)
	case "logs":
		return runLogs(ctx, ws, network, cmdArgs, stdout, stderr)
	case "reset":
		return runReset(ctx, ctl, network, cmdArgs, stdout, stderr)
	case "-h", "--help", "help":
		printUsage(stdout)
		return exitOK
	default:
		fprintf(stderr, "unknown command: %q\n", cmd)
		printUsage(stderr)
		return exitUsage
	}
}

func runUp(ctx context.Context, ctl controller.Controller, network model.Network, autoInstall bool, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(string(network)+" up", flag.ContinueOnError)
	fs.SetOutput(stderr)

	provider := fs.String("provider", "", "Provider node: <pubkey>@<host:port> (required for testnet)")
	startLND := fs.Bool("start-lnd", false, "start lnd if lncli getinfo fails (best-effort)")
	openChannel := fs.Bool("open-channel", false, "attempt to open a channel if liquidity is insufficient")
	channelSats := fs.Int64("channel-sats", 0, "channel size in sats (required with --open-channel)")
	iUnderstandMainnet := fs.Bool("i-understand-mainnet", false, "required for mainnet up")
	iUnderstandExposingOpenAI := fs.Bool("i-understand-exposing-openai", false, "allow openai-serve to bind non-loopback addr (dangerous)")

	lcpdGRPCAddr := fs.String("lcpd-grpc-addr", defaultLCPDGRPCAddr, "lcpd-grpcd listen addr")
	lndRPCAddr := fs.String("lnd-rpc-addr", defaultLNDRPCAddr, "lnd gRPC addr host:port")
	lndTLSCert := fs.String("lnd-tls-cert-path", "", "lnd tls.cert path (default: ~/.lnd/tls.cert)")
	lndMacaroon := fs.String("lnd-admin-macaroon-path", "", "lnd admin.macaroon path (default: ~/.lnd/.../admin.macaroon)")

	openAIHTTPAddr := fs.String("openai-http-addr", defaultOpenAIHTTPAddr, "openai-serve HTTP listen addr")
	openAIAPIKeys := fs.String("openai-api-keys", defaultOpenAIAPIKeys, "openai-serve API keys CSV (empty disables auth)")
	openAIMaxPrice := fs.String("openai-max-price-msat", "", "openai-serve max price msat (empty uses network default; 0 disables)")

	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return exitOK
		}
		fprintln(stderr, err)
		return exitUsage
	}

	var maxPrice uint64
	var maxPriceSet bool
	if raw := strings.TrimSpace(*openAIMaxPrice); raw != "" {
		parsed, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			fprintln(stderr, "invalid --openai-max-price-msat:", err)
			return exitUsage
		}
		maxPrice = parsed
		maxPriceSet = true
	}

	resolvedLNCLI, err := ensureLNDTools(ctx, ctl.Workspace, ctl.LNCLI.Path, autoInstall, *startLND)
	if err != nil {
		fprintln(stderr, err)
		return exitError
	}
	ctl.LNCLI.Path = resolvedLNCLI

	opts := controller.UpOptions{
		Network:                   network,
		Provider:                  *provider,
		StartLND:                  *startLND,
		OpenChannel:               *openChannel,
		ChannelSats:               *channelSats,
		IUnderstandMainnet:        *iUnderstandMainnet,
		IUnderstandExposingOpenAI: *iUnderstandExposingOpenAI,
		LCPDGRPCAddr:              *lcpdGRPCAddr,
		LNDRPCAddr:                *lndRPCAddr,
		LNDTLSCertPath:            *lndTLSCert,
		LNDAdminMacaroonPath:      *lndMacaroon,
		OpenAIHTTPAddr:            *openAIHTTPAddr,
		OpenAIAPIKeysCSV:          *openAIAPIKeys,
		OpenAIMaxPriceMsat:        maxPrice,
		OpenAIMaxPriceSet:         maxPriceSet,
	}

	ctxTimeout, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	if err := ctl.Up(ctxTimeout, opts); err != nil {
		fprintln(stderr, err)
		return exitError
	}

	fprintf(stdout, "ok: %s up\n", network)
	fprintf(stdout, "healthz: curl -sS http://%s/healthz\n", *openAIHTTPAddr)
	fprintf(stdout, "openai base_url: http://%s/v1\n", *openAIHTTPAddr)
	fprintf(stdout, "status: lcp-quickstart %s status\n", network)
	fprintf(stdout, "down: lcp-quickstart %s down\n", network)
	return exitOK
}

func runDown(ctx context.Context, ctl controller.Controller, network model.Network, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(string(network)+" down", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return exitOK
		}
		fprintln(stderr, err)
		return exitUsage
	}

	if err := ctl.Down(ctx, network); err != nil {
		fprintln(stderr, err)
		return exitError
	}
	fprintf(stdout, "ok: %s down\n", network)
	return exitOK
}

func runStatus(ctx context.Context, ctl controller.Controller, network model.Network, autoInstall bool, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(string(network)+" status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print state.json and exit")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return exitOK
		}
		fprintln(stderr, err)
		return exitUsage
	}

	resolvedLNCLI, err := ensureLNDTools(ctx, ctl.Workspace, ctl.LNCLI.Path, autoInstall, false)
	if err != nil {
		fprintln(stderr, err)
		return exitError
	}
	ctl.LNCLI.Path = resolvedLNCLI

	st, err := ctl.Status(ctx, network)
	if err != nil {
		fprintln(stderr, err)
		return exitError
	}

	if *asJSON {
		b, err := json.MarshalIndent(st, "", "  ")
		if err != nil {
			fprintln(stderr, err)
			return exitError
		}
		fprintln(stdout, string(b))
		return exitOK
	}

	walletState := lncli.WalletUnknown
	if !st.Readiness.LNDReady {
		walletState = ctl.LNCLI.DetectWalletState(ctx, network)
	}
	printState(stdout, ctl.Workspace, st)
	printNextSteps(stdout, network, st, walletState)
	return exitOK
}

func runLNCLI(ctx context.Context, ctl controller.Controller, network model.Network, autoInstall bool, args []string, stdout, stderr io.Writer) int {
	_ = stdout
	_ = stderr

	resolvedLNCLI, err := ensureLNDTools(ctx, ctl.Workspace, ctl.LNCLI.Path, autoInstall, false)
	if err != nil {
		fprintln(os.Stderr, err)
		return exitError
	}

	cmdArgs := make([]string, 0, len(args)+1)
	if network == model.NetworkTestnet && !hasLNCLINetworkFlag(args) {
		cmdArgs = append(cmdArgs, "--network=testnet")
	}
	cmdArgs = append(cmdArgs, args...)

	cmd := exec.CommandContext(ctx, resolvedLNCLI, cmdArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		return exitError
	}
	return exitOK
}

func hasLNCLINetworkFlag(args []string) bool {
	for i, arg := range args {
		arg = strings.TrimSpace(arg)
		if strings.HasPrefix(arg, "--network=") {
			return true
		}
		if arg == "--network" && i+1 < len(args) {
			return true
		}
	}
	return false
}

func runLogs(ctx context.Context, ws workspace.Workspace, network model.Network, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(string(network)+" logs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return exitOK
		}
		fprintln(stderr, err)
		return exitUsage
	}
	if fs.NArg() != 1 {
		fprintln(stderr, "usage: lcp-quickstart <network> logs <component>")
		fprintln(stderr, "components: lnd, lcpd-grpcd, openai-serve")
		return exitUsage
	}
	componentRaw := strings.TrimSpace(fs.Arg(0))
	component, ok := normalizeComponent(componentRaw)
	if !ok {
		fprintf(stderr, "unknown component: %q\n", componentRaw)
		fprintln(stderr, "components: lnd, lcpd-grpcd, openai-serve")
		return exitUsage
	}
	logPath := ws.LogPath(component)
	if _, err := os.Stat(logPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fprintf(stderr, "log file not found: %s\n", logPath)
			fprintf(stderr, "run `lcp-quickstart %s up` first\n", network)
			return exitError
		}
		fprintln(stderr, err)
		return exitError
	}
	fprintf(stdout, "tailing %s\n", logPath)
	cmd := exec.CommandContext(ctx, "tail", "-n", "200", "-f", logPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		fprintln(stderr, err)
		return exitError
	}
	return exitOK
}

func runReset(ctx context.Context, ctl controller.Controller, network model.Network, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(string(network)+" reset", flag.ContinueOnError)
	fs.SetOutput(stderr)
	force := fs.Bool("force", false, "required")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return exitOK
		}
		fprintln(stderr, err)
		return exitUsage
	}
	if err := ctl.Reset(ctx, network, *force); err != nil {
		fprintln(stderr, err)
		return exitError
	}
	fprintf(stdout, "ok: %s reset\n", network)
	return exitOK
}

const (
	defaultLCPDGRPCAddr   = "127.0.0.1:50051"
	defaultLNDRPCAddr     = "localhost:10009"
	defaultOpenAIHTTPAddr = "127.0.0.1:8080"
	defaultOpenAIAPIKeys  = "lcp-dev"
)

func printUsage(w io.Writer) {
	fprintln(w, "usage: lcp-quickstart [--home <dir>] [--repo-root <path>] [--auto-install] <testnet|mainnet> <up|down|status|logs|reset|lncli> [args]")
	fprintln(w, "")
	fprintln(w, "examples:")
	fprintln(w, "  lcp-quickstart testnet up --provider <pubkey>@<host:port>")
	fprintln(w, "  lcp-quickstart mainnet up --i-understand-mainnet")
	fprintln(w, "  lcp-quickstart testnet status")
	fprintln(w, "  lcp-quickstart testnet logs openai-serve")
	fprintln(w, "  lcp-quickstart testnet lncli getinfo")
	fprintln(w, "  lcp-quickstart testnet down")
	fprintln(w, "  lcp-quickstart testnet reset --force")
}

func printState(w io.Writer, ws workspace.Workspace, st model.State) {
	fprintf(w, "network: %s\n", st.Network)
	if strings.TrimSpace(string(st.ProviderNode)) != "" {
		fprintf(w, "provider: %s\n", st.ProviderNode)
	}
	fprintf(w, "workspace: %s\n", ws.DataDir)
	fprintln(w, "")

	fprintf(w, "readiness: lnd=%t provider_connected=%t channel_ready=%t lcpd=%t openai=%t\n",
		st.Readiness.LNDReady,
		st.Readiness.ProviderConnected,
		st.Readiness.ChannelReady,
		st.Readiness.LCPDReady,
		st.Readiness.OpenAIReady,
	)

	for _, component := range []string{controller.ComponentLND, controller.ComponentLCPDGRPCD, controller.ComponentOpenAIServe} {
		cs, ok := st.Components[component]
		if !ok {
			continue
		}
		status := "stopped"
		if cs.Running {
			status = "running"
		}
		fprintf(w, "- %s: %s", component, status)
		if cs.PID != 0 {
			fprintf(w, " pid=%d", cs.PID)
		}
		if strings.TrimSpace(cs.Addr) != "" {
			fprintf(w, " addr=%s", cs.Addr)
		}
		if cs.Managed {
			fprint(w, " managed")
		}
		fprintln(w, "")
		if strings.TrimSpace(cs.LastError) != "" {
			fprintf(w, "  last_error: %s\n", cs.LastError)
		}
		fprintf(w, "  log: %s\n", ws.LogPath(component))
	}
}

func printNextSteps(w io.Writer, network model.Network, st model.State, walletState lncli.WalletState) {
	next := statusNextSteps(network, st, walletState)
	if len(next) == 0 {
		return
	}
	fprintln(w, "")
	fprintln(w, "next:")
	for _, line := range next {
		fprintf(w, "  - %s\n", line)
	}
}

func statusNextSteps(network model.Network, st model.State, walletState lncli.WalletState) []string {
	var next []string

	upCmd := upCommand(network, st)

	if !st.Readiness.LNDReady {
		switch walletState {
		case lncli.WalletUninitialized:
			next = append(next, fmt.Sprintf("initialize lnd wallet: %s create", lncliCommandPrefix(network)))
		case lncli.WalletLocked:
			next = append(next, fmt.Sprintf("unlock lnd wallet: %s unlock", lncliCommandPrefix(network)))
		default:
			next = append(next, fmt.Sprintf("start lnd (or re-run: %s --start-lnd)", upCmd))
		}
		return next
	}

	if strings.TrimSpace(string(st.ProviderNode)) == "" {
		if network == model.NetworkTestnet {
			next = append(next, "set a Provider and run: "+upCmd)
			return next
		}
		next = append(next, "run: "+upCmd)
		return next
	}

	switch {
	case !st.Readiness.ProviderConnected:
		next = append(next, "connect Provider / start services: "+upCmd)
	case !st.Readiness.ChannelReady:
		next = append(next, "need outbound liquidity; open a channel: "+upCmd+" --open-channel --channel-sats <sats>")
	case !st.Readiness.LCPDReady || !st.Readiness.OpenAIReady:
		next = append(next, "start services: "+upCmd)
	}

	if openAIAddr := strings.TrimSpace(st.Components[controller.ComponentOpenAIServe].Addr); openAIAddr != "" {
		next = append(next, fmt.Sprintf("healthz: curl -sS http://%s/healthz", openAIAddr))
	}

	return next
}

func upCommand(network model.Network, st model.State) string {
	parts := []string{"lcp-quickstart", string(network), "up"}
	if network == model.NetworkMainnet {
		parts = append(parts, "--i-understand-mainnet")
	}

	node := strings.TrimSpace(string(st.ProviderNode))
	switch {
	case node != "":
		parts = append(parts, "--provider", node)
	case network == model.NetworkTestnet:
		parts = append(parts, "--provider", "<pubkey>@<host:port>")
	}

	return strings.Join(parts, " ")
}

func lncliCommandPrefix(network model.Network) string {
	return "lcp-quickstart " + string(network) + " lncli"
}

func normalizeComponent(component string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(component)) {
	case controller.ComponentLND:
		return controller.ComponentLND, true
	case controller.ComponentLCPDGRPCD, "lcpd", "grpcd":
		return controller.ComponentLCPDGRPCD, true
	case controller.ComponentOpenAIServe, "openai", "serve":
		return controller.ComponentOpenAIServe, true
	default:
		return "", false
	}
}

func fprintln(w io.Writer, a ...any) {
	_, _ = fmt.Fprintln(w, a...)
}

func ensureLNDTools(ctx context.Context, ws workspace.Workspace, lncliPath string, autoInstall bool, needLND bool) (string, error) {
	lncliPath = strings.TrimSpace(lncliPath)
	if lncliPath == "" {
		lncliPath = "lncli"
	}

	if lncliPath != "lncli" {
		if _, err := exec.LookPath(lncliPath); err != nil {
			return "", fmt.Errorf("lncli not found: %s", lncliPath)
		}
		return lncliPath, nil
	}

	_, lncliErr := exec.LookPath("lncli")
	_, lndErr := exec.LookPath("lnd")

	if lncliErr == nil && (!needLND || lndErr == nil) {
		return "lncli", nil
	}

	// If we already downloaded LND in the workspace, use that first.
	downloadedLNCLI := filepath.Join(ws.BinDir, "lncli")
	downloadedLND := filepath.Join(ws.BinDir, "lnd")
	if fileExists(downloadedLNCLI) && (!needLND || fileExists(downloadedLND)) {
		return downloadedLNCLI, nil
	}

	if !autoInstall {
		if lncliErr != nil {
			return "", errors.New("lncli not found; install lnd/lncli or pass --auto-install")
		}
		return "", errors.New("lnd not found; install lnd or pass --auto-install")
	}

	bundle, err := deps.EnsureLND(ctx, ws, deps.DefaultLNDVersion)
	if err != nil {
		return "", err
	}
	return bundle.LNCLIPath, nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func fprint(w io.Writer, a ...any) {
	_, _ = fmt.Fprint(w, a...)
}

func fprintf(w io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(w, format, a...)
}
