package state

import (
	"path/filepath"
	"testing"

	"github.com/bruwbird/lcp/apps/lcp-quickstart/internal/model"
)

func TestRead_MissingReturnsEmptyState(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	st, err := Read(path)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if st.Version != model.StateVersion {
		t.Fatalf("st.Version = %d, want %d", st.Version, model.StateVersion)
	}
	if st.Components == nil {
		t.Fatal("st.Components is nil")
	}
}

func TestWriteThenRead_RoundTrip(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	want := model.NewState()
	want.Network = model.NetworkTestnet
	want.ProviderNode = "aa"
	want.Components["openai-serve"] = model.ComponentState{Running: true, PID: 123}
	if err := Write(path, want); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	got, err := Read(path)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if got.Network != want.Network {
		t.Fatalf("Network = %q, want %q", got.Network, want.Network)
	}
	if got.Components["openai-serve"].PID != 123 {
		t.Fatalf("components[openai-serve].PID = %d, want %d", got.Components["openai-serve"].PID, 123)
	}
	if got.UpdatedAtRFC3339 == "" {
		t.Fatal("UpdatedAtRFC3339 is empty")
	}
}

func TestUpdate_MutatesAndPersists(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	_, err := Update(path, func(st model.State) model.State {
		st.Network = model.NetworkMainnet
		st.Components["lcpd-grpcd"] = model.ComponentState{Running: true, PID: 456}
		return st
	})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	got, err := Read(path)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if got.Network != model.NetworkMainnet {
		t.Fatalf("Network = %q, want %q", got.Network, model.NetworkMainnet)
	}
	if got.Components["lcpd-grpcd"].PID != 456 {
		t.Fatalf("components[lcpd-grpcd].PID = %d, want %d", got.Components["lcpd-grpcd"].PID, 456)
	}
}

