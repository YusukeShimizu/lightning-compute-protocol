package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/bruwbird/lcp/go-lcpd/internal/computebackend"
)

const DefaultMaxOutputTokens uint32 = 4096

type ExecutionPolicy struct {
	MaxOutputTokens uint32
}

type ExecutionPolicyProvider interface {
	Policy() ExecutionPolicy
	Apply(task computebackend.Task) (computebackend.Task, error)
}

type FixedExecutionPolicy struct {
	policy ExecutionPolicy
}

func NewFixedExecutionPolicy(maxOutputTokens uint32) (*FixedExecutionPolicy, error) {
	if maxOutputTokens == 0 {
		return nil, errors.New("max_output_tokens must be > 0")
	}
	return &FixedExecutionPolicy{
		policy: ExecutionPolicy{
			MaxOutputTokens: maxOutputTokens,
		},
	}, nil
}

func MustFixedExecutionPolicy(maxOutputTokens uint32) *FixedExecutionPolicy {
	p, err := NewFixedExecutionPolicy(maxOutputTokens)
	if err != nil {
		panic(err)
	}
	return p
}

func (p *FixedExecutionPolicy) Policy() ExecutionPolicy {
	return p.policy
}

func (p *FixedExecutionPolicy) Apply(task computebackend.Task) (computebackend.Task, error) {
	if task.TaskKind != "llm.chat" {
		return computebackend.Task{}, fmt.Errorf(
			"%w: %q",
			computebackend.ErrUnsupportedTaskKind,
			task.TaskKind,
		)
	}
	if strings.TrimSpace(task.Model) == "" {
		return computebackend.Task{}, fmt.Errorf(
			"%w: model is required",
			computebackend.ErrInvalidTask,
		)
	}
	if len(task.InputBytes) == 0 {
		return computebackend.Task{}, fmt.Errorf(
			"%w: input_bytes is required",
			computebackend.ErrInvalidTask,
		)
	}
	if len(task.ParamsBytes) != 0 {
		return computebackend.Task{}, fmt.Errorf(
			"%w: params_bytes must be empty",
			computebackend.ErrInvalidTask,
		)
	}

	params, err := json.Marshal(struct {
		MaxOutputTokens uint32 `json:"max_output_tokens"`
	}{
		MaxOutputTokens: p.policy.MaxOutputTokens,
	})
	if err != nil {
		return computebackend.Task{}, fmt.Errorf("marshal policy params: %w", err)
	}

	task.ParamsBytes = params
	return task, nil
}
