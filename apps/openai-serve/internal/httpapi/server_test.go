package httpapi_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	lcpdv1 "github.com/bruwbird/lcp/apps/openai-serve/gen/go/lcpd/v1"
	"github.com/bruwbird/lcp/apps/openai-serve/internal/config"
	httpapi "github.com/bruwbird/lcp/apps/openai-serve/internal/httpapi"
	"github.com/bruwbird/lcp/apps/openai-serve/internal/openai"
	"github.com/gin-gonic/gin"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	maxRequestBodyBytes = 1 << 20
	priceMsat123        = uint64(123)
	priceMsatOverLimit  = uint64(1000)
	priceMsatLimit      = uint64(10)
	cancelWaitTimeout   = time.Second
	requestTimeout      = time.Second
)

type recordingLCPDClient struct {
	listPeersResp *lcpdv1.ListLCPPeersResponse
	listPeersErr  error

	requestQuoteReq  *lcpdv1.RequestQuoteRequest
	requestQuoteResp *lcpdv1.RequestQuoteResponse
	requestQuoteErr  error

	acceptReq  *lcpdv1.AcceptAndExecuteRequest
	acceptResp *lcpdv1.AcceptAndExecuteResponse
	acceptErr  error

	allowCancel bool
	cancelCh    chan<- *lcpdv1.CancelJobRequest
}

func (c *recordingLCPDClient) ListLCPPeers(
	_ context.Context,
	_ *lcpdv1.ListLCPPeersRequest,
	_ ...grpc.CallOption,
) (*lcpdv1.ListLCPPeersResponse, error) {
	if c.listPeersResp == nil && c.listPeersErr == nil {
		panic("ListLCPPeers was called but not configured")
	}
	return c.listPeersResp, c.listPeersErr
}

func (c *recordingLCPDClient) GetLocalInfo(
	_ context.Context,
	_ *lcpdv1.GetLocalInfoRequest,
	_ ...grpc.CallOption,
) (*lcpdv1.GetLocalInfoResponse, error) {
	panic("GetLocalInfo should not be called by openai-serve httpapi")
}

func (c *recordingLCPDClient) RequestQuote(
	_ context.Context,
	in *lcpdv1.RequestQuoteRequest,
	_ ...grpc.CallOption,
) (*lcpdv1.RequestQuoteResponse, error) {
	c.requestQuoteReq = in
	if c.requestQuoteResp == nil && c.requestQuoteErr == nil {
		panic("RequestQuote was called but not configured")
	}
	return c.requestQuoteResp, c.requestQuoteErr
}

func (c *recordingLCPDClient) AcceptAndExecute(
	_ context.Context,
	in *lcpdv1.AcceptAndExecuteRequest,
	_ ...grpc.CallOption,
) (*lcpdv1.AcceptAndExecuteResponse, error) {
	c.acceptReq = in
	if c.acceptResp == nil && c.acceptErr == nil {
		panic("AcceptAndExecute was called but not configured")
	}
	return c.acceptResp, c.acceptErr
}

func (c *recordingLCPDClient) CancelJob(
	_ context.Context,
	in *lcpdv1.CancelJobRequest,
	_ ...grpc.CallOption,
) (*lcpdv1.CancelJobResponse, error) {
	if !c.allowCancel {
		panic("CancelJob was called but not expected")
	}
	if c.cancelCh != nil {
		c.cancelCh <- in
	}
	return &lcpdv1.CancelJobResponse{Success: true}, nil
}

func newTestHandler(t *testing.T, cfg config.Config, lcpdClient lcpdv1.LCPDServiceClient) http.Handler {
	t.Helper()
	gin.SetMode(gin.TestMode)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := httpapi.New(cfg, lcpdClient, logger)
	if err != nil {
		t.Fatalf("httpapi.New() error: %v", err)
	}
	return s.Handler()
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal() error: %v", err)
	}
	return b
}

func mustUnmarshalJSON(t *testing.T, b []byte, dst any) {
	t.Helper()
	if err := json.Unmarshal(b, dst); err != nil {
		t.Fatalf("json.Unmarshal() error: %v", err)
	}
}

func mustDiff[T any](t *testing.T, want, got T, opts ...cmp.Option) {
	t.Helper()
	if diff := cmp.Diff(want, got, opts...); diff != "" {
		t.Fatalf("mismatch (-want +got):\n%s", diff)
	}
}

