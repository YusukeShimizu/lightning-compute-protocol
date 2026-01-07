package reporoot

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func Detect(startDir string) (string, error) {
	startDir = filepath.Clean(startDir)
	dir := startDir
	for {
		if isRepoRoot(dir) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("failed to detect repo root from %s; set --repo-root", startDir)
}

func isRepoRoot(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, "go-lcpd", "go.mod")); err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, "apps", "openai-serve", "go.mod")); err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, "apps", "lcp-quickstart", "spec.md")); err != nil {
		return false
	}
	return true
}

func MustDetectFromCWD() string {
	cwd, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	root, err := Detect(cwd)
	if err != nil {
		panic(err)
	}
	return root
}

func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil || !errors.Is(err, os.ErrNotExist)
}

