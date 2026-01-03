package lcptasks

import (
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
	lcpdv1 "github.com/bruwbird/lcp/proto-go/lcpd/v1"
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
	Input    []byte

	ParamsBytes   []byte
	LLMChatParams *lcpwire.LLMChatParams
}

// ValidateTask ensures the proto Task adheres to the strict LCP v0.1 rules.
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
// by lcp_quote_request (task_kind/input/params_bytes).
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

	input := []byte(llmChat.GetPrompt())

	return QuoteRequestTask{
		TaskKind:      TaskKindLLMChat,
		Input:         input,
		ParamsBytes:   paramsBytes,
		LLMChatParams: &llmParams,
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
