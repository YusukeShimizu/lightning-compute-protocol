package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bruwbird/lcp/apps/openai-serve/internal/openai"
	lcpdv1 "github.com/bruwbird/lcp/proto-go/lcpd/v1"
	"github.com/gin-gonic/gin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	responseIDPrefix = "lcpchatcmpl-"
	responseIDBytes  = 12

	cancelJobTimeout      = 2 * time.Second
	unsupportedFieldsCap  = 12
	temperatureMilliScale = 1000
)

func (s *Server) handleChatCompletions(c *gin.Context) {
	if c.Request.Method != http.MethodPost {
		writeOpenAIError(c, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	started := time.Now()
	ctx := c.Request.Context()

	req, ok := decodeAndValidateChatRequest(c)
	if !ok {
		return
	}

	prompt, ok := s.buildPrompt(c, req.Messages)
	if !ok {
		return
	}
	promptBytes := len([]byte(prompt))
	promptTokens := openai.ApproxTokensFromBytes(promptBytes)

	model := strings.TrimSpace(req.Model)

	peerID, ok := s.resolvePeerForModel(c, model)
	if !ok {
		return
	}

	task, ok := s.buildLCPChatTask(c, model, prompt, req)
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

	resp, ok := s.buildChatCompletionResponse(c, model, prompt, execResp)
	if !ok {
		return
	}

	price := quote.GetTerms().GetPriceMsat()
	totalLatency := time.Since(started)

	resultBytes := execResp.GetResult().GetResult()
	completionBytes := len(resultBytes)
	completionTokens := openai.ApproxTokensFromBytes(completionBytes)
	totalTokens := promptTokens + completionTokens

	s.log.InfoContext(
		ctx,
		"chat completion",
		"model", model,
		"peer_id", peerID,
		"job_id", hex.EncodeToString(jobID),
		"price_msat", price,
		"terms_hash", hex.EncodeToString(quote.GetTerms().GetTermsHash()),
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
	c.Header("X-Lcp-Job-Id", hex.EncodeToString(jobID))
	c.Header("X-Lcp-Price-Msat", strconv.FormatUint(price, 10))
	c.Header("X-Lcp-Terms-Hash", hex.EncodeToString(quote.GetTerms().GetTermsHash()))
	writeJSON(c, http.StatusOK, resp)
}

func decodeAndValidateChatRequest(c *gin.Context) (openai.ChatCompletionsRequest, bool) {
	var req openai.ChatCompletionsRequest
	if err := decodeJSONBody(c, &req); err != nil {
		if isRequestBodyTooLarge(err) {
			writeOpenAIError(c, http.StatusRequestEntityTooLarge, "invalid_request_error", err.Error())
			return openai.ChatCompletionsRequest{}, false
		}
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return openai.ChatCompletionsRequest{}, false
	}

	if err := validateChatRequest(req); err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return openai.ChatCompletionsRequest{}, false
	}
	return req, true
}

func (s *Server) buildPrompt(c *gin.Context, messages []openai.ChatMessage) (string, bool) {
	prompt, err := openai.BuildPrompt(messages)
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return "", false
	}
	if s.cfg.MaxPromptBytes > 0 && len([]byte(prompt)) > s.cfg.MaxPromptBytes {
		writeOpenAIError(c, http.StatusRequestEntityTooLarge, "invalid_request_error", "prompt is too large")
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

func (s *Server) buildLCPChatTask(
	c *gin.Context,
	model string,
	prompt string,
	req openai.ChatCompletionsRequest,
) (*lcpdv1.Task, bool) {
	task, err := buildLCPChatTask(model, prompt, req)
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

func (s *Server) buildChatCompletionResponse(
	c *gin.Context,
	model string,
	prompt string,
	execResp *lcpdv1.AcceptAndExecuteResponse,
) (openai.ChatCompletionsResponse, bool) {
	resultBytes := execResp.GetResult().GetResult()
	if !utf8.Valid(resultBytes) {
		writeOpenAIError(c, http.StatusBadGateway, "server_error", "provider returned non-utf8 result")
		return openai.ChatCompletionsResponse{}, false
	}

	id, err := randomHexID(responseIDPrefix, responseIDBytes)
	if err != nil {
		writeOpenAIError(c, http.StatusInternalServerError, "server_error", "failed to generate response id")
		return openai.ChatCompletionsResponse{}, false
	}

	resp := buildChatResponse(id, model, prompt, string(resultBytes))
	return resp, true
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

func buildLCPChatTask(model, prompt string, req openai.ChatCompletionsRequest) (*lcpdv1.Task, error) {
	temperatureMilli, err := temperatureToMilli(req.Temperature)
	if err != nil {
		return nil, err
	}
	maxOutputTokens, err := maxTokens(req.MaxTokens, req.MaxCompletionTokens)
	if err != nil {
		return nil, err
	}

	return &lcpdv1.Task{
		Spec: &lcpdv1.Task_LlmChat{
			LlmChat: &lcpdv1.LLMChatTaskSpec{
				Prompt: prompt,
				Params: &lcpdv1.LLMChatParams{
					Profile:          model,
					TemperatureMilli: temperatureMilli,
					MaxOutputTokens:  maxOutputTokens,
				},
			},
		},
	}, nil
}

func buildChatResponse(id, model, prompt, completion string) openai.ChatCompletionsResponse {
	promptTokens := openai.ApproxTokensFromBytes(len([]byte(prompt)))
	completionTokens := openai.ApproxTokensFromBytes(len([]byte(completion)))
	totalTokens := promptTokens + completionTokens

	return openai.ChatCompletionsResponse{
		ID:      id,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []openai.ChatChoice{
			{
				Index: 0,
				Message: openai.ChatMessage{
					Role:    "assistant",
					Content: openai.ChatContent(completion),
				},
				FinishReason: "stop",
			},
		},
		Usage: &openai.Usage{
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      totalTokens,
		},
	}
}

func validateChatRequest(req openai.ChatCompletionsRequest) error {
	if strings.TrimSpace(req.Model) == "" {
		return errors.New("model is required")
	}
	if len(req.Messages) == 0 {
		return errors.New("messages is required")
	}

	if req.Stream != nil && *req.Stream {
		return errors.New("stream=true is not supported")
	}
	if req.N != nil && *req.N != 1 {
		return errors.New("only n=1 is supported")
	}

	unsupported := make([]string, 0, unsupportedFieldsCap)
	if req.TopP != nil {
		unsupported = append(unsupported, "top_p")
	}
	if len(req.Stop) > 0 {
		unsupported = append(unsupported, "stop")
	}
	if req.PresencePenalty != nil {
		unsupported = append(unsupported, "presence_penalty")
	}
	if req.FrequencyPenalty != nil {
		unsupported = append(unsupported, "frequency_penalty")
	}
	if req.Seed != nil {
		unsupported = append(unsupported, "seed")
	}
	if len(req.Tools) > 0 {
		unsupported = append(unsupported, "tools")
	}
	if len(req.ToolChoice) > 0 {
		unsupported = append(unsupported, "tool_choice")
	}
	if len(req.ResponseFormat) > 0 {
		unsupported = append(unsupported, "response_format")
	}
	if len(req.Functions) > 0 {
		unsupported = append(unsupported, "functions")
	}
	if len(req.FunctionCall) > 0 {
		unsupported = append(unsupported, "function_call")
	}
	if len(req.Logprobs) > 0 || len(req.TopLogprobs) > 0 {
		unsupported = append(unsupported, "logprobs/top_logprobs")
	}

	if len(unsupported) > 0 {
		sort.Strings(unsupported)
		return fmt.Errorf("unsupported fields: %s", strings.Join(unsupported, ", "))
	}

	return nil
}

func temperatureToMilli(v *float64) (uint32, error) {
	if v == nil {
		return 0, nil
	}
	if math.IsNaN(*v) || math.IsInf(*v, 0) {
		return 0, errors.New("temperature must be a finite number")
	}
	if *v < 0 || *v > 2 {
		return 0, errors.New("temperature must be between 0 and 2")
	}
	milli := int64(math.Round(*v * temperatureMilliScale))
	if milli < 0 || milli > math.MaxUint32 {
		return 0, errors.New("temperature is out of range")
	}
	return uint32(milli), nil
}

func maxTokens(maxTokensValue *int, maxCompletionTokensValue *int) (uint32, error) {
	var v *int
	switch {
	case maxTokensValue != nil && maxCompletionTokensValue != nil:
		if *maxTokensValue != *maxCompletionTokensValue {
			return 0, errors.New("max_tokens and max_completion_tokens cannot both be set to different values")
		}
		v = maxTokensValue
	case maxTokensValue != nil:
		v = maxTokensValue
	case maxCompletionTokensValue != nil:
		v = maxCompletionTokensValue
	default:
		return 0, nil
	}

	if *v <= 0 {
		return 0, errors.New("max_tokens must be > 0")
	}
	uv := uint64(*v)
	if uv > uint64(math.MaxUint32) {
		return 0, errors.New("max_tokens is too large")
	}
	return uint32(uv), nil
}

func randomHexID(prefix string, nBytes int) (string, error) {
	if nBytes <= 0 {
		return "", errors.New("nBytes must be > 0")
	}
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b), nil
}

func copyBytes(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
