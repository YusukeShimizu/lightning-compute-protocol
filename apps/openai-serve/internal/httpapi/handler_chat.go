package httpapi

import (
	"context"
	"encoding/hex"
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
	cancelJobTimeout = 2 * time.Second

	cancelReasonRequestCanceled        = "request canceled"
	providerReturnedNoResultMessage    = "provider returned no result"
	providerReturnedNonOKStatusMessage = "provider returned non-ok status"
)

type buildTaskFunc func(c *gin.Context, model string, reqBytes []byte) (*lcpdv1.Task, bool)

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

	streaming := req.Stream != nil && *req.Stream
	model := strings.TrimSpace(req.Model)
	s.handleOpenAIPassthrough(
		c,
		started,
		lcpdv1.LCPTaskKind_LCP_TASK_KIND_OPENAI_CHAT_COMPLETIONS_V1,
		model,
		reqBytes,
		streaming,
		s.buildLCPOpenAIChatCompletionsV1Task,
		"chat completion",
	)
}

func (s *Server) handleOpenAIPassthrough(
	c *gin.Context,
	started time.Time,
	taskKind lcpdv1.LCPTaskKind,
	model string,
	reqBytes []byte,
	streaming bool,
	buildTask buildTaskFunc,
	operation string,
) {
	requestBytes := len(reqBytes)
	requestTokens := openai.ApproxTokensFromBytes(requestBytes)

	peerID, ok := s.resolvePeerForTaskKindAndModel(c, taskKind, model)
	if !ok {
		return
	}

	task, ok := buildTask(c, model, reqBytes)
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

	if streaming {
		s.handleOpenAIStreaming(
			c,
			started,
			operation,
			model,
			peerID,
			jobID,
			quote,
			requestBytes,
			requestTokens,
			quoteLatency,
		)
		return
	}

	s.handleOpenAINonStreaming(
		c,
		started,
		operation,
		model,
		peerID,
		jobID,
		quote,
		requestBytes,
		requestTokens,
		quoteLatency,
	)
}

func (s *Server) handleOpenAIStreaming(
	c *gin.Context,
	started time.Time,
	operation string,
	model string,
	peerID string,
	jobID []byte,
	quote *lcpdv1.RequestQuoteResponse,
	requestBytes int,
	requestTokens int,
	quoteLatency time.Duration,
) {
	execStart := time.Now()
	grpcStream, cancel, err := s.acceptAndExecuteStream(c, peerID, jobID)
	if err != nil {
		cancel()
		s.cancelJobIfRequestCanceled(c, peerID, jobID, err)
		writeOpenAIError(c, httpStatusFromGRPC(err), "server_error", grpcErrMessage(err))
		return
	}
	defer cancel()

	writeLCPHeaders(c, peerID, jobID, quote)

	streamed, streamErr := s.writeLCPStreamToHTTP(c, grpcStream)
	execLatency := time.Since(execStart)

	if streamErr != nil {
		s.cancelJobIfRequestCanceled(c, peerID, jobID, streamErr)
		if !streamed.wroteBody {
			writeOpenAIError(c, httpStatusFromGRPC(streamErr), "server_error", grpcErrMessage(streamErr))
		}
		return
	}

	if _, ok := validateStreamedTerminalResult(c, streamed); !ok {
		return
	}

	s.logStreamResult(
		c,
		operation+" (stream)",
		started,
		model,
		peerID,
		jobID,
		quote,
		requestBytes,
		requestTokens,
		quoteLatency,
		execLatency,
		streamed.bytesWritten,
	)
}

func (s *Server) handleOpenAINonStreaming(
	c *gin.Context,
	started time.Time,
	operation string,
	model string,
	peerID string,
	jobID []byte,
	quote *lcpdv1.RequestQuoteResponse,
	requestBytes int,
	requestTokens int,
	quoteLatency time.Duration,
) {
	execStart := time.Now()
	execResp, ok := s.acceptAndExecuteWithCancelOnFailure(c, peerID, jobID)
	if !ok {
		return
	}
	execLatency := time.Since(execStart)

	result, ok := validateResult(c, execResp)
	if !ok {
		return
	}

	s.logAndWriteResult(
		c,
		operation,
		started,
		model,
		peerID,
		jobID,
		quote,
		requestBytes,
		requestTokens,
		quoteLatency,
		execLatency,
		result,
	)
}

func (s *Server) cancelJobIfRequestCanceled(c *gin.Context, peerID string, jobID []byte, err error) {
	if shouldCancelAfterExecuteError(c, err) {
		s.cancelJobAsync(peerID, jobID)
	}
}

func validateStreamedTerminalResult(c *gin.Context, streamed streamWriteResult) (*lcpdv1.Result, bool) {
	if streamed.terminalResult == nil {
		writeOpenAIErrorIfNoBody(c, streamed.wroteBody, http.StatusBadGateway, providerReturnedNoResultMessage)
		return nil, false
	}
	if streamed.terminalResult.GetStatus() != lcpdv1.Result_STATUS_OK {
		msg := strings.TrimSpace(streamed.terminalResult.GetMessage())
		if msg == "" {
			msg = providerReturnedNonOKStatusMessage
		}
		writeOpenAIErrorIfNoBody(c, streamed.wroteBody, http.StatusBadGateway, msg)
		return nil, false
	}
	return streamed.terminalResult, true
}

func writeOpenAIErrorIfNoBody(c *gin.Context, wroteBody bool, status int, message string) {
	if wroteBody {
		return
	}
	writeOpenAIError(c, status, "server_error", message)
}

