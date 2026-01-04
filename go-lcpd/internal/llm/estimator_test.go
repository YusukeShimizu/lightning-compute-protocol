package llm_test

import (
	"testing"

	"github.com/bruwbird/lcp/go-lcpd/internal/computebackend"
	"github.com/bruwbird/lcp/go-lcpd/internal/llm"
	"github.com/google/go-cmp/cmp"
)

func TestApproxUsageEstimator_Estimate_Deterministic(t *testing.T) {
	t.Parallel()

	estimator := llm.NewApproxUsageEstimator()

	got, err := estimator.Estimate(
		computebackend.Task{
			TaskKind:   "openai.chat_completions.v1",
			Model:      "test-model",
			InputBytes: []byte("hello"), // len=5 -> ceil(5/4)=2
		},
		llm.ExecutionPolicy{MaxOutputTokens: 10},
	)
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}

	want := llm.Estimation{
		Usage: llm.UsageEstimate{
			InputTokens:     2,
			MaxOutputTokens: 10,
			TotalTokens:     12,
		},
		Resources: []llm.ResourceEstimate{
			{Name: "tokens.input_estimate", Amount: 2},
			{Name: "tokens.output_max", Amount: 10},
			{Name: "tokens.total_estimate", Amount: 12},
		},
		EstimatorID: "approx.v1",
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("estimation mismatch (-want +got):\n%s", diff)
	}
}
