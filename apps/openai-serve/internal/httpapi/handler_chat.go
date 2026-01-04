package httpapi

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bruwbird/lcp/apps/openai-serve/internal/openai"
	lcpdv1 "github.com/bruwbird/lcp/proto-go/lcpd/v1"
	"github.com/gin-gonic/gin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	cancelJobTimeout        = 2 * time.Second
	contentEncodingIdentity = "identity"
)

func (s *Server) handleChatCompletions(c *gin.Context) {
	if c.Request.Method != http.MethodPost {
		writeOpenAIError(c, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	started := time.Now()

	reqBytes, req, ok := decodeAndValidateChatRequest(c)
	if !ok {
		return
	}

	prompt, ok := s.buildPrompt(c, req.Messages)
	if !ok {
		return
	}
	promptBytes := len(prompt)
	promptTokens := openai.ApproxTokensFromBytes(promptBytes)

	model := strings.TrimSpace(req.Model)
	peerID, ok := s.resolvePeerForModel(c, model)
	if !ok {
		return
	}

	task, ok := s.buildLCPOpenAIChatCompletionsV1Task(c, model, reqBytes)
	if !ok {
		return
	}

	quoteStart := time.Now()
	quote, ok := s.requestQuoteAndValidatePrice(c, peerID, task)
	if !ok {
		return
	}
	quoteLatency := time.Since(quoteStart)

	jobID := copyBytes(quote.GetTerms().GetJobId())

	execStart := time.Now()
	execResp, ok := s.acceptAndExecuteWithCancelOnFailure(c, peerID, jobID)
	if !ok {
		return
	}
	execLatency := time.Since(execStart)

	result, ok := validateChatCompletionResult(c, execResp)
	if !ok {
		return
	}

	if !s.logAndWriteChatCompletionResult(
		c,
		started,
		model,
		peerID,
		jobID,
		quote,
		promptBytes,
		promptTokens,
		quoteLatency,
		execLatency,
		result,
	) {
		return
	}
}

func decodeAndValidateChatRequest(c *gin.Context) ([]byte, openai.ChatCompletionsRequest, bool) {
	body, readErr := readRequestBodyBytes(c)
	if readErr != nil {
		if isRequestBodyTooLarge(readErr) {
			writeOpenAIError(c, http.StatusRequestEntityTooLarge, "invalid_request_error", readErr.Error())
			return nil, openai.ChatCompletionsRequest{}, false
		}
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", readErr.Error())
		return nil, openai.ChatCompletionsRequest{}, false
	}

	var parsed openai.ChatCompletionsRequest
	if err := json.Unmarshal(body, &parsed); err != nil {
		msg := strings.TrimPrefix(err.Error(), "json: ")
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", msg)
		return nil, openai.ChatCompletionsRequest{}, false
	}

	model := parsed.Model
	trimmedModel := strings.TrimSpace(model)
	if trimmedModel == "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return nil, openai.ChatCompletionsRequest{}, false
	}
	if trimmedModel != model {
		writeOpenAIError(
			c,
			http.StatusBadRequest,
			"invalid_request_error",
			"model must not have leading/trailing whitespace",
		)
		return nil, openai.ChatCompletionsRequest{}, false
	}
	if len(parsed.Messages) == 0 {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "messages is required")
		return nil, openai.ChatCompletionsRequest{}, false
	}
	if parsed.Stream != nil && *parsed.Stream {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "stream=true is not supported")
		return nil, openai.ChatCompletionsRequest{}, false
	}

	return body, parsed, true
}

func (s *Server) buildPrompt(c *gin.Context, messages []openai.ChatMessage) (string, bool) {
	prompt, err := openai.BuildPrompt(messages)
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return "", false
	}
	if s.cfg.MaxPromptBytes > 0 && len(prompt) > s.cfg.MaxPromptBytes {
		writeOpenAIError(
			c,
			http.StatusBadRequest,
			"invalid_request_error",
			fmt.Sprintf(
				"prompt exceeds max bytes: prompt_bytes=%d max_prompt_bytes=%d",
				len(prompt),
				s.cfg.MaxPromptBytes,
			),
		)
		return "", false
	}
	return prompt, true
}