func writeLCPHeaders(c *gin.Context, peerID string, jobID []byte, quote *lcpdv1.RequestQuoteResponse) {
	price := quote.GetTerms().GetPriceMsat()
	jobIDHex := hex.EncodeToString(jobID)
	termsHashHex := hex.EncodeToString(quote.GetTerms().GetTermsHash())
	c.Header("X-Lcp-Peer-Id", peerID)
	c.Header("X-Lcp-Job-Id", jobIDHex)
	c.Header("X-Lcp-Price-Msat", strconv.FormatUint(price, 10))
	c.Header("X-Lcp-Terms-Hash", termsHashHex)
}

func (s *Server) logStreamResult(
	c *gin.Context,
	operation string,
	started time.Time,
	model string,
	peerID string,
	jobID []byte,
	quote *lcpdv1.RequestQuoteResponse,
	requestBytes int,
	requestTokens int,
	quoteLatency time.Duration,
	execLatency time.Duration,
	streamedBytes int,
) {
	ctx := c.Request.Context()

	price := quote.GetTerms().GetPriceMsat()
	totalLatency := time.Since(started)

	streamedTokens := openai.ApproxTokensFromBytes(streamedBytes)
	totalTokens := requestTokens + streamedTokens

	jobIDHex := hex.EncodeToString(jobID)
	termsHashHex := hex.EncodeToString(quote.GetTerms().GetTermsHash())

	s.log.InfoContext(
		ctx,
		operation,
		"model", model,
		"peer_id", peerID,
		"job_id", jobIDHex,
		"price_msat", price,
		"terms_hash", termsHashHex,
		"request_bytes", requestBytes,
		"streamed_bytes", streamedBytes,
		"request_tokens_approx", requestTokens,
		"streamed_tokens_approx", streamedTokens,
		"total_tokens_approx", totalTokens,
		"quote_ms", quoteLatency.Milliseconds(),
		"execute_ms", execLatency.Milliseconds(),
		"total_ms", totalLatency.Milliseconds(),
	)
}

func decodeAndValidateChatRequest(c *gin.Context) ([]byte, openai.ChatCompletionsRequest, bool) {
	var parsed openai.ChatCompletionsRequest
	body, ok := decodeJSONRequest(c, &parsed)
	if !ok {
		return nil, openai.ChatCompletionsRequest{}, false
	}
	if !validateModelField(c, parsed.Model) {
		return nil, openai.ChatCompletionsRequest{}, false
	}
	if len(parsed.Messages) == 0 {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "messages is required")
		return nil, openai.ChatCompletionsRequest{}, false
	}

	return body, parsed, true
}

func (s *Server) resolvePeerForTaskKindAndModel(
	c *gin.Context,
	taskKind lcpdv1.LCPTaskKind,
	model string,
) (string, bool) {
	peersResp, err := s.listPeers(c)
	if err != nil {
		writeOpenAIError(c, httpStatusFromGRPC(err), "server_error", grpcErrMessage(err))
		return "", false
	}
	if modelErr := s.validateModelForTaskKind(model, taskKind, peersResp); modelErr != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", modelErr.Error())
		return "", false
	}
	peerID, err := s.resolvePeerIDForTaskKind(model, taskKind, peersResp)
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
			s.cancelJobAsync(peerID, jobID)
		}
		writeOpenAIError(c, httpStatusFromGRPC(err), "server_error", grpcErrMessage(err))
		return nil, false
	}
	return execResp, true
}

func validateResult(
	c *gin.Context,
	execResp *lcpdv1.AcceptAndExecuteResponse,
) (*lcpdv1.Result, bool) {
	result := execResp.GetResult()
	if result == nil {
		writeOpenAIError(c, http.StatusBadGateway, "server_error", providerReturnedNoResultMessage)
		return nil, false
	}
	if result.GetStatus() != lcpdv1.Result_STATUS_OK {
		msg := strings.TrimSpace(result.GetMessage())
		if msg == "" {
			msg = providerReturnedNonOKStatusMessage
		}
		writeOpenAIError(c, http.StatusBadGateway, "server_error", msg)
		return nil, false
	}
	return result, true
}

func (s *Server) logAndWriteResult(
	c *gin.Context,
	operation string,
	started time.Time,
	model string,
	peerID string,
	jobID []byte,
	quote *lcpdv1.RequestQuoteResponse,
	requestBytes int,
	requestTokens int,
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
	totalTokens := requestTokens + completionTokens

	jobIDHex := hex.EncodeToString(jobID)
	termsHashHex := hex.EncodeToString(quote.GetTerms().GetTermsHash())

	s.log.InfoContext(
		ctx,
		operation,
		"model", model,
		"peer_id", peerID,
		"job_id", jobIDHex,
		"price_msat", price,
		"terms_hash", termsHashHex,
		"request_bytes", requestBytes,
		"completion_bytes", completionBytes,
		"request_tokens_approx", requestTokens,
		"completion_tokens_approx", completionTokens,
		"total_tokens_approx", totalTokens,
		"quote_ms", quoteLatency.Milliseconds(),
		"execute_ms", execLatency.Milliseconds(),
		"total_ms", totalLatency.Milliseconds(),
	)

	writeLCPHeaders(c, peerID, jobID, quote)

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

func (s *Server) cancelJobAsync(peerID string, jobID []byte) {
	jobIDCopy := copyBytes(jobID)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), cancelJobTimeout)
		defer cancel()
		_, err := s.lcpd.CancelJob(ctx, &lcpdv1.CancelJobRequest{
			PeerId: peerID,
			JobId:  jobIDCopy,
			Reason: cancelReasonRequestCanceled,
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
