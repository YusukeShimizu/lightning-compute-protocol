package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
	lcpdv1 "github.com/bruwbird/lcp/proto-go/lcpd/v1"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc"
)

func TestRunPipeline_RequestQuoteOnly(t *testing.T) {
	t.Parallel()

	fake := &fakeClient{
		resultText:  "ok",
		contentType: "text/plain; charset=utf-8",
	}

	opts := runOptions{
		PeerID:           strings.Repeat("a", 66),
		Model:            "gpt-5.2",
		TemperatureMilli: 700,
		MaxOutputTokens:  256,
		PayInvoice:       false,
		Prompt:           "ping",
	}

	res, err := runPipeline(context.Background(), fake, opts)
	if err != nil {
		t.Fatalf("runPipeline: %v", err)
	}

	if fake.requestQuoteReq == nil {
		t.Fatalf("RequestQuote was not called")
	}
	if fake.acceptReq != nil {
		t.Fatalf("AcceptAndExecute must not be called when pay-invoice=false")
	}

	if diff := cmp.Diff(opts.PeerID, fake.requestQuoteReq.GetPeerId()); diff != "" {
		t.Fatalf("peer_id mismatch (-want +got):\n%s", diff)
	}
	openaiTask := fake.requestQuoteReq.GetTask().GetOpenaiChatCompletionsV1()
	if openaiTask == nil {
		t.Fatalf("task.openai_chat_completions_v1 is nil")
	}
	params := openaiTask.GetParams()
	if params == nil {
		t.Fatalf("task.openai_chat_completions_v1.params is nil")
	}

	if diff := cmp.Diff(opts.Model, params.GetModel()); diff != "" {
		t.Fatalf("params.model mismatch (-want +got):\n%s", diff)
	}

	var req struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Temperature *float64 `json:"temperature,omitempty"`
		MaxTokens   *uint32  `json:"max_tokens,omitempty"`
	}
	if unmarshalErr := json.Unmarshal(openaiTask.GetRequestJson(), &req); unmarshalErr != nil {
		t.Fatalf("unmarshal request_json: %v", unmarshalErr)
	}
	if diff := cmp.Diff(opts.Model, req.Model); diff != "" {
		t.Fatalf("request_json.model mismatch (-want +got):\n%s", diff)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("request_json.messages length mismatch: got %d want 1", len(req.Messages))
	}
	if diff := cmp.Diff("user", req.Messages[0].Role); diff != "" {
		t.Fatalf("request_json.messages[0].role mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(opts.Prompt, req.Messages[0].Content); diff != "" {
		t.Fatalf("request_json.messages[0].content mismatch (-want +got):\n%s", diff)
	}

	if req.Temperature == nil {
		t.Fatalf("request_json.temperature is nil")
	}
	if got, want := *req.Temperature, 0.7; math.Abs(got-want) > 1e-9 {
		t.Fatalf("request_json.temperature mismatch: got %v want %v", got, want)
	}

	if req.MaxTokens == nil {
		t.Fatalf("request_json.max_tokens is nil")
	}
	if diff := cmp.Diff(opts.MaxOutputTokens, *req.MaxTokens); diff != "" {
		t.Fatalf("request_json.max_tokens mismatch (-want +got):\n%s", diff)
	}

	if res.Terms == nil {
		t.Fatalf("Terms is nil")
	}
	if res.Result != nil {
		t.Fatalf("Result must be nil when pay-invoice=false")
	}
}

