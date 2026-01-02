package openai_test

import (
	"testing"

	"github.com/bruwbird/lcp/apps/openai-serve/internal/openai"
	"github.com/google/go-cmp/cmp"
)

func TestBuildPrompt_Basic(t *testing.T) {
	got, err := openai.BuildPrompt([]openai.ChatMessage{
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "user", Content: "Say hello."},
	})
	if err != nil {
		t.Fatalf("BuildPrompt() error: %v", err)
	}

	want := "<SYSTEM>\nYou are a helpful assistant.\n</SYSTEM>\n<CONVERSATION>\nUser: Say hello.\n</CONVERSATION>\nAssistant:"
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("unexpected prompt (-want +got):\n%s", diff)
	}
}

func TestBuildPrompt_UnsupportedRole(t *testing.T) {
	_, err := openai.BuildPrompt([]openai.ChatMessage{
		{Role: "tool", Content: "nope"},
	})
	if err == nil {
		t.Fatalf("expected error")
	}
}

func TestApproxTokensFromBytes(t *testing.T) {
	cases := []struct {
		bytes int
		want  int
	}{
		{bytes: 0, want: 0},
		{bytes: 1, want: 1},
		{bytes: 4, want: 1},
		{bytes: 5, want: 2},
	}
	for _, tc := range cases {
		got := openai.ApproxTokensFromBytes(tc.bytes)
		if diff := cmp.Diff(tc.want, got); diff != "" {
			t.Fatalf("ApproxTokensFromBytes(%d) mismatch (-want +got):\n%s", tc.bytes, diff)
		}
	}
}
