package openaiserve

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

const defaultReadyTimeout = 25 * time.Second

// BuildBinary builds apps/openai-serve/cmd/openai-serve and returns the binary path.
func BuildBinary(t *testing.T) string {
	t.Helper()

	out := filepath.Join(t.TempDir(), "openai-serve")
	cmd := exec.CommandContext(context.Background(), "go", "build", "-o", out, "./cmd/openai-serve")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Dir = findModuleRoot(t)

	if err := cmd.Run(); err != nil {
		t.Fatalf("build openai-serve: %v", err)
	}
	return out
}

// RunConfig describes an openai-serve process to launch.
type RunConfig struct {
	BinaryPath string
	HTTPAddr   string

	LCPDGRPCAddr string
	ExtraEnv     []string
}

// Handle holds a running openai-serve process.
type Handle struct {
	Process *proc.Process
	Addr    string
}

// Start launches openai-serve and waits for it to become ready.
func Start(ctx context.Context, t *testing.T, cfg RunConfig) *Handle {
	t.Helper()

	if cfg.LCPDGRPCAddr == "" {
		t.Fatalf("LCPDGRPCAddr is required")
	}

	bin := cfg.BinaryPath
	if bin == "" {
		bin = BuildBinary(t)
	}

	httpAddr := cfg.HTTPAddr
	if httpAddr == "" {
		httpAddr = "127.0.0.1:8080"
	}

	env := []string{
		"OPENAI_SERVE_HTTP_ADDR=" + httpAddr,
		"OPENAI_SERVE_LCPD_GRPC_ADDR=" + cfg.LCPDGRPCAddr,
		"OPENAI_SERVE_LOG_LEVEL=info",
		// Ensure the started process is hermetic and not influenced by the
		// developer's local OPENAI_SERVE_* env vars.
		"OPENAI_SERVE_API_KEYS=",
		"OPENAI_SERVE_DEFAULT_PEER_ID=",
		"OPENAI_SERVE_MODEL_MAP=",
		"OPENAI_SERVE_MODEL_ALLOWLIST=",
		"OPENAI_SERVE_ALLOW_UNLISTED_MODELS=",
		"OPENAI_SERVE_MAX_PRICE_MSAT=",
		"OPENAI_SERVE_TIMEOUT_QUOTE=",
		"OPENAI_SERVE_TIMEOUT_EXECUTE=",
	}
	env = append(env, cfg.ExtraEnv...)

	handle := proc.Start(ctx, t, proc.Config{
		Path:         bin,
		Env:          env,
		ReadyText:    "listening",
		ReadyTimeout: defaultReadyTimeout,
		TeeStdout:    proc.TeeEnabled(),
		TeeStderr:    proc.TeeEnabled(),
	})

	return &Handle{
		Process: handle,
		Addr:    httpAddr,
	}
}

func findModuleRoot(t *testing.T) string {
	t.Helper()

	const (
		goLCPDModuleLine      = "module github.com/bruwbird/lcp/go-lcpd"
		openAIServeModuleLine = "module github.com/bruwbird/lcp/apps/openai-serve"
	)

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	start := dir

	for {
		modPath := filepath.Join(dir, "go.mod")
		data, readErr := os.ReadFile(modPath)
		if readErr == nil && bytes.Contains(data, []byte(goLCPDModuleLine)) {
			repoRoot := filepath.Dir(dir)
			openAI := filepath.Join(repoRoot, "apps", "openai-serve")
			openAIMod := filepath.Join(openAI, "go.mod")
			openAIModBytes, openAIModErr := os.ReadFile(openAIMod)
			if openAIModErr == nil &&
				bytes.Contains(openAIModBytes, []byte(openAIServeModuleLine)) {
				return openAI
			}
			if openAIModErr != nil {
				t.Fatalf("read openai-serve go.mod (%s): %v", openAIMod, openAIModErr)
			}
			t.Fatalf("go.mod at %s does not contain %q", openAIMod, openAIServeModuleLine)
		}

		next := filepath.Dir(dir)
		if next == dir {
			t.Fatalf("go.mod (%s) not found upwards from %s", goLCPDModuleLine, start)
		}
		dir = next
	}
}

func (c RunConfig) String() string {
	return fmt.Sprintf("openai-serve(http=%s,lcpd=%s)", c.HTTPAddr, c.LCPDGRPCAddr)
}