func TestChatCompletions_Success(t *testing.T) {
	const (
		model    = "gpt-5.2"
		peerID   = "021111111111111111111111111111111111111111111111111111111111111111"
		jobIDStr = "job-123"
		termsStr = "terms-hash-123"
	)

	wantRespBytes := mustJSON(t, map[string]any{
		"id":      "chatcmpl-123",
		"object":  "chat.completion",
		"created": 123,
		"model":   model,
		"choices": []map[string]any{
			{"index": 0, "message": map[string]any{"role": "assistant", "content": "こんにちは"}, "finish_reason": "stop"},
		},
	})

	client := &recordingLCPDClient{
		listPeersResp: &lcpdv1.ListLCPPeersResponse{
			Peers: []*lcpdv1.LCPPeer{
				{
					PeerId: peerID,
					RemoteManifest: &lcpdv1.LCPManifest{
						SupportedTasks: []*lcpdv1.LCPTaskTemplate{
							{
								Kind: lcpdv1.LCPTaskKind_LCP_TASK_KIND_OPENAI_CHAT_COMPLETIONS_V1,
								ParamsTemplate: &lcpdv1.LCPTaskTemplate_OpenaiChatCompletionsV1{
									OpenaiChatCompletionsV1: &lcpdv1.OpenAIChatCompletionsV1Params{Model: model},
								},
							},
						},
					},
				},
			},
		},
		requestQuoteResp: &lcpdv1.RequestQuoteResponse{
			PeerId: peerID,
			Terms: &lcpdv1.Terms{
				JobId:     []byte(jobIDStr),
				PriceMsat: priceMsat123,
				TermsHash: []byte(termsStr),
			},
		},
		acceptResp: &lcpdv1.AcceptAndExecuteResponse{
			Result: &lcpdv1.Result{
				Status:          lcpdv1.Result_STATUS_OK,
				Result:          wantRespBytes,
				ContentType:     "application/json; charset=utf-8",
				ContentEncoding: "identity",
			},
		},
	}

	cfg := config.Config{
		TimeoutQuote:   requestTimeout,
		TimeoutExecute: requestTimeout,
	}
	h := newTestHandler(t, cfg, client)

	reqBody := mustJSON(t, map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "user", "content": "Say hello in Japanese."},
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	mustDiff(t, http.StatusOK, rec.Code)
	mustDiff(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"))
	mustDiff(t, wantRespBytes, rec.Body.Bytes())

	mustDiff(t, peerID, rec.Header().Get("X-Lcp-Peer-Id"))
	mustDiff(t, hex.EncodeToString([]byte(jobIDStr)), rec.Header().Get("X-Lcp-Job-Id"))
	mustDiff(t, "123", rec.Header().Get("X-Lcp-Price-Msat"))
	mustDiff(t, hex.EncodeToString([]byte(termsStr)), rec.Header().Get("X-Lcp-Terms-Hash"))

	if client.requestQuoteReq == nil {
		t.Fatalf("expected RequestQuote to be called")
	}
	if client.acceptReq == nil {
		t.Fatalf("expected AcceptAndExecute to be called")
	}

	mustDiff(t, peerID, client.requestQuoteReq.GetPeerId())
	mustDiff(t, peerID, client.acceptReq.GetPeerId())
	mustDiff(t, []byte(jobIDStr), client.acceptReq.GetJobId())
	mustDiff(t, true, client.acceptReq.GetPayInvoice())

	openaiTask := client.requestQuoteReq.GetTask().GetOpenaiChatCompletionsV1()
	if openaiTask == nil {
		t.Fatalf("expected openai_chat_completions_v1 task")
	}
	mustDiff(t, model, openaiTask.GetParams().GetModel())
	mustDiff(t, reqBody, openaiTask.GetRequestJson())
}

func TestChatCompletions_Passthrough_AllowsExtraFields(t *testing.T) {
	const (
		model  = "gpt-5.2"
		peerID = "021111111111111111111111111111111111111111111111111111111111111111"
	)

	tests := []struct {
		name       string
		extra      map[string]any
		wantErrMsg string
	}{
		{name: "unknown field", extra: map[string]any{"unknown_field": true}},
		{
			name: "tools field",
			extra: map[string]any{
				"tools": []map[string]any{
					{
						"type": "function",
						"function": map[string]any{
							"name":        "get_weather",
							"description": "get the current weather",
							"parameters": map[string]any{
								"type":       "object",
								"properties": map[string]any{"city": map[string]any{"type": "string"}},
								"required":   []string{"city"},
							},
						},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wantRespBytes := mustJSON(t, map[string]any{"ok": true})

			client := &recordingLCPDClient{
				listPeersResp: &lcpdv1.ListLCPPeersResponse{
					Peers: []*lcpdv1.LCPPeer{
						{
							PeerId: peerID,
							RemoteManifest: &lcpdv1.LCPManifest{
								SupportedTasks: []*lcpdv1.LCPTaskTemplate{
									{
										Kind: lcpdv1.LCPTaskKind_LCP_TASK_KIND_OPENAI_CHAT_COMPLETIONS_V1,
										ParamsTemplate: &lcpdv1.LCPTaskTemplate_OpenaiChatCompletionsV1{
											OpenaiChatCompletionsV1: &lcpdv1.OpenAIChatCompletionsV1Params{
												Model: model,
											},
										},
									},
								},
							},
						},
					},
				},
				requestQuoteResp: &lcpdv1.RequestQuoteResponse{
					PeerId: peerID,
					Terms: &lcpdv1.Terms{
						JobId:     []byte("job-1"),
						PriceMsat: priceMsat123,
						TermsHash: []byte("terms-1"),
					},
				},
				acceptResp: &lcpdv1.AcceptAndExecuteResponse{
					Result: &lcpdv1.Result{
						Status:          lcpdv1.Result_STATUS_OK,
						Result:          wantRespBytes,
						ContentType:     "application/json; charset=utf-8",
						ContentEncoding: "identity",
					},
				},
			}

			cfg := config.Config{
				TimeoutQuote:   requestTimeout,
				TimeoutExecute: requestTimeout,
			}
			h := newTestHandler(t, cfg, client)

			bodyMap := map[string]any{
				"model": model,
				"messages": []map[string]any{
					{"role": "user", "content": "hi"},
				},
			}
			maps.Copy(bodyMap, tt.extra)
			reqBody := mustJSON(t, bodyMap)

			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(reqBody))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			mustDiff(t, http.StatusOK, rec.Code)
			mustDiff(t, wantRespBytes, rec.Body.Bytes())

			openaiTask := client.requestQuoteReq.GetTask().GetOpenaiChatCompletionsV1()
			if openaiTask == nil {
				t.Fatalf("expected openai_chat_completions_v1 task")
			}
			mustDiff(t, reqBody, openaiTask.GetRequestJson())
		})
	}
}

func TestChatCompletions_RejectsStreamTrue(t *testing.T) {
	client := &recordingLCPDClient{}
	cfg := config.Config{
		TimeoutQuote:   requestTimeout,
		TimeoutExecute: requestTimeout,
	}
	h := newTestHandler(t, cfg, client)

	reqBody := mustJSON(t, map[string]any{
		"model": "gpt-5.2",
		"messages": []map[string]any{
			{"role": "user", "content": "hi"},
		},
		"stream": true,
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	mustDiff(t, http.StatusBadRequest, rec.Code)

	var got openai.ErrorResponse
	mustUnmarshalJSON(t, rec.Body.Bytes(), &got)
	mustDiff(t, "stream=true is not supported", got.Error.Message)
}

func TestChatCompletions_RequestBodyTooLarge(t *testing.T) {
	client := &recordingLCPDClient{}
	cfg := config.Config{
		TimeoutQuote:   requestTimeout,
		TimeoutExecute: requestTimeout,
	}
	h := newTestHandler(t, cfg, client)

	largeContent := strings.Repeat("a", maxRequestBodyBytes+len("x"))
	body := []byte(`{"model":"gpt-5.2","messages":[{"role":"user","content":"` + largeContent + `"}]}`)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	mustDiff(t, http.StatusRequestEntityTooLarge, rec.Code)

	var got openai.ErrorResponse
	mustUnmarshalJSON(t, rec.Body.Bytes(), &got)
	mustDiff(t, "request body too large", got.Error.Message)
}

func TestAuthMiddleware_ProtectsV1RoutesOnly(t *testing.T) {
	const apiKey = "devkey1"

	client := &recordingLCPDClient{}
	cfg := config.Config{
		APIKeys: map[string]struct{}{apiKey: {}},

		TimeoutQuote:   requestTimeout,
		TimeoutExecute: requestTimeout,
		ModelAllowlist: map[string]struct{}{"gpt-5.2": {}},
	}
	h := newTestHandler(t, cfg, client)

	t.Run("healthz is not protected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		mustDiff(t, http.StatusOK, rec.Code)
	})

	t.Run("v1 models is protected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		mustDiff(t, http.StatusUnauthorized, rec.Code)
		mustDiff(t, `Bearer realm="openai-serve"`, rec.Header().Get("WWW-Authenticate"))

		var got openai.ErrorResponse
		mustUnmarshalJSON(t, rec.Body.Bytes(), &got)
		mustDiff(t, "missing bearer token", got.Error.Message)
	})

	t.Run("v1 models allows valid key", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+apiKey)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		mustDiff(t, http.StatusOK, rec.Code)
	})
}

