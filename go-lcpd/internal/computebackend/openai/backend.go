package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/bruwbird/lcp/go-lcpd/internal/computebackend"
	openai "github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

const defaultHTTPTimeout = 30 * time.Second

type Config struct {
	APIKey     string
	BaseURL    string
	HTTPClient *http.Client
}

type Backend struct {
	client openai.Client
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

	opts := []option.RequestOption{
		option.WithAPIKey(apiKey),
		option.WithHTTPClient(httpClient),
	}

	baseURL := strings.TrimSpace(cfg.BaseURL)
	if baseURL != "" {
		baseURL = strings.TrimRight(baseURL, "/")
		if !strings.HasSuffix(baseURL, "/v1") {
			baseURL += "/v1"
		}
		opts = append(opts, option.WithBaseURL(baseURL))
	}

	client := openai.NewClient(opts...)

	return &Backend{
		client: client,
	}, nil
}

func (b *Backend) Execute(
	ctx context.Context,
	task computebackend.Task,
) (computebackend.ExecutionResult, error) {
	if task.TaskKind != "llm.chat" {
		return computebackend.ExecutionResult{}, fmt.Errorf(
			"%w: %q",
			computebackend.ErrUnsupportedTaskKind,
			task.TaskKind,
		)
	}
	if strings.TrimSpace(task.Model) == "" {
		return computebackend.ExecutionResult{}, fmt.Errorf(
			"%w: model is required",
			computebackend.ErrInvalidTask,
		)
	}
	if len(task.InputBytes) == 0 {
		return computebackend.ExecutionResult{}, fmt.Errorf(
			"%w: input_bytes is required",
			computebackend.ErrInvalidTask,
		)
	}

	prompt := string(task.InputBytes)

	parsed, err := parseChatCompletionParams(task.ParamsBytes)
	if err != nil {
		return computebackend.ExecutionResult{}, err
	}

	params := openai.ChatCompletionNewParams{
		Model:    task.Model,
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage(prompt)},
	}
	if parsed.MaxOutputTokens != nil {
		params.MaxCompletionTokens = openai.Int(int64(*parsed.MaxOutputTokens))
	}
	if parsed.Temperature != nil {
		params.Temperature = openai.Float(*parsed.Temperature)
	}
	if parsed.TopP != nil {
		params.TopP = openai.Float(*parsed.TopP)
	}
	if parsed.Stop != nil {
		params.Stop = *parsed.Stop
	}
	if parsed.PresencePenalty != nil {
		params.PresencePenalty = openai.Float(*parsed.PresencePenalty)
	}
	if parsed.FrequencyPenalty != nil {
		params.FrequencyPenalty = openai.Float(*parsed.FrequencyPenalty)
	}
	if parsed.Seed != nil {
		params.Seed = openai.Int(*parsed.Seed)
	}

	resp, err := b.client.Chat.Completions.New(ctx, params)
	if err != nil {
		return computebackend.ExecutionResult{}, b.mapAPIError(err)
	}

	content := ""
	if len(resp.Choices) > 0 {
		content = resp.Choices[0].Message.Content
	}
	if strings.TrimSpace(content) == "" {
		return computebackend.ExecutionResult{}, fmt.Errorf(
			"%w: empty content",
			computebackend.ErrBackendUnavailable,
		)
	}

	usage, err := usageFromResponse(resp.Usage)
	if err != nil {
		return computebackend.ExecutionResult{}, err
	}

	return computebackend.ExecutionResult{
		OutputBytes: []byte(content),
		Usage:       usage,
	}, nil
}

func usageFromResponse(usage openai.CompletionUsage) (computebackend.Usage, error) {
	totalUnits, err := uint64FromNonNegativeInt64(usage.TotalTokens)
	if err != nil {
		return computebackend.Usage{}, fmt.Errorf(
			"%w: invalid total_tokens: %w",
			computebackend.ErrBackendUnavailable,
			err,
		)
	}
	inputUnits, err := uint64FromNonNegativeInt64(usage.PromptTokens)
	if err != nil {
		return computebackend.Usage{}, fmt.Errorf(
			"%w: invalid prompt_tokens: %w",
			computebackend.ErrBackendUnavailable,
			err,
		)
	}
	outputUnits, err := uint64FromNonNegativeInt64(usage.CompletionTokens)
	if err != nil {
		return computebackend.Usage{}, fmt.Errorf(
			"%w: invalid completion_tokens: %w",
			computebackend.ErrBackendUnavailable,
			err,
		)
	}
	return computebackend.Usage{
		TotalUnits:  totalUnits,
		InputUnits:  inputUnits,
		OutputUnits: outputUnits,
	}, nil
}