func (s *Server) resolvePeerForModel(
	c *gin.Context,
	model string,
) (string, bool) {
	peersResp, err := s.listPeers(c)
	if err != nil {
		writeOpenAIError(c, httpStatusFromGRPC(err), "server_error", grpcErrMessage(err))
		return "", false
	}
	if modelErr := s.validateModel(model, peersResp); modelErr != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", modelErr.Error())
		return "", false
	}
	peerID, err := s.resolvePeerID(model, peersResp)
	if err != nil {
		writeOpenAIError(c, http.StatusServiceUnavailable, "server_error", err.Error())
		return "", false
	}
	return peerID, true
}

func (s *Server) buildLCPOpenAIChatCompletionsV1Task(
	c *gin.Context,
	model string,
	reqBytes []byte,
) (*lcpdv1.Task, bool) {
	task, err := buildLCPOpenAIChatCompletionsV1Task(model, reqBytes)
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return nil, false
	}
	return task, true
}

func (s *Server) requestQuoteAndValidatePrice(
	c *gin.Context,
	peerID string,
	task *lcpdv1.Task,
) (*lcpdv1.RequestQuoteResponse, bool) {
	quote, err := s.requestQuote(c, peerID, task)
	if err != nil {
		writeOpenAIError(c, httpStatusFromGRPC(err), "server_error", grpcErrMessage(err))
		return nil, false
	}

	price := quote.GetTerms().GetPriceMsat()
	if s.cfg.MaxPriceMsat > 0 && price > s.cfg.MaxPriceMsat {
		writeOpenAIError(
			c,
			http.StatusBadRequest,
			"invalid_request_error",
			fmt.Sprintf("quote price exceeds limit: price_msat=%d limit_msat=%d", price, s.cfg.MaxPriceMsat),
		)
		return nil, false
	}
	return quote, true
}

func (s *Server) acceptAndExecuteWithCancelOnFailure(
	c *gin.Context,
	peerID string,
	jobID []byte,
) (*lcpdv1.AcceptAndExecuteResponse, bool) {
	execResp, err := s.acceptAndExecute(c, peerID, jobID)
	if err != nil {
		if shouldCancelAfterExecuteError(c, err) {
			s.cancelJobAsync(peerID, jobID, "request canceled")
		}
		writeOpenAIError(c, httpStatusFromGRPC(err), "server_error", grpcErrMessage(err))
		return nil, false
	}
	return execResp, true
}

func validateChatCompletionResult(
	c *gin.Context,
	execResp *lcpdv1.AcceptAndExecuteResponse,
) (*lcpdv1.Result, bool) {
	result := execResp.GetResult()
	if result == nil {
		writeOpenAIError(c, http.StatusBadGateway, "server_error", "provider returned no result")
		return nil, false
	}
	if result.GetStatus() != lcpdv1.Result_STATUS_OK {
		msg := strings.TrimSpace(result.GetMessage())
		if msg == "" {
			msg = "provider returned non-ok status"
		}
		writeOpenAIError(c, http.StatusBadGateway, "server_error", msg)
		return nil, false
	}
	return result, true
}