func TestChatCompletions_RejectsQuotePriceOverLimit(t *testing.T) {
	const (
		model  = "gpt-5.2"
		peerID = "031111111111111111111111111111111111111111111111111111111111111111"
	)

	client := &recordingLCPDClient{
		listPeersResp: &lcpdv1.ListLCPPeersResponse{
			Peers: []*lcpdv1.LCPPeer{{PeerId: peerID}},
		},
		requestQuoteResp: &lcpdv1.RequestQuoteResponse{
			PeerId: peerID,
			Terms: &lcpdv1.Terms{
				JobId:     []byte("job"),
				PriceMsat: priceMsatOverLimit,
				TermsHash: []byte("terms"),
			},
		},
	}

	cfg := config.Config{
		TimeoutQuote:        requestTimeout,
		TimeoutExecute:      requestTimeout,
		AllowUnlistedModels: true,
		MaxPriceMsat:        priceMsatLimit,
	}
	h := newTestHandler(t, cfg, client)

	reqBody := mustJSON(t, map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "user", "content": "hi"},
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	mustDiff(t, http.StatusBadRequest, rec.Code)

	var got openai.ErrorResponse
	mustUnmarshalJSON(t, rec.Body.Bytes(), &got)
	if !strings.Contains(got.Error.Message, "quote price exceeds limit") {
		t.Fatalf("expected price limit error, got %q", got.Error.Message)
	}
}

