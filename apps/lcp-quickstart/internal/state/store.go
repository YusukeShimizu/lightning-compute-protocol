package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/bruwbird/lcp/apps/lcp-quickstart/internal/model"
)

const (
	lockWaitTimeout  = 5 * time.Second
	lockStaleAfter   = 30 * time.Second
	lockPollInterval = 50 * time.Millisecond
)

func Read(path string) (model.State, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return model.NewState(), nil
		}
		return model.State{}, err
	}
	var st model.State
	if err := json.Unmarshal(b, &st); err != nil {
		return model.State{}, fmt.Errorf("parse state json: %w", err)
	}
	st.EnsureDefaults()
	return st, nil
}

func Write(path string, st model.State) error {
	st.EnsureDefaults()
	st = st.WithUpdatedAt(time.Now())
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(path, b, 0o600)
}

func Update(path string, mutate func(model.State) model.State) (model.State, error) {
	lockPath := path + ".lock"
	var updated model.State
	err := withLock(lockPath, func() error {
		st, err := Read(path)
		if err != nil {
			return err
		}
		updated = mutate(st)
		return Write(path, updated)
	})
	if err != nil {
		return model.State{}, err
	}
	return updated, nil
}

func withLock(lockPath string, fn func() error) error {
	deadline := time.Now().Add(lockWaitTimeout)
	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = fmt.Fprintf(f, "pid=%d time=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))
			_ = f.Close()
			defer func() { _ = os.Remove(lockPath) }()
			return fn()
		}
		if !errors.Is(err, os.ErrExist) {
			return err
		}

		info, statErr := os.Stat(lockPath)
		if statErr == nil && time.Since(info.ModTime()) > lockStaleAfter {
			_ = os.Remove(lockPath)
			continue
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for state lock: %s", lockPath)
		}
		time.Sleep(lockPollInterval)
	}
}

func atomicWriteFile(path string, contents []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp.*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(contents); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}

	// Best-effort durability: fsync the containing directory.
	if dirFile, err := os.Open(dir); err == nil {
		_ = dirFile.Sync()
		_ = dirFile.Close()
	}
	return nil
}

