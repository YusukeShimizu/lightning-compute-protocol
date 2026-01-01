package lcptasks

import (
	"errors"
	"fmt"
	"unicode/utf8"

	lcpdv1 "github.com/bruwbird/lcp/go-lcpd/gen/go/lcpd/v1"
	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
)

const (
	TaskKindLLMChat = "llm.chat"
)

var (
	ErrTaskRequired           = errors.New("task is required")
	ErrTaskSpecRequired       = errors.New("task spec is required")
	ErrUnsupportedTask        = errors.New("unsupported task")
	ErrLLMChatTaskRequired    = errors.New("llm_chat task is required")
	ErrLLMChatPromptRequired  = errors.New("llm_chat.prompt is required")
	ErrLLMChatPromptInvalid   = errors.New("llm_chat.prompt must be valid UTF-8")
	ErrLLMChatParamsRequired  = errors.New("llm_chat.params is required")
	ErrLLMChatProfileRequired = errors.New("llm_chat.params.profile is required")
	ErrLLMChatProfileInvalid  = errors.New("llm_chat.params.profile must be valid UTF-8")
)

type QuoteRequestTask struct {
	TaskKind string

	ParamsBytes   []byte
	LLMChatParams *lcpwire.LLMChatParams
}

type InputStream struct {
	DecodedBytes    []byte
	ContentType     string
	ContentEncoding string
}

const (
	ContentTypeTextPlainUTF8 = "text/plain; charset=utf-8"
	ContentEncodingIdentity  = "identity"
)

// ValidateTask ensures the proto Task adheres to the strict LCP v0.2 rules.
func ValidateTask(task *lcpdv1.Task) error {
	if task == nil {
		return ErrTaskRequired
	}

	switch spec := task.GetSpec().(type) {
	case nil:
		return ErrTaskSpecRequired
	case *lcpdv1.Task_LlmChat:
		return validateLLMChat(spec.LlmChat)
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

	llmChat := task.GetLlmChat()
	params := llmChat.GetParams()

	llmParams := lcpwire.LLMChatParams{
		Profile: params.GetProfile(),
	}
	if v := params.GetTemperatureMilli(); v != 0 {
		llmParams.TemperatureMilli = &v
	}
	if v := params.GetMaxOutputTokens(); v != 0 {
		llmParams.MaxOutputTokens = &v
	}

	paramsBytes, err := lcpwire.EncodeLLMChatParams(llmParams)
	if err != nil {
		return QuoteRequestTask{}, fmt.Errorf("encode llm_chat params: %w", err)
	}

	return QuoteRequestTask{
		TaskKind:      TaskKindLLMChat,
		ParamsBytes:   paramsBytes,
		LLMChatParams: &llmParams,
	}, nil
}

// ToWireInputStream converts a proto Task into the input stream (decoded bytes
// plus metadata) as defined in `protocol/protocol.md` (LCP v0.2).
func ToWireInputStream(task *lcpdv1.Task) (InputStream, error) {
	if err := ValidateTask(task); err != nil {
		return InputStream{}, err
	}

	llmChat := task.GetLlmChat()

	return InputStream{
		DecodedBytes:    []byte(llmChat.GetPrompt()),
		ContentType:     ContentTypeTextPlainUTF8,
		ContentEncoding: ContentEncodingIdentity,
	}, nil
}

func validateLLMChat(spec *lcpdv1.LLMChatTaskSpec) error {
	if spec == nil {
		return ErrLLMChatTaskRequired
	}
	if spec.GetPrompt() == "" {
		return ErrLLMChatPromptRequired
	}
	if !utf8.ValidString(spec.GetPrompt()) {
		return ErrLLMChatPromptInvalid
	}

	params := spec.GetParams()
	if params == nil {
		return ErrLLMChatParamsRequired
	}
	if params.GetProfile() == "" {
		return ErrLLMChatProfileRequired
	}
	if !utf8.ValidString(params.GetProfile()) {
		return ErrLLMChatProfileInvalid
	}

	return nil
}
