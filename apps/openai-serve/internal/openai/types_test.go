package openai_test

import (
	"encoding/json"
	"testing"

	"github.com/bruwbird/lcp/apps/openai-serve/internal/openai"
	"github.com/google/go-cmp/cmp"
)

func TestChatContent_Unmarshal_Null(t *testing.T) {
	var msg openai.ChatMessage
	if err := json.Unmarshal([]byte(`{"role":"assistant","content":null}`), &msg); err != nil {
		t.Fatalf("json.Unmarshal() error: %v", err)
	}
	if diff := cmp.Diff(openai.ChatContent(""), msg.Content); diff != "" {
		t.Fatalf("unexpected content (-want +got):\n%s", diff)
	}
}

func TestChatContent_Unmarshal_ContentPartsArray(t *testing.T) {
	var msg openai.ChatMessage
	if err := json.Unmarshal([]byte(`{
		"role": "user",
		"content": [
			{"type": "text", "text": "hello"},
			{"type": "image_url", "image_url": {"url": "https://example.com"} },
			{"type": "text", "text": " world"}
		]
	}`), &msg); err != nil {
		t.Fatalf("json.Unmarshal() error: %v", err)
	}
	if diff := cmp.Diff(openai.ChatContent("hello world"), msg.Content); diff != "" {
		t.Fatalf("unexpected content (-want +got):\n%s", diff)
	}
}