func uint64FromNonNegativeInt64(v int64) (uint64, error) {
	if v < 0 {
		return 0, fmt.Errorf("negative value %d", v)
	}
	return uint64(v), nil
}

func (b *Backend) mapAPIError(err error) error {
	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		msg := strings.TrimSpace(apiErr.Message)
		if msg == "" {
			msg = http.StatusText(apiErr.StatusCode)
		}

		switch apiErr.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return fmt.Errorf(
				"%w: http %d: %s",
				computebackend.ErrUnauthenticated,
				apiErr.StatusCode,
				msg,
			)
		case http.StatusBadRequest:
			return fmt.Errorf(
				"%w: http %d: %s",
				computebackend.ErrInvalidTask,
				apiErr.StatusCode,
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
				apiErr.StatusCode,
				msg,
			)
		default:
			return fmt.Errorf(
				"%w: http %d: %s",
				computebackend.ErrBackendUnavailable,
				apiErr.StatusCode,
				msg,
			)
		}
	}

	return fmt.Errorf("%w: request failed: %w", computebackend.ErrBackendUnavailable, err)
}

type chatCompletionParams struct {
	MaxOutputTokens  *uint32
	Temperature      *float64
	TopP             *float64
	Stop             *openai.ChatCompletionNewParamsStopUnion
	PresencePenalty  *float64
	FrequencyPenalty *float64
	Seed             *int64
}

func parseChatCompletionParams(paramsBytes []byte) (chatCompletionParams, error) {
	if len(paramsBytes) == 0 {
		return chatCompletionParams{}, nil
	}

	var raw struct {
		MaxOutputTokens  uint32   `json:"max_output_tokens"`
		LegacyMaxTokens  uint32   `json:"max_tokens"`
		Temperature      *float64 `json:"temperature"`
		TopP             *float64 `json:"top_p"`
		Stop             any      `json:"stop"`
		PresencePenalty  *float64 `json:"presence_penalty"`
		FrequencyPenalty *float64 `json:"frequency_penalty"`
		Seed             *int64   `json:"seed"`
	}

	dec := json.NewDecoder(bytes.NewReader(paramsBytes))
	dec.DisallowUnknownFields()
	if decodeErr := dec.Decode(&raw); decodeErr != nil {
		return chatCompletionParams{}, fmt.Errorf(
			"%w: params_bytes must be valid json: %s",
			computebackend.ErrInvalidTask,
			decodeErr.Error(),
		)
	}

	maxTokens := raw.MaxOutputTokens
	if maxTokens == 0 {
		maxTokens = raw.LegacyMaxTokens
	}
	if maxTokens == 0 {
		return chatCompletionParams{}, fmt.Errorf(
			"%w: max_output_tokens must be > 0",
			computebackend.ErrInvalidTask,
		)
	}

	var stopUnion *openai.ChatCompletionNewParamsStopUnion
	switch v := raw.Stop.(type) {
	case nil:
		// unset
	case string:
		if strings.TrimSpace(v) == "" {
			return chatCompletionParams{}, fmt.Errorf("%w: stop must be non-empty", computebackend.ErrInvalidTask)
		}
		stopUnion = &openai.ChatCompletionNewParamsStopUnion{OfString: openai.String(v)}
	case []any:
		if len(v) == 0 {
			return chatCompletionParams{}, fmt.Errorf("%w: stop must be non-empty", computebackend.ErrInvalidTask)
		}
		out := make([]string, 0, len(v))
		for _, elem := range v {
			s, ok := elem.(string)
			if !ok {
				return chatCompletionParams{}, fmt.Errorf("%w: stop must be a string or array of strings", computebackend.ErrInvalidTask)
			}
			if strings.TrimSpace(s) == "" {
				return chatCompletionParams{}, fmt.Errorf("%w: stop must be non-empty", computebackend.ErrInvalidTask)
			}
			out = append(out, s)
		}
		stopUnion = &openai.ChatCompletionNewParamsStopUnion{OfStringArray: out}
	default:
		return chatCompletionParams{}, fmt.Errorf("%w: stop must be a string or array of strings", computebackend.ErrInvalidTask)
	}

	maxTokensCopy := maxTokens
	return chatCompletionParams{
		MaxOutputTokens:  &maxTokensCopy,
		Temperature:      raw.Temperature,
		TopP:             raw.TopP,
		Stop:             stopUnion,
		PresencePenalty:  raw.PresencePenalty,
		FrequencyPenalty: raw.FrequencyPenalty,
		Seed:             raw.Seed,
	}, nil
}

var _ computebackend.Backend = (*Backend)(nil)
