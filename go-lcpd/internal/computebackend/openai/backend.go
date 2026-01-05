package openai

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bruwbird/lcp/go-lcpd/internal/computebackend"
)

const (
	defaultHTTPTimeout = 30 * time.Second
	defaultBaseURL     = "https://api.openai.com/v1"

	maxPassthroughResponseBytes  = 16 * 1024 * 1024
	maxUpstreamErrorMessageBytes = 1024
)

type Config struct {
	APIKey     string
	BaseURL    string
	HTTPClient *http.Client
}

type Backend struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
}

func New(cfg Config) (*Backend, error) {
	apiKey := strings.TrimSpace(cfg.APIKey)
	if apiKey == "" {
		return nil, fmt.Errorf("%w: api key is required", computebackend.ErrUnauthenticated)
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultHTTPTimeout}
	}

	baseURL := strings.TrimSpace(cfg.BaseURL)
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if !strings.HasSuffix(baseURL, "/v1") {
		baseURL += "/v1"
	}

	return &Backend{
		apiKey:     apiKey,
		baseURL:    baseURL,
		httpClient: httpClient,
	}, nil
}

func (b *Backend) Execute(
	ctx context.Context,
	task computebackend.Task,
) (computebackend.ExecutionResult, error) {
	streaming, err := b.ExecuteStreaming(ctx, task)
	if err != nil {
		return computebackend.ExecutionResult{}, err
	}
	defer func() { _ = streaming.Output.Close() }()

	body, err := readLimitedBody(streaming.Output, maxPassthroughResponseBytes)
	if err != nil {
		return computebackend.ExecutionResult{}, err
	}
	if len(body) == 0 {
		return computebackend.ExecutionResult{}, fmt.Errorf(
			"%w: empty response",
			computebackend.ErrBackendUnavailable,
		)
	}

	return computebackend.ExecutionResult{OutputBytes: body, Usage: streaming.Usage}, nil
}

func (b *Backend) ExecuteStreaming(
	ctx context.Context,
	task computebackend.Task,
) (computebackend.StreamingExecutionResult, error) {
	var urlPath string
	switch task.TaskKind {
	case "openai.chat_completions.v1":
		urlPath = "/chat/completions"
	case "openai.responses.v1":
		urlPath = "/responses"
	default:
		return computebackend.StreamingExecutionResult{}, fmt.Errorf(
			"%w: %q",
			computebackend.ErrUnsupportedTaskKind,
			task.TaskKind,
		)
	}

	if err := validatePassthroughTask(task); err != nil {
		return computebackend.StreamingExecutionResult{}, err
	}

	req, err := b.newPassthroughRequest(ctx, urlPath, task.InputBytes)
	if err != nil {
		return computebackend.StreamingExecutionResult{}, err
	}

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return computebackend.StreamingExecutionResult{}, fmt.Errorf(
			"%w: request failed: %s",
			computebackend.ErrBackendUnavailable,
			err.Error(),
		)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, readErr := readLimitedBody(resp.Body, maxUpstreamErrorMessageBytes)
		_ = resp.Body.Close()
		if readErr != nil {
			return computebackend.StreamingExecutionResult{}, readErr
		}
		return computebackend.StreamingExecutionResult{}, errorFromPassthroughHTTPResponse(
			resp.StatusCode,
			body,
		)
	}

	return computebackend.StreamingExecutionResult{Output: resp.Body}, nil
}

func validatePassthroughTask(task computebackend.Task) error {
	if strings.TrimSpace(task.Model) == "" {
		return fmt.Errorf(
			"%w: model is required",
			computebackend.ErrInvalidTask,
		)
	}
	if len(task.InputBytes) == 0 {
		return fmt.Errorf(
			"%w: input_bytes is required",
			computebackend.ErrInvalidTask,
		)
	}
	if len(task.ParamsBytes) != 0 {
		return fmt.Errorf(
			"%w: params_bytes must be empty for openai.chat_completions.v1",
			computebackend.ErrInvalidTask,
		)
	}
	return nil
}

func (b *Backend) newPassthroughRequest(
	ctx context.Context,
	path string,
	body []byte,
) (*http.Request, error) {
	url := b.baseURL + path

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf(
			"%w: build request: %s",
			computebackend.ErrBackendUnavailable,
			err.Error(),
		)
	}
	req.Header.Set("Authorization", "Bearer "+b.apiKey)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Accept", "*/*")
	return req, nil
}

func readLimitedBody(r io.Reader, limitBytes int) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, int64(limitBytes)+1))
	if err != nil {
		return nil, fmt.Errorf(
			"%w: read response: %s",
			computebackend.ErrBackendUnavailable,
			err.Error(),
		)
	}
	if len(body) > limitBytes {
		return nil, fmt.Errorf(
			"%w: response too large",
			computebackend.ErrBackendUnavailable,
		)
	}
	return body, nil
}

func errorFromPassthroughHTTPResponse(statusCode int, body []byte) error {
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = http.StatusText(statusCode)
	}
	if len(msg) > maxUpstreamErrorMessageBytes {
		msg = msg[:maxUpstreamErrorMessageBytes] + "…"
	}

	switch statusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf(
			"%w: http %d: %s",
			computebackend.ErrUnauthenticated,
			statusCode,
			msg,
		)
	case http.StatusBadRequest:
		return fmt.Errorf(
			"%w: http %d: %s",
			computebackend.ErrInvalidTask,
			statusCode,
			msg,
		)
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return fmt.Errorf(
			"%w: http %d: %s",
			computebackend.ErrBackendUnavailable,
			statusCode,
			msg,
		)
	default:
		return fmt.Errorf(
			"%w: http %d: %s",
			computebackend.ErrBackendUnavailable,
			statusCode,
			msg,
		)
	}
}

var _ computebackend.Backend = (*Backend)(nil)
var _ computebackend.StreamingBackend = (*Backend)(nil)
