package openai_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bruwbird/lcp/go-lcpd/internal/computebackend"
	openaibackend "github.com/bruwbird/lcp/go-lcpd/internal/computebackend/openai"
	"github.com/google/go-cmp/cmp"
)

const testAPIKey = "test-key"

func TestBackend_Execute_OpenAIChatCompletionsV1_Passthrough(t *testing.T) {
	t.Parallel()

	reqCh := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/v1/chat/completions"; got != want {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if got, want := r.Header.Get("Authorization"), "Bearer "+testAPIKey; got != want {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		reqCh <- body

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp","object":"chat.completion"}`))
	}))
	t.Cleanup(srv.Close)

	backend, err := openaibackend.New(openaibackend.Config{
		APIKey:  testAPIKey,
		BaseURL: srv.URL,
		HTTPClient: &http.Client{
			Timeout: 2 * time.Second,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	input := []byte(
		`{"model":"gpt-5.2","messages":[{"role":"user","content":"hi"}],` +
			`"tools":[{"type":"function"}]}`,
	)

	got, err := backend.Execute(context.Background(), computebackend.Task{
		TaskKind:   "openai.chat_completions.v1",
		Model:      "gpt-5.2",
		InputBytes: input,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	reqBody := mustRecvBytes(t, reqCh)
	if diff := cmp.Diff(input, reqBody); diff != "" {
		t.Fatalf("request body mismatch (-want +got):\n%s", diff)
	}

	want := computebackend.ExecutionResult{
		OutputBytes: []byte(`{"id":"resp","object":"chat.completion"}`),
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("result mismatch (-want +got):\n%s", diff)
	}
}

func TestBackend_Execute_OpenAIChatCompletionsV1_RejectsParamsBytes(t *testing.T) {
	t.Parallel()

	backend, err := openaibackend.New(openaibackend.Config{
		APIKey:  testAPIKey,
		BaseURL: "http://example.com",
		HTTPClient: &http.Client{
			Timeout: 2 * time.Second,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, execErr := backend.Execute(context.Background(), computebackend.Task{
		TaskKind:    "openai.chat_completions.v1",
		Model:       "gpt-5.2",
		InputBytes:  []byte(`{"model":"gpt-5.2","messages":[{"role":"user","content":"hi"}]}`),
		ParamsBytes: []byte(`{"unexpected":true}`),
	})
	if execErr == nil {
		t.Fatalf("Execute: expected error")
	}
	if !errors.Is(execErr, computebackend.ErrInvalidTask) {
		t.Fatalf("expected invalid task error, got %v", execErr)
	}
}

func TestBackend_Execute_OpenAIChatCompletionsV1_Unauthorized(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":{"message":"bad key"}}`, http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	backend, err := openaibackend.New(openaibackend.Config{
		APIKey:  testAPIKey,
		BaseURL: srv.URL,
		HTTPClient: &http.Client{
			Timeout: 2 * time.Second,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, execErr := backend.Execute(context.Background(), computebackend.Task{
		TaskKind:   "openai.chat_completions.v1",
		Model:      "gpt-5.2",
		InputBytes: []byte(`{"model":"gpt-5.2","messages":[{"role":"user","content":"hi"}]}`),
	})
	if execErr == nil {
		t.Fatalf("Execute: expected error")
	}
	if !errors.Is(execErr, computebackend.ErrUnauthenticated) {
		t.Fatalf("expected unauthenticated error, got %v", execErr)
	}
}

func mustRecvBytes(t *testing.T, ch <-chan []byte) []byte {
	t.Helper()

	select {
	case b := <-ch:
		return b
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for request")
		return nil
	}
}
