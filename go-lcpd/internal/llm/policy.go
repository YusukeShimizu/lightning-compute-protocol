package llm

import (
	"errors"
)

const DefaultMaxOutputTokens uint32 = 4096

type ExecutionPolicy struct {
	MaxOutputTokens uint32
}

type ExecutionPolicyProvider interface {
	Policy() ExecutionPolicy
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
