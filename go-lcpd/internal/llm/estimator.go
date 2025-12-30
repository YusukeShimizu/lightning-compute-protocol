package llm

import (
	"errors"
	"fmt"

	"github.com/bruwbird/lcp/go-lcpd/internal/computebackend"
)

type UsageEstimate struct {
	InputTokens     uint64
	MaxOutputTokens uint64
	TotalTokens     uint64
}

type ResourceEstimate struct {
	Name   string
	Amount uint64
}

type Estimation struct {
	Usage       UsageEstimate
	Resources   []ResourceEstimate
	EstimatorID string
}

type UsageEstimator interface {
	Estimate(task computebackend.Task, policy ExecutionPolicy) (Estimation, error)
}

type ApproxUsageEstimator struct {
	EstimatorID string
}

func NewApproxUsageEstimator() ApproxUsageEstimator {
	return ApproxUsageEstimator{EstimatorID: "approx.v1"}
}

func (e ApproxUsageEstimator) Estimate(
	task computebackend.Task,
	policy ExecutionPolicy,
) (Estimation, error) {
	if task.TaskKind != "llm.chat" {
		return Estimation{}, fmt.Errorf(
			"%w: %q",
			computebackend.ErrUnsupportedTaskKind,
			task.TaskKind,
		)
	}
	if len(task.InputBytes) == 0 {
		return Estimation{}, fmt.Errorf(
			"%w: input_bytes is required",
			computebackend.ErrInvalidTask,
		)
	}
	if policy.MaxOutputTokens == 0 {
		return Estimation{}, errors.New("policy.max_output_tokens must be > 0")
	}

	inputTokens := approxTokenCountBytes(task.InputBytes)
	maxOutputTokens := uint64(policy.MaxOutputTokens)
	totalTokens := inputTokens + maxOutputTokens

	resources := []ResourceEstimate{
		{Name: "tokens.input_estimate", Amount: inputTokens},
		{Name: "tokens.output_max", Amount: maxOutputTokens},
		{Name: "tokens.total_estimate", Amount: totalTokens},
	}

	return Estimation{
		Usage: UsageEstimate{
			InputTokens:     inputTokens,
			MaxOutputTokens: maxOutputTokens,
			TotalTokens:     totalTokens,
		},
		Resources:   resources,
		EstimatorID: e.EstimatorID,
	}, nil
}

func approxTokenCountBytes(b []byte) uint64 {
	// Deterministic approximation: ceil(len(bytes)/4).
	n := uint64(len(b))
	if n == 0 {
		return 0
	}

	const (
		approxBytesPerToken         uint64 = 4
		approxBytesPerTokenRounding uint64 = approxBytesPerToken - 1
	)

	return (n + approxBytesPerTokenRounding) / approxBytesPerToken
}
