package llm_test

import (
	"testing"

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

func TestFixedExecutionPolicy_Policy_ReturnsConfiguredValue(t *testing.T) {
	t.Parallel()

	p := llm.MustFixedExecutionPolicy(123)

	if diff := cmp.Diff(uint32(123), p.Policy().MaxOutputTokens); diff != "" {
		t.Fatalf("max_output_tokens mismatch (-want +got):\n%s", diff)
	}
}
