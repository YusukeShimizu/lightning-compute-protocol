package computebackend

import (
	"context"
	"errors"
	"fmt"
)

var (
	// ErrUnsupportedTaskKind indicates the backend does not support the given task kind.
	ErrUnsupportedTaskKind = errors.New("unsupported task_kind")

	// ErrInvalidTask indicates the task payload (input_bytes/params_bytes) is invalid for the given task kind.
	ErrInvalidTask = errors.New("invalid task")

	// ErrUnauthenticated indicates missing/invalid credentials for the backend.
	ErrUnauthenticated = errors.New("unauthenticated")

	// ErrBackendUnavailable indicates a transient backend failure (network, 5xx, timeouts, etc).
	ErrBackendUnavailable = errors.New("backend unavailable")
)

type Task struct {
	TaskKind    string
	Model       string
	InputBytes  []byte
	ParamsBytes []byte
}

type Usage struct {
	InputUnits  uint64
	OutputUnits uint64
	TotalUnits  uint64
}

type ExecutionResult struct {
	OutputBytes []byte
	Usage       Usage
}

type Backend interface {
	Execute(ctx context.Context, task Task) (ExecutionResult, error)
}

// Disabled is a compute backend that always fails. It is the default backend when no backend is configured.
type Disabled struct{}

func NewDisabled() *Disabled {
	return &Disabled{}
}

func (b *Disabled) Execute(context.Context, Task) (ExecutionResult, error) {
	return ExecutionResult{}, fmt.Errorf("%w: compute backend is disabled", ErrBackendUnavailable)
}
