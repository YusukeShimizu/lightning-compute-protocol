package openai_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bruwbird/lcp/go-lcpd/internal/computebackend"
	openaibackend "github.com/bruwbird/lcp/go-lcpd/internal/computebackend/openai"
	"github.com/google/go-cmp/cmp"
)

const testAPIKey = "test-key"

func newChatCompletionsServer(t *testing.T) (*httptest.Server, <-chan map[string]any) {
	t.Helper()

	reqCh := make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/v1/chat/completions"; got != want {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if got, want := r.Header.Get("Authorization"), "Bearer "+testAPIKey; got != want {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		var req map[string]any
		if decodeErr := json.NewDecoder(r.Body).Decode(&req); decodeErr != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		reqCh <- req

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{
				map[string]any{
					"message": map[string]any{"content": "hello"},
				},
			},
			"usage": map[string]any{
				"prompt_tokens":     3,
				"completion_tokens": 5,
				"total_tokens":      8,
			},
		})
	}))
	t.Cleanup(srv.Close)

	return srv, reqCh
}

func mustRecvRequest(t *testing.T, reqCh <-chan map[string]any) map[string]any {
	t.Helper()

	select {
	case req := <-reqCh:
		return req
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for request")
		return nil
	}
}

func requireModelField(t *testing.T, req map[string]any, want string) {
	t.Helper()

	got, ok := req["model"].(string)
	if !ok {
		t.Fatalf("expected model to be string, got %T", req["model"])
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("model mismatch (-want +got):\n%s", diff)
	}
}

func requireFloatField(t *testing.T, req map[string]any, key string, want float64) {
	t.Helper()

	got, ok := req[key].(float64)
	if !ok {
		t.Fatalf("expected %s to be float64, got %T", key, req[key])
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("%s mismatch (-want +got):\n%s", key, diff)
	}
}

func requireNoField(t *testing.T, req map[string]any, key string) {
	t.Helper()

	if _, ok := req[key]; ok {
		t.Fatalf("expected %s to be omitted", key)
	}
}

func requireStringArrayField(t *testing.T, req map[string]any, key string, want []string) {
	t.Helper()

	raw, ok := req[key].([]any)
	if !ok {
		t.Fatalf("expected %s to be array, got %T", key, req[key])
	}
	if diff := cmp.Diff(len(want), len(raw)); diff != "" {
		t.Fatalf("%s length mismatch (-want +got):\n%s", key, diff)
	}
	for i := range want {
		gotElem, isString := raw[i].(string)
		if !isString {
			t.Fatalf("expected %s[%d] to be string, got %T", key, i, raw[i])
		}
		if diff := cmp.Diff(want[i], gotElem); diff != "" {
			t.Fatalf("%s[%d] mismatch (-want +got):\n%s", key, i, diff)
		}
	}
}

func TestBackend_Execute_Success(t *testing.T) {
	t.Parallel()

	const model = "test-model-success"

	srv, reqCh := newChatCompletionsServer(t)

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

	got, err := backend.Execute(context.Background(), computebackend.Task{
		TaskKind:   "llm.chat",
		Model:      model,
		InputBytes: []byte("prompt"),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	req := mustRecvRequest(t, reqCh)
	requireModelField(t, req, model)

	want := computebackend.ExecutionResult{
		OutputBytes: []byte("hello"),
		Usage: computebackend.Usage{
			InputUnits:  3,
			OutputUnits: 5,
			TotalUnits:  8,
		},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("result mismatch (-want +got):\n%s", diff)
	}
}

func TestBackend_Execute_IncludesMaxOutputTokensWhenParamsProvided(t *testing.T) {
	t.Parallel()

	const model = "test-model-max"

	srv, reqCh := newChatCompletionsServer(t)

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

	_, err = backend.Execute(context.Background(), computebackend.Task{
		TaskKind:    "llm.chat",
		Model:       model,
		InputBytes:  []byte("prompt"),
		ParamsBytes: []byte(`{"max_output_tokens":77}`),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	req := mustRecvRequest(t, reqCh)
	requireModelField(t, req, model)
	requireNoField(t, req, "max_tokens")
	requireFloatField(t, req, "max_completion_tokens", 77)
}

func TestBackend_Execute_IncludesSupportedParamsWhenProvided(t *testing.T) {
	t.Parallel()

	const model = "test-model-params"

	srv, reqCh := newChatCompletionsServer(t)

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

	_, err = backend.Execute(context.Background(), computebackend.Task{
		TaskKind:   "llm.chat",
		Model:      model,
		InputBytes: []byte("prompt"),
		ParamsBytes: []byte(
			`{"max_output_tokens":77,"temperature":0.7,"top_p":0.9,"stop":["END"],` +
				`"presence_penalty":1.1,"frequency_penalty":-0.4,"seed":123}`,
		),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	req := mustRecvRequest(t, reqCh)
	requireModelField(t, req, model)
	requireNoField(t, req, "max_tokens")
	requireFloatField(t, req, "max_completion_tokens", 77)
	requireFloatField(t, req, "temperature", 0.7)
	requireFloatField(t, req, "top_p", 0.9)
	requireFloatField(t, req, "presence_penalty", 1.1)
	requireFloatField(t, req, "frequency_penalty", -0.4)
	requireFloatField(t, req, "seed", 123)
	requireStringArrayField(t, req, "stop", []string{"END"})
}

func TestBackend_Execute_UsesLegacyMaxTokensWhenParamsProvided(t *testing.T) {
	t.Parallel()

	const model = "test-model-legacy"

	srv, reqCh := newChatCompletionsServer(t)

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

	_, err = backend.Execute(context.Background(), computebackend.Task{
		TaskKind:    "llm.chat",
		Model:       model,
		InputBytes:  []byte("prompt"),
		ParamsBytes: []byte(`{"max_tokens":77}`),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	req := mustRecvRequest(t, reqCh)
	requireModelField(t, req, model)
	requireNoField(t, req, "max_tokens")
	requireFloatField(t, req, "max_completion_tokens", 77)
}

func TestBackend_Execute_RejectsUnknownParamsKeys(t *testing.T) {
	t.Parallel()

	backend, err := openaibackend.New(openaibackend.Config{
		APIKey:  "test-key",
		BaseURL: "http://example.com",
		HTTPClient: &http.Client{
			Timeout: 2 * time.Second,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, execErr := backend.Execute(context.Background(), computebackend.Task{
		TaskKind:    "llm.chat",
		Model:       "test-model",
		InputBytes:  []byte("prompt"),
		ParamsBytes: []byte(`{"max_output_tokens":77,"unknown":1}`),
	})
	if execErr == nil {
		t.Fatalf("Execute: expected error")
	}
	if !errors.Is(execErr, computebackend.ErrInvalidTask) {
		t.Fatalf("expected invalid task error, got %v", execErr)
	}
}

func TestBackend_Execute_Unauthorized(t *testing.T) {
	t.Parallel()

	const apiKey = "test-key"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":{"message":"bad key"}}`, http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	backend, err := openaibackend.New(openaibackend.Config{APIKey: apiKey, BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, execErr := backend.Execute(context.Background(), computebackend.Task{
		TaskKind:   "llm.chat",
		Model:      "test-model",
		InputBytes: []byte("prompt"),
	})
	if execErr == nil {
		t.Fatalf("Execute: expected error")
	}
	if !errors.Is(execErr, computebackend.ErrUnauthenticated) {
		t.Fatalf("Execute: expected unauthenticated error, got %v", execErr)
	}
}
