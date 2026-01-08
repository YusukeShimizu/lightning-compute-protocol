package proc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	defaultStopTimeout = 5 * time.Second
	stopPollInterval   = 100 * time.Millisecond
)

type Spec struct {
	Name string

	Command string
	Args    []string
	Env     []string
	Dir     string

	LogPath string
	PIDPath string
}

func Start(spec Spec) (int, error) {
	if running, pid := IsRunning(spec.PIDPath); running {
		return pid, fmt.Errorf("%s is already running (pid %d)", spec.Name, pid)
	}
	if err := os.MkdirAll(filepath.Dir(spec.LogPath), 0o700); err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(spec.PIDPath), 0o700); err != nil {
		return 0, err
	}

	logFile, err := os.OpenFile(spec.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, err
	}
	defer func() { _ = logFile.Close() }()

	devNull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0o600)
	if err != nil {
		return 0, err
	}
	defer func() { _ = devNull.Close() }()

	cmd := exec.Command(spec.Command, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = devNull
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if len(spec.Env) > 0 {
		cmd.Env = append(os.Environ(), spec.Env...)
	}

	if err := cmd.Start(); err != nil {
		return 0, err
	}

	pid := cmd.Process.Pid
	if err := os.WriteFile(spec.PIDPath, []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		return 0, err
	}
	return pid, nil
}

func Stop(pidPath string) error {
	return StopWithTimeout(pidPath, defaultStopTimeout)
}

func StopWithTimeout(pidPath string, timeout time.Duration) error {
	pid, err := ReadPID(pidPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}

	if !pidRunning(pid) {
		_ = os.Remove(pidPath)
		return nil
	}

	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}

	_ = p.Signal(syscall.SIGTERM)

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !pidRunning(pid) {
			_ = os.Remove(pidPath)
			return nil
		}
		time.Sleep(stopPollInterval)
	}

	_ = p.Signal(syscall.SIGKILL)
	time.Sleep(200 * time.Millisecond)
	_ = os.Remove(pidPath)
	return nil
}

func IsRunning(pidPath string) (bool, int) {
	pid, err := ReadPID(pidPath)
	if err != nil {
		return false, 0
	}
	if !pidRunning(pid) {
		return false, pid
	}
	return true, pid
}

func ReadPID(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	raw := strings.TrimSpace(string(b))
	if raw == "" {
		return 0, fmt.Errorf("pid file %s is empty", path)
	}
	pid, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("parse pid in %s: %w", path, err)
	}
	if pid <= 0 {
		return 0, fmt.Errorf("invalid pid %d in %s", pid, path)
	}
	return pid, nil
}

func pidRunning(pid int) bool {
	err := syscall.Kill(pid, 0)
	switch {
	case err == nil:
		return true
	case errors.Is(err, syscall.EPERM):
		return true
	case errors.Is(err, syscall.ESRCH):
		return false
	default:
		return false
	}
}

