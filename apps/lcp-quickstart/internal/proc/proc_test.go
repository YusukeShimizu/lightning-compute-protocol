package proc

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadPID(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "p.pid")
	if err := os.WriteFile(path, []byte("123\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	pid, err := ReadPID(path)
	if err != nil {
		t.Fatalf("ReadPID() error = %v", err)
	}
	if pid != 123 {
		t.Fatalf("pid = %d, want %d", pid, 123)
	}
}

