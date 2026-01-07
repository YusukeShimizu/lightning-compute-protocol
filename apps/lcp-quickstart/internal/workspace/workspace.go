package workspace

import (
	"os"
	"path/filepath"
	"strings"
)

type Workspace struct {
	DataDir   string
	StatePath string

	LogsDir string
	PIDsDir string
	BinDir  string
}

func Resolve(homeOverride string) Workspace {
	homeOverride = strings.TrimSpace(homeOverride)
	if homeOverride != "" {
		return workspaceAt(expandTilde(homeOverride))
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return workspaceAt(".lcp-quickstart")
	}
	return workspaceAt(filepath.Join(home, ".lcp-quickstart"))
}

func workspaceAt(dir string) Workspace {
	return Workspace{
		DataDir:   dir,
		StatePath: filepath.Join(dir, "state.json"),
		LogsDir:   filepath.Join(dir, "logs"),
		PIDsDir:   filepath.Join(dir, "pids"),
		BinDir:    filepath.Join(dir, "bin"),
	}
}

func (w Workspace) Ensure() error {
	if err := os.MkdirAll(w.DataDir, 0o700); err != nil {
		return err
	}
	for _, dir := range []string{w.LogsDir, w.PIDsDir, w.BinDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return nil
}

func (w Workspace) LogPath(component string) string {
	return filepath.Join(w.LogsDir, component+".log")
}

func (w Workspace) PIDPath(component string) string {
	return filepath.Join(w.PIDsDir, component+".pid")
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
