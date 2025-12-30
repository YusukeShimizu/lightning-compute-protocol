package llm_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/bruwbird/lcp/go-lcpd/internal/computebackend"
	"github.com/bruwbird/lcp/go-lcpd/internal/llm"
	"github.com/google/go-cmp/cmp"
)

func TestNewFixedExecutionPolicy_RejectsZero(t *testing.T) {
	t.Parallel()

	_, err := llm.NewFixedExecutionPolicy(0)
	if err == nil {
		t.Fatalf("expected error")
	}
}

func TestFixedExecutionPolicy_Apply_SetsMaxOutputTokensParams(t *testing.T) {
	t.Parallel()

	p := llm.MustFixedExecutionPolicy(123)

	got, err := p.Apply(computebackend.Task{
		TaskKind:   "llm.chat",
		Model:      "test-model",
		InputBytes: []byte("hello"),
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	var decoded struct {
		MaxOutputTokens uint32 `json:"max_output_tokens"`
	}
	unmarshalErr := json.Unmarshal(got.ParamsBytes, &decoded)
	if unmarshalErr != nil {
		t.Fatalf("unmarshal params: %v", unmarshalErr)
	}

	if diff := cmp.Diff(uint32(123), decoded.MaxOutputTokens); diff != "" {
		t.Fatalf("max_output_tokens mismatch (-want +got):\n%s", diff)
	}
}

func TestFixedExecutionPolicy_Apply_RejectsRequesterParamsBytes(t *testing.T) {
	t.Parallel()

	p := llm.MustFixedExecutionPolicy(123)

	_, err := p.Apply(computebackend.Task{
		TaskKind:    "llm.chat",
		Model:       "test-model",
		InputBytes:  []byte("hello"),
		ParamsBytes: []byte("not-empty"),
	})
	if err == nil {
		t.Fatalf("expected error")
	}
	if !errors.Is(err, computebackend.ErrInvalidTask) {
		t.Fatalf("expected invalid task error, got %v", err)
	}
}

func TestFixedExecutionPolicy_Apply_RejectsMissingInputBytes(t *testing.T) {
	t.Parallel()

	p := llm.MustFixedExecutionPolicy(123)

	_, err := p.Apply(computebackend.Task{
		TaskKind: "llm.chat",
		Model:    "test-model",
	})
	if err == nil {
		t.Fatalf("expected error")
	}
	if !errors.Is(err, computebackend.ErrInvalidTask) {
		t.Fatalf("expected invalid task error, got %v", err)
	}
}

func TestFixedExecutionPolicy_Apply_RejectsUnsupportedTaskKind(t *testing.T) {
	t.Parallel()

	p := llm.MustFixedExecutionPolicy(123)

	_, err := p.Apply(computebackend.Task{
		TaskKind:   "embedding",
		Model:      "test-model",
		InputBytes: []byte("hello"),
	})
	if err == nil {
		t.Fatalf("expected error")
	}
	if !errors.Is(err, computebackend.ErrUnsupportedTaskKind) {
		t.Fatalf("expected unsupported task_kind error, got %v", err)
	}
}