func TestRunPipeline_RequestQuoteAndAcceptAndExecute(t *testing.T) {
	t.Parallel()

	fake := &fakeClient{
		resultText:  "ok",
		contentType: "text/plain; charset=utf-8",
	}

	opts := runOptions{
		PeerID:           strings.Repeat("b", 66),
		Model:            "gpt-5.2",
		TemperatureMilli: 0,
		MaxOutputTokens:  0,
		PayInvoice:       true,
		Prompt:           "ping",
	}

	res, err := runPipeline(context.Background(), fake, opts)
	if err != nil {
		t.Fatalf("runPipeline: %v", err)
	}

	if fake.requestQuoteReq == nil {
		t.Fatalf("RequestQuote was not called")
	}
	if fake.acceptReq == nil {
		t.Fatalf("AcceptAndExecute was not called")
	}

	if diff := cmp.Diff(opts.PeerID, fake.acceptReq.GetPeerId()); diff != "" {
		t.Fatalf("accept peer_id mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(fake.terms.GetJobId(), fake.acceptReq.GetJobId()); diff != "" {
		t.Fatalf("accept job_id mismatch (-want +got):\n%s", diff)
	}
	if got := fake.acceptReq.GetPayInvoice(); got != true {
		t.Fatalf("accept pay_invoice: got %v want true", got)
	}

	if res.Result == nil {
		t.Fatalf("Result is nil")
	}
	if diff := cmp.Diff([]byte(fake.resultText), res.Result.GetResult()); diff != "" {
		t.Fatalf("result mismatch (-want +got):\n%s", diff)
	}
}

func TestResolvePromptPreference(t *testing.T) {
	t.Parallel()

	t.Run("flag wins", func(t *testing.T) {
		t.Parallel()

		got, err := resolvePrompt(
			"from-flag",
			[]string{"from-arg"},
			strings.NewReader("from-stdin\n"),
		)
		if err != nil {
			t.Fatalf("resolvePrompt: %v", err)
		}
		if diff := cmp.Diff("from-flag", got); diff != "" {
			t.Fatalf("prompt mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("arg fallback", func(t *testing.T) {
		t.Parallel()

		got, err := resolvePrompt("", []string{"from-arg"}, strings.NewReader("from-stdin\n"))
		if err != nil {
			t.Fatalf("resolvePrompt: %v", err)
		}
		if diff := cmp.Diff("from-arg", got); diff != "" {
			t.Fatalf("prompt mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("stdin fallback", func(t *testing.T) {
		t.Parallel()

		got, err := resolvePrompt("", nil, strings.NewReader("from-stdin\n"))
		if err != nil {
			t.Fatalf("resolvePrompt: %v", err)
		}
		if diff := cmp.Diff("from-stdin", got); diff != "" {
			t.Fatalf("prompt mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("empty error", func(t *testing.T) {
		t.Parallel()

		_, err := resolvePrompt("", nil, strings.NewReader(""))
		if err == nil {
			t.Fatalf("resolvePrompt: want error, got nil")
		}
		if !errors.Is(err, errPromptEmpty) {
			t.Fatalf("resolvePrompt: want errPromptEmpty, got %v", err)
		}
	})
}

func TestFormatHumanSummary(t *testing.T) {
	t.Parallel()

	res := pipelineResult{
		PeerID: strings.Repeat("a", 66),
		Terms: &lcpdv1.Terms{
			JobId:          bytes.Repeat([]byte{0xbb}, 32),
			TermsHash:      bytes.Repeat([]byte{0xaa}, 32),
			PriceMsat:      1500,
			PaymentRequest: "lnbc1testinvoice",
		},
		Result: &lcpdv1.Result{
			Result:      []byte("hello"),
			ContentType: "text/plain; charset=utf-8",
		},
	}

	out := formatHumanSummary(res)

	for _, substr := range []string{
		"peer_id=" + strings.Repeat("a", 66),
		"price_msat=1500",
		"terms_hash=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"job_id=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"payment_request:\nlnbc1testinvoice",
		"content_type=text/plain; charset=utf-8",
		"result:\nhello",
	} {
		if !strings.Contains(out, substr) {
			t.Fatalf("formatHumanSummary missing substring %q\noutput:\n%s", substr, out)
		}
	}
}

type fakeClient struct {
	resultText  string
	contentType string
	acceptErr   error

	requestQuoteReq *lcpdv1.RequestQuoteRequest
	acceptReq       *lcpdv1.AcceptAndExecuteRequest

	terms      *lcpdv1.Terms
	acceptResp *lcpdv1.AcceptAndExecuteResponse
}

func (f *fakeClient) RequestQuote(
	_ context.Context,
	req *lcpdv1.RequestQuoteRequest,
	_ ...grpc.CallOption,
) (*lcpdv1.RequestQuoteResponse, error) {
	f.requestQuoteReq = req

	terms := &lcpdv1.Terms{
		ProtocolVersion: uint32(lcpwire.ProtocolVersionV02),
		JobId:           bytes.Repeat([]byte{0x01}, 32),
		PriceMsat:       1500,
		TermsHash:       bytes.Repeat([]byte{0x02}, 32),
		PaymentRequest:  "lnbc1fakeinvoice",
	}
	f.terms = terms

	return &lcpdv1.RequestQuoteResponse{
		PeerId: req.GetPeerId(),
		Terms:  terms,
	}, nil
}

func (f *fakeClient) AcceptAndExecute(
	_ context.Context,
	req *lcpdv1.AcceptAndExecuteRequest,
	_ ...grpc.CallOption,
) (*lcpdv1.AcceptAndExecuteResponse, error) {
	f.acceptReq = req
	if f.acceptErr != nil {
		return nil, f.acceptErr
	}

	f.acceptResp = &lcpdv1.AcceptAndExecuteResponse{
		Result: &lcpdv1.Result{
			Result:      []byte(f.resultText),
			ContentType: f.contentType,
		},
	}
	return f.acceptResp, nil
}
