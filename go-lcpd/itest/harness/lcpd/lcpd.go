package lcpd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/bruwbird/lcp/go-lcpd/itest/harness/proc"
)

const defaultLCPDReadyTimeout = 25 * time.Second

// BuildBinary builds tools/lcpd-grpcd and returns the binary path.
func BuildBinary(t *testing.T) string {
	t.Helper()

	out := filepath.Join(t.TempDir(), "lcpd-grpcd")
	cmd := exec.CommandContext(context.Background(), "go", "build", "-o", out, "./tools/lcpd-grpcd")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Dir = findRepoRoot(t)

	if err := cmd.Run(); err != nil {
		t.Fatalf("build lcpd-grpcd: %v", err)
	}
	return out
}

// LNDConfig contains lnd connection parameters for lcpd-grpcd.
type LNDConfig struct {
	RPCAddr           string
	TLSCertPath       string
	AdminMacaroonPath string
}

// RunConfig describes an lcpd-grpcd process to launch.
type RunConfig struct {
	BinaryPath       string
	GRPCAddr         string
	LND              *LNDConfig
	Backend          string
	DeterministicB64 string
	ExtraEnv         []string
}

// Handle holds a running lcpd-grpcd process.
type Handle struct {
	Process *proc.Process
	Addr    string
}

// Start launches lcpd-grpcd with deterministic backend by default.
func Start(ctx context.Context, t *testing.T, cfg RunConfig) *Handle {
	t.Helper()

	bin := cfg.BinaryPath
	if bin == "" {
		bin = BuildBinary(t)
	}
	addr := cfg.GRPCAddr
	if addr == "" {
		addr = "127.0.0.1:50051"
	}
	backend := cfg.Backend
	if backend == "" {
		backend = "deterministic"
	}

	args := []string{
		fmt.Sprintf("-grpc_addr=%s", addr),
	}
	if cfg.LND != nil {
		if cfg.LND.RPCAddr != "" {
			args = append(args, fmt.Sprintf("-lnd_rpc_addr=%s", cfg.LND.RPCAddr))
		}
		if cfg.LND.TLSCertPath != "" {
			args = append(args, fmt.Sprintf("-lnd_tls_cert_path=%s", cfg.LND.TLSCertPath))
		}
		if cfg.LND.AdminMacaroonPath != "" {
			args = append(
				args,
				fmt.Sprintf("-lnd_admin_macaroon_path=%s", cfg.LND.AdminMacaroonPath),
			)
		}
	}

	env := []string{
		"LCPD_BACKEND=" + backend,
		// Ensure the started process is hermetic and not influenced by the
		// developer's local LND_* env vars (which would break E2E expectations).
		"LCPD_LND_RPC_ADDR=",
		"LCPD_LND_TLS_CERT_PATH=",
		"LCPD_LND_ADMIN_MACAROON_PATH=",
		"LCPD_LND_MACAROON_PATH=",
		"LCPD_LND_MANIFEST_RESEND_INTERVAL=",
	}
	if cfg.DeterministicB64 != "" {
		env = append(env, "LCPD_DETERMINISTIC_OUTPUT_BASE64="+cfg.DeterministicB64)
	}
	env = append(env, cfg.ExtraEnv...)

	handle := proc.Start(ctx, t, proc.Config{
		Path:         bin,
		Args:         args,
		Env:          env,
		ReadyText:    "ready",
		ReadyTimeout: defaultLCPDReadyTimeout,
		TeeStdout:    true,
		TeeStderr:    true,
	})

	return &Handle{
		Process: handle,
		Addr:    addr,
	}
}

func findRepoRoot(t *testing.T) string {
	t.Helper()

	const modTarget = "module github.com/bruwbird/lcp/go-lcpd"

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	start := dir

	for {
		modPath := filepath.Join(dir, "go.mod")
		data, readErr := os.ReadFile(modPath)
		if readErr == nil && bytes.Contains(data, []byte(modTarget)) {
			return dir
		}

		next := filepath.Dir(dir)
		if next == dir {
			t.Fatalf("go.mod (%s) not found upwards from %s", modTarget, start)
		}
		dir = next
	}
}