func TestChatCompletions_CancelsJobOnCanceledExecute(t *testing.T) {
	const (
		model  = "gpt-5.2"
		peerID = "021111111111111111111111111111111111111111111111111111111111111111"
		jobID  = "job-abc"
	)

	cancelCh := make(chan *lcpdv1.CancelJobRequest, len("x"))
	client := &recordingLCPDClient{
		listPeersResp: &lcpdv1.ListLCPPeersResponse{
			Peers: []*lcpdv1.LCPPeer{{PeerId: peerID}},
		},
		requestQuoteResp: &lcpdv1.RequestQuoteResponse{
			PeerId: peerID,
			Terms: &lcpdv1.Terms{
				JobId:     []byte(jobID),
				PriceMsat: uint64(len("x")),
				TermsHash: []byte("terms"),
			},
		},
		acceptErr:   status.Error(codes.Canceled, "request canceled"),
		allowCancel: true,
		cancelCh:    cancelCh,
	}

	cfg := config.Config{
		TimeoutQuote:        requestTimeout,
		TimeoutExecute:      requestTimeout,
		AllowUnlistedModels: true,
	}
	h := newTestHandler(t, cfg, client)

	reqBody := mustJSON(t, map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "user", "content": "hi"},
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	mustDiff(t, http.StatusRequestTimeout, rec.Code)

	var got openai.ErrorResponse
	mustUnmarshalJSON(t, rec.Body.Bytes(), &got)
	mustDiff(t, "request canceled", got.Error.Message)

	select {
	case cancelReq := <-cancelCh:
		mustDiff(t, peerID, cancelReq.GetPeerId())
		mustDiff(t, []byte(jobID), cancelReq.GetJobId())
	case <-time.After(cancelWaitTimeout):
		t.Fatalf("expected CancelJob to be called")
	}
}
