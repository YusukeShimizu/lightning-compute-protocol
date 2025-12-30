package proc

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	readLoopCount = 2
	readBufSize   = 1024

	defaultReadyTimeout = 20 * time.Second
	defaultStopTimeout  = 5 * time.Second
)

// Config describes a process to start for integration/E2E tests.
type Config struct {
	Path         string
	Args         []string
	Env          []string
	WorkDir      string
	ReadyText    string
	ReadyTimeout time.Duration
	StopTimeout  time.Duration
	TeeStdout    bool
	TeeStderr    bool
}

// Process represents a running external command.
type Process struct {
	cmd    *exec.Cmd
	stdout bytes.Buffer
	stderr bytes.Buffer
}

// Start launches the process and waits for ReadyText if provided.
func Start(ctx context.Context, t *testing.T, cfg Config) *Process {
	t.Helper()

	if cfg.Path == "" {
		t.Fatalf("process path is empty")
	}

	//nolint:gosec // Test harness executes commands controlled by the test code/config.
	cmd := exec.CommandContext(ctx, cfg.Path, cfg.Args...)
	if cfg.WorkDir != "" {
		cmd.Dir = cfg.WorkDir
	}
	if len(cfg.Env) > 0 {
		cmd.Env = append(os.Environ(), cfg.Env...)
	}

	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()

	var p Process
	cmd.Stdout = io.MultiWriter(&p.stdout, stdoutW)
	cmd.Stderr = io.MultiWriter(&p.stderr, stderrW)

	if err := cmd.Start(); err != nil {
		t.Fatalf("start process %s: %v", cfg.Path, err)
	}
	p.cmd = cmd

	var wg sync.WaitGroup
	wg.Add(readLoopCount)

	readyCh := make(chan struct{}, 1)
	go readLines(t, stdoutR, cfg.TeeStdout, "[stdout] ", cfg.ReadyText, readyCh, &wg)
	go readLines(t, stderrR, cfg.TeeStderr, "[stderr] ", cfg.ReadyText, readyCh, &wg)

	waitReady(t, cmd, cfg.Path, cfg.ReadyText, cfg.ReadyTimeout, readyCh)

	t.Cleanup(func() {
		if err := stdoutW.Close(); err != nil {
			t.Logf("close stdout pipe: %v", err)
		}
		if err := stderrW.Close(); err != nil {
			t.Logf("close stderr pipe: %v", err)
		}
		wg.Wait()
		if err := stop(cmd, cfg.StopTimeout); err != nil {
			t.Logf("stop process: %v", err)
		}
	})

	return &p
}

func readLines(
	t *testing.T,
	r io.Reader,
	tee bool,
	prefix string,
	readyText string,
	ready chan<- struct{},
	wg *sync.WaitGroup,
) {
	defer wg.Done()

	readyBytes := bytes.ToLower([]byte(strings.TrimSpace(readyText)))
	buf := make([]byte, readBufSize)
	for {
		n, err := r.Read(buf)
		if n > 0 && tee {
			t.Logf("%s%s", prefix, strings.TrimRight(string(buf[:n]), "\n"))
		}
		if n > 0 && len(readyBytes) > 0 && bytes.Contains(bytes.ToLower(buf[:n]), readyBytes) {
			select {
			case ready <- struct{}{}:
			default:
			}
		}
		if err != nil {
			return
		}
	}
}

func waitReady(
	t *testing.T,
	cmd *exec.Cmd,
	path string,
	readyText string,
	readyTimeout time.Duration,
	readyCh <-chan struct{},
) {
	t.Helper()

	if strings.TrimSpace(readyText) == "" {
		return
	}

	timeout := readyTimeout
	if timeout <= 0 {
		timeout = defaultReadyTimeout
	}

	select {
	case <-readyCh:
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		t.Fatalf("process %s did not become ready within %s", path, timeout)
	}
}

// Stop terminates the process (used rarely; Start already registers cleanup).
func (p *Process) Stop(timeout time.Duration) error {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	return stop(p.cmd, timeout)
}

func stop(cmd *exec.Cmd, timeout time.Duration) error {
	if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
		return nil
	}

	_ = cmd.Process.Signal(os.Interrupt)

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	if timeout <= 0 {
		timeout = defaultStopTimeout
	}

	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		return errors.New("process kill after timeout")
	}
}