func (s *Server) logAndWriteChatCompletionResult(
	c *gin.Context,
	started time.Time,
	model string,
	peerID string,
	jobID []byte,
	quote *lcpdv1.RequestQuoteResponse,
	promptBytes int,
	promptTokens int,
	quoteLatency time.Duration,
	execLatency time.Duration,
	result *lcpdv1.Result,
) bool {
	ctx := c.Request.Context()

	price := quote.GetTerms().GetPriceMsat()
	totalLatency := time.Since(started)

	resultBytes := result.GetResult()
	completionBytes := len(resultBytes)
	completionTokens := openai.ApproxTokensFromBytes(completionBytes)
	totalTokens := promptTokens + completionTokens

	jobIDHex := hex.EncodeToString(jobID)
	termsHashHex := hex.EncodeToString(quote.GetTerms().GetTermsHash())

	s.log.InfoContext(
		ctx,
		"chat completion",
		"model", model,
		"peer_id", peerID,
		"job_id", jobIDHex,
		"price_msat", price,
		"terms_hash", termsHashHex,
		"prompt_bytes", promptBytes,
		"completion_bytes", completionBytes,
		"prompt_tokens_approx", promptTokens,
		"completion_tokens_approx", completionTokens,
		"total_tokens_approx", totalTokens,
		"quote_ms", quoteLatency.Milliseconds(),
		"execute_ms", execLatency.Milliseconds(),
		"total_ms", totalLatency.Milliseconds(),
	)

	c.Header("X-Lcp-Peer-Id", peerID)
	c.Header("X-Lcp-Job-Id", jobIDHex)
	c.Header("X-Lcp-Price-Msat", strconv.FormatUint(price, 10))
	c.Header("X-Lcp-Terms-Hash", termsHashHex)

	if enc := strings.TrimSpace(result.GetContentEncoding()); enc != "" && enc != contentEncodingIdentity {
		writeOpenAIError(c, http.StatusBadGateway, "server_error", fmt.Sprintf("unsupported content encoding: %q", enc))
		return false
	}

	contentType := strings.TrimSpace(result.GetContentType())
	if contentType == "" {
		contentType = "application/json; charset=utf-8"
	}
	c.Data(http.StatusOK, contentType, resultBytes)
	return true
}

func (s *Server) listPeers(c *gin.Context) (*lcpdv1.ListLCPPeersResponse, error) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), s.cfg.TimeoutQuote)
	defer cancel()
	return s.lcpd.ListLCPPeers(ctx, &lcpdv1.ListLCPPeersRequest{})
}

func (s *Server) requestQuote(c *gin.Context, peerID string, task *lcpdv1.Task) (*lcpdv1.RequestQuoteResponse, error) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), s.cfg.TimeoutQuote)
	defer cancel()
	return s.lcpd.RequestQuote(ctx, &lcpdv1.RequestQuoteRequest{
		PeerId: peerID,
		Task:   task,
	})
}

func (s *Server) acceptAndExecute(
	c *gin.Context,
	peerID string,
	jobID []byte,
) (*lcpdv1.AcceptAndExecuteResponse, error) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), s.cfg.TimeoutExecute)
	defer cancel()
	return s.lcpd.AcceptAndExecute(ctx, &lcpdv1.AcceptAndExecuteRequest{
		PeerId:     peerID,
		JobId:      jobID,
		PayInvoice: true,
	})
}

func (s *Server) cancelJobAsync(peerID string, jobID []byte, reason string) {
	jobIDCopy := copyBytes(jobID)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), cancelJobTimeout)
		defer cancel()
		_, err := s.lcpd.CancelJob(ctx, &lcpdv1.CancelJobRequest{
			PeerId: peerID,
			JobId:  jobIDCopy,
			Reason: reason,
		})
		if err != nil {
			s.log.ErrorContext(ctx, "cancel job failed", "err", err, "peer_id", peerID)
		}
	}()
}

func shouldCancelAfterExecuteError(c *gin.Context, err error) bool {
	if errors.Is(c.Request.Context().Err(), context.Canceled) {
		return true
	}
	if errors.Is(c.Request.Context().Err(), context.DeadlineExceeded) {
		return true
	}
	if st, ok := status.FromError(err); ok && (st.Code() == codes.Canceled || st.Code() == codes.DeadlineExceeded) {
		return true
	}
	return false
}

func buildLCPOpenAIChatCompletionsV1Task(model string, requestJSON []byte) (*lcpdv1.Task, error) {
	if strings.TrimSpace(model) == "" {
		return nil, errors.New("model is required")
	}
	if len(requestJSON) == 0 {
		return nil, errors.New("request body is required")
	}
	return &lcpdv1.Task{
		Spec: &lcpdv1.Task_OpenaiChatCompletionsV1{
			OpenaiChatCompletionsV1: &lcpdv1.OpenAIChatCompletionsV1TaskSpec{
				RequestJson: requestJSON,
				Params: &lcpdv1.OpenAIChatCompletionsV1Params{
					Model: model,
				},
			},
		},
	}, nil
}

func copyBytes(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
