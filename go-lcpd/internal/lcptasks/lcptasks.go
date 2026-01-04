package lcptasks

import (
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
	lcpdv1 "github.com/bruwbird/lcp/proto-go/lcpd/v1"
)

const (
	TaskKindOpenAIChatCompletionsV1 = "openai.chat_completions.v1"
)

var (
	ErrTaskRequired     = errors.New("task is required")
	ErrTaskSpecRequired = errors.New("task spec is required")
	ErrUnsupportedTask  = errors.New("unsupported task")

	ErrOpenAIChatCompletionsV1TaskRequired = errors.New(
		"openai_chat_completions_v1 task is required",
	)
	ErrOpenAIChatCompletionsV1RequestJSONRequired = errors.New(
		"openai_chat_completions_v1.request_json is required",
	)
	ErrOpenAIChatCompletionsV1RequestJSONInvalid = errors.New(
		"openai_chat_completions_v1.request_json must be valid json",
	)
	ErrOpenAIChatCompletionsV1ParamsRequired = errors.New(
		"openai_chat_completions_v1.params is required",
	)
	ErrOpenAIChatCompletionsV1ModelRequired = errors.New(
		"openai_chat_completions_v1.params.model is required",
	)
	ErrOpenAIChatCompletionsV1ModelInvalid = errors.New(
		"openai_chat_completions_v1.params.model must be valid UTF-8",
	)
	ErrOpenAIChatCompletionsV1RequestModelRequired = errors.New(
		"openai_chat_completions_v1.request_json.model is required",
	)
	ErrOpenAIChatCompletionsV1RequestMessagesRequired = errors.New(
		"openai_chat_completions_v1.request_json.messages is required",
	)
	ErrOpenAIChatCompletionsV1ModelMismatch = errors.New(
		"openai_chat_completions_v1.params.model must match request_json.model",
	)
	ErrOpenAIChatCompletionsV1StreamingUnsupported = errors.New(
		"openai_chat_completions_v1.request_json.stream must be omitted or false",
	)
)

type QuoteRequestTask struct {
	TaskKind string

	ParamsBytes []byte
}

type InputStream struct {
	DecodedBytes    []byte
	ContentType     string
	ContentEncoding string
}

const (
	ContentTypeTextPlainUTF8       = "text/plain; charset=utf-8"
	ContentTypeApplicationJSONUTF8 = "application/json; charset=utf-8"
	ContentEncodingIdentity        = "identity"
)

// ValidateTask ensures the proto Task adheres to the strict LCP v0.2 rules.
func ValidateTask(task *lcpdv1.Task) error {
	if task == nil {
		return ErrTaskRequired
	}

	switch spec := task.GetSpec().(type) {
	case nil:
		return ErrTaskSpecRequired
	case *lcpdv1.Task_OpenaiChatCompletionsV1:
		return validateOpenAIChatCompletionsV1(spec.OpenaiChatCompletionsV1)
	default:
		return ErrUnsupportedTask
	}
}

// ToWireQuoteRequestTask converts a proto Task into the wire representation used
// by lcp_quote_request (task_kind/params_bytes).
func ToWireQuoteRequestTask(task *lcpdv1.Task) (QuoteRequestTask, error) {
	if err := ValidateTask(task); err != nil {
		return QuoteRequestTask{}, err
	}

	switch spec := task.GetSpec().(type) {
	case *lcpdv1.Task_OpenaiChatCompletionsV1:
		openaiTask := spec.OpenaiChatCompletionsV1
		model := openaiTask.GetParams().GetModel()

		openaiParams := lcpwire.OpenAIChatCompletionsV1Params{Model: model}
		paramsBytes, err := lcpwire.EncodeOpenAIChatCompletionsV1Params(openaiParams)
		if err != nil {
			return QuoteRequestTask{}, fmt.Errorf("encode openai_chat_completions_v1 params: %w", err)
		}

		return QuoteRequestTask{
			TaskKind:    TaskKindOpenAIChatCompletionsV1,
			ParamsBytes: paramsBytes,
		}, nil
	default:
		return QuoteRequestTask{}, ErrUnsupportedTask
	}
}

// ToWireInputStream converts a proto Task into the input stream (decoded bytes
// plus metadata) as defined in `protocol/protocol.md` (LCP v0.2).
func ToWireInputStream(task *lcpdv1.Task) (InputStream, error) {
	if err := ValidateTask(task); err != nil {
		return InputStream{}, err
	}

	switch spec := task.GetSpec().(type) {
	case *lcpdv1.Task_OpenaiChatCompletionsV1:
		openaiTask := spec.OpenaiChatCompletionsV1
		reqBytes := append([]byte(nil), openaiTask.GetRequestJson()...)
		return InputStream{
			DecodedBytes:    reqBytes,
			ContentType:     ContentTypeApplicationJSONUTF8,
			ContentEncoding: ContentEncodingIdentity,
		}, nil
	default:
		return InputStream{}, ErrUnsupportedTask
	}
}

func validateOpenAIChatCompletionsV1(spec *lcpdv1.OpenAIChatCompletionsV1TaskSpec) error {
	if spec == nil {
		return ErrOpenAIChatCompletionsV1TaskRequired
	}
	if len(spec.GetRequestJson()) == 0 {
		return ErrOpenAIChatCompletionsV1RequestJSONRequired
	}

	params := spec.GetParams()
	if params == nil {
		return ErrOpenAIChatCompletionsV1ParamsRequired
	}
	if params.GetModel() == "" {
		return ErrOpenAIChatCompletionsV1ModelRequired
	}
	if !utf8.ValidString(params.GetModel()) {
		return ErrOpenAIChatCompletionsV1ModelInvalid
	}

	var parsed struct {
		Model    string            `json:"model"`
		Messages []json.RawMessage `json:"messages"`
		Stream   *bool             `json:"stream,omitempty"`
	}
	if err := json.Unmarshal(spec.GetRequestJson(), &parsed); err != nil {
		return fmt.Errorf("%w: %s", ErrOpenAIChatCompletionsV1RequestJSONInvalid, err.Error())
	}
	if parsed.Stream != nil && *parsed.Stream {
		return ErrOpenAIChatCompletionsV1StreamingUnsupported
	}
	if parsed.Model == "" {
		return ErrOpenAIChatCompletionsV1RequestModelRequired
	}
	if len(parsed.Messages) == 0 {
		return ErrOpenAIChatCompletionsV1RequestMessagesRequired
	}
	if parsed.Model != params.GetModel() {
		return ErrOpenAIChatCompletionsV1ModelMismatch
	}

	return nil
}
