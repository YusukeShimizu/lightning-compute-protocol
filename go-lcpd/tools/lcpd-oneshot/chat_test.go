package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func TestFormatMsatAsSat(t *testing.T) {
	tests := []struct {
		name string
		msat uint64
		want string
	}{
		{name: "WholeSat", msat: 12000, want: "12"},
		{name: "MilliSatRemainder", msat: 12345, want: "12.345"},
		{name: "Zero", msat: 0, want: "0"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := formatMsatAsSat(tc.msat)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("formatMsatAsSat mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestNormalizeAssistantReply(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "LowercasePrefix", in: "assistant: hello", want: "hello"},
		{name: "CapitalizedPrefix", in: "Assistant: hello", want: "hello"},
		{name: "NoPrefix", in: "hello", want: "hello"},
		{name: "LeadingNewlines", in: "\n\nAssistant: hello", want: "hello"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeAssistantReply(tc.in)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("normalizeAssistantReply mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestBuildChatPrompt_NoTrim_MaxBytesZero(t *testing.T) {
	history := []chatMessage{
		{Role: chatRoleUser, Content: "hi"},
		{Role: chatRoleAssistant, Content: "hello"},
		{Role: chatRoleUser, Content: "how are you?"},
	}

	gotPrompt, gotHistory, gotTrimmed, err := buildChatPrompt("be helpful", history, 0)
	if err != nil {
		t.Fatalf("buildChatPrompt returned error: %v", err)
	}

	wantPrompt := "" +
		"System: be helpful\n\n" +
		"User: hi\n" +
		"Assistant: hello\n" +
		"User: how are you?\n" +
		"Assistant: "

	if diff := cmp.Diff(wantPrompt, gotPrompt); diff != "" {
		t.Fatalf("prompt mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(history, gotHistory); diff != "" {
		t.Fatalf("history mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(false, gotTrimmed); diff != "" {
		t.Fatalf("trimmed mismatch (-want +got):\n%s", diff)
	}
}

func TestBuildChatPrompt_TrimsOldestMessages(t *testing.T) {
	history := []chatMessage{
		{Role: chatRoleUser, Content: "a"},
		{Role: chatRoleAssistant, Content: "b"},
		{Role: chatRoleUser, Content: "c"},
	}

	// baseLen = len("Assistant: ") = 11
	// plus last message "User: c\n" = 8 => 19 bytes total.
	gotPrompt, gotHistory, gotTrimmed, err := buildChatPrompt("", history, 19)
	if err != nil {
		t.Fatalf("buildChatPrompt returned error: %v", err)
	}

	wantHistory := []chatMessage{
		{Role: chatRoleUser, Content: "c"},
	}
	wantPrompt := "User: c\nAssistant: "

	if diff := cmp.Diff(wantPrompt, gotPrompt); diff != "" {
		t.Fatalf("prompt mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(wantHistory, gotHistory); diff != "" {
		t.Fatalf("history mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(true, gotTrimmed); diff != "" {
		t.Fatalf("trimmed mismatch (-want +got):\n%s", diff)
	}
}

func TestBuildChatPrompt_ErrorsWhenMaxBytesTooSmall(t *testing.T) {
	_, _, _, err := buildChatPrompt(
		"be helpful",
		[]chatMessage{{Role: chatRoleUser, Content: "hi"}},
		1,
	)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
}

func TestChatSession_Turn_Success(t *testing.T) {
	t.Parallel()

	fake := &fakeClient{
		resultText:  "Assistant: hello\n\n",
		contentType: "text/plain; charset=utf-8",
	}

	opts := runOptions{
		PeerID:           "aa",
		Model:            "gpt-5.2",
		PayInvoice:       true,
		Timeout:          time.Second,
		MaxPromptBytes:   0,
		SystemPrompt:     "",
		TemperatureMilli: 0,
		MaxOutputTokens:  0,
	}

	session := newChatSession(opts)

	out, err := session.Turn(context.Background(), fake, "hi")
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}

	if diff := cmp.Diff("hello", out.Reply); diff != "" {
		t.Fatalf("reply mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(uint64(1500), out.PriceMsat); diff != "" {
		t.Fatalf("price mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(uint64(1500), out.TotalMsat); diff != "" {
		t.Fatalf("total mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(false, out.Trimmed); diff != "" {
		t.Fatalf("trimmed mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(uint64(1500), session.totalMsat); diff != "" {
		t.Fatalf("session total mismatch (-want +got):\n%s", diff)
	}
	if gotLen := len(session.history); gotLen != 2 {
		t.Fatalf("session history len=%d want 2", gotLen)
	}
}

func TestChatSession_Turn_RollbackOnError(t *testing.T) {
	t.Parallel()

	fake := &fakeClient{
		resultText:  "irrelevant",
		contentType: "text/plain; charset=utf-8",
		acceptErr:   errors.New("boom"),
	}

	opts := runOptions{
		PeerID:         "aa",
		Model:          "gpt-5.2",
		PayInvoice:     true,
		Timeout:        time.Second,
		MaxPromptBytes: 0,
	}

	session := newChatSession(opts)

	_, err := session.Turn(context.Background(), fake, "hi")
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if gotLen := len(session.history); gotLen != 0 {
		t.Fatalf("session history len=%d want 0", gotLen)
	}
	if diff := cmp.Diff(uint64(0), session.totalMsat); diff != "" {
		t.Fatalf("session total mismatch (-want +got):\n%s", diff)
	}
}

func TestChatSession_Turn_ReportsTrimmed(t *testing.T) {
	t.Parallel()

	fake := &fakeClient{
		resultText:  "ok",
		contentType: "text/plain; charset=utf-8",
	}

	opts := runOptions{
		PeerID:         "aa",
		Model:          "gpt-5.2",
		PayInvoice:     true,
		Timeout:        time.Second,
		MaxPromptBytes: 19,
	}

	session := newChatSession(opts)
	session.history = []chatMessage{
		{Role: chatRoleUser, Content: "a"},
		{Role: chatRoleAssistant, Content: "b"},
	}

	out, err := session.Turn(context.Background(), fake, "c")
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if diff := cmp.Diff(true, out.Trimmed); diff != "" {
		t.Fatalf("trimmed mismatch (-want +got):\n%s", diff)
	}
	if gotLen := len(session.history); gotLen != 2 {
		t.Fatalf("session history len=%d want 2", gotLen)
	}
	if diff := cmp.Diff("c", session.history[0].Content); diff != "" {
		t.Fatalf("history[0] content mismatch (-want +got):\n%s", diff)
	}
}
