package openai

import (
	"bytes"
	"encoding/json"
	"strings"
)

type ChatContent string

func (c *ChatContent) UnmarshalJSON(b []byte) error {
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		*c = ""
		return nil
	}

	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*c = ChatContent(s)
		return nil
	}

	// OpenAI-compatible "content parts" array. We extract text parts and ignore others.
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text,omitempty"`
	}
	if err := json.Unmarshal(b, &parts); err != nil {
		return err
	}
	var sb strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			sb.WriteString(p.Text)
		}
	}
	*c = ChatContent(sb.String())
	return nil
}

type ChatMessage struct {
	Role    string      `json:"role"`
	Content ChatContent `json:"content"`
	Name    string      `json:"name,omitempty"`
}

type ChatCompletionsRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`

	Stream *bool `json:"stream,omitempty"`
	N      *int  `json:"n,omitempty"`

	Temperature         *float64 `json:"temperature,omitempty"`
	MaxTokens           *int     `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int     `json:"max_completion_tokens,omitempty"`

	// Accepted but not supported (explicitly rejected if present).
	TopP             *float64        `json:"top_p,omitempty"`
	Stop             json.RawMessage `json:"stop,omitempty"`
	PresencePenalty  *float64        `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64        `json:"frequency_penalty,omitempty"`
	Seed             *int64          `json:"seed,omitempty"`
	Tools            json.RawMessage `json:"tools,omitempty"`
	ToolChoice       json.RawMessage `json:"tool_choice,omitempty"`
	ResponseFormat   json.RawMessage `json:"response_format,omitempty"`
	Functions        json.RawMessage `json:"functions,omitempty"`
	FunctionCall     json.RawMessage `json:"function_call,omitempty"`
	Logprobs         json.RawMessage `json:"logprobs,omitempty"`
	TopLogprobs      json.RawMessage `json:"top_logprobs,omitempty"`

	// Ignored (common in some clients).
	User json.RawMessage `json:"user,omitempty"`
}

type ResponsesRequest struct {
	Model string `json:"model"`
	Input json.RawMessage `json:"input"`

	Stream *bool `json:"stream,omitempty"`
}

type ChatCompletionsResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"` // "chat.completion"
	Created int64  `json:"created"`
	Model   string `json:"model"`

	Choices []ChatChoice `json:"choices"`
	Usage   *Usage       `json:"usage,omitempty"`
}

type ChatChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type ModelsResponse struct {
	Object string      `json:"object"` // "list"
	Data   []ModelInfo `json:"data"`
}

type ModelInfo struct {
	ID      string `json:"id"`
	Object  string `json:"object"` // "model"
	Created int64  `json:"created,omitempty"`
	OwnedBy string `json:"owned_by,omitempty"`
}

type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

type ErrorDetail struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    *string `json:"code"`
}
