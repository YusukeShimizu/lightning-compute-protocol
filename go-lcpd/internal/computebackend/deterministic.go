package computebackend

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Deterministic returns a fixed output for every Execute call.
type Deterministic struct {
	Output []byte
}

func NewDeterministic(output []byte) *Deterministic {
	return &Deterministic{Output: append([]byte(nil), output...)}
}

func NewDeterministicFromBase64(b64 string) (*Deterministic, error) {
	trimmed := strings.TrimSpace(b64)
	if trimmed == "" {
		return nil, errors.New("deterministic output base64 is empty")
	}

	decoded, err := base64.StdEncoding.DecodeString(trimmed)
	if err != nil {
		return nil, fmt.Errorf("decode deterministic output: %w", err)
	}
	if len(decoded) == 0 {
		return nil, errors.New("deterministic output decoded to empty")
	}
	return NewDeterministic(decoded), nil
}

func (b *Deterministic) Execute(context.Context, Task) (ExecutionResult, error) {
	return ExecutionResult{
		OutputBytes: append([]byte(nil), b.Output...),
	}, nil
}

func (b *Deterministic) ExecuteStreaming(context.Context, Task) (StreamingExecutionResult, error) {
	return StreamingExecutionResult{
		Output: io.NopCloser(bytes.NewReader(b.Output)),
	}, nil
}
