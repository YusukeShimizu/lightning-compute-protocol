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

type buildCallFunc func(c *gin.Context, model string, reqBytes []byte) (*lcpdv1.CallSpec, bool)

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
		lcpMethodOpenAIChatCompletionsV1,
		model,
		reqBytes,
		streaming,
		s.buildLCPOpenAIChatCompletionsV1Call,
		"chat completion",
	)
}

func (s *Server) handleOpenAIPassthrough(
	c *gin.Context,
	started time.Time,
	method string,
	model string,
	reqBytes []byte,
	streaming bool,
	buildCall buildCallFunc,
	operation string,
) {
	requestBytes := len(reqBytes)
	requestTokens := openai.ApproxTokensFromBytes(requestBytes)

	peerID, ok := s.resolvePeerForMethodAndModel(c, method, model)
	if !ok {
		return
	}

	call, ok := buildCall(c, model, reqBytes)
	if !ok {
		return
	}

	quoteStart := time.Now()
	quote, ok := s.requestQuoteAndValidatePrice(c, peerID, call)
	if !ok {
		return
	}
	quoteLatency := time.Since(quoteStart)

	callID := copyBytes(quote.GetQuote().GetCallId())

	if streaming {
		s.handleOpenAIStreaming(
			c,
			started,
			operation,
			model,
			peerID,
			callID,
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
		callID,
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
	callID []byte,
	quote *lcpdv1.RequestQuoteResponse,
	requestBytes int,
	requestTokens int,
	quoteLatency time.Duration,
) {
	execStart := time.Now()
	grpcStream, cancel, err := s.acceptAndExecuteStream(c, peerID, callID)
	if err != nil {
		cancel()
		s.cancelJobIfRequestCanceled(c, peerID, callID, err)
		writeOpenAIError(c, httpStatusFromGRPC(err), "server_error", grpcErrMessage(err))
		return
	}
	defer cancel()

	writeLCPHeaders(c, peerID, callID, quote)

	streamed, streamErr := s.writeLCPStreamToHTTP(c, grpcStream)
	execLatency := time.Since(execStart)

	if streamErr != nil {
		s.cancelJobIfRequestCanceled(c, peerID, callID, streamErr)
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
		callID,
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
	callID []byte,
	quote *lcpdv1.RequestQuoteResponse,
	requestBytes int,
	requestTokens int,
	quoteLatency time.Duration,
) {
	execStart := time.Now()
	execResp, ok := s.acceptAndExecuteWithCancelOnFailure(c, peerID, callID)
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
		callID,
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

func validateStreamedTerminalResult(c *gin.Context, streamed streamWriteResult) (*lcpdv1.Complete, bool) {
	if streamed.terminalComplete == nil {
		writeOpenAIErrorIfNoBody(c, streamed.wroteBody, http.StatusBadGateway, providerReturnedNoResultMessage)
		return nil, false
	}
	if streamed.terminalComplete.GetStatus() != lcpdv1.Complete_STATUS_OK {
		msg := strings.TrimSpace(streamed.terminalComplete.GetMessage())
		if msg == "" {
			msg = providerReturnedNonOKStatusMessage
		}
		writeOpenAIErrorIfNoBody(c, streamed.wroteBody, http.StatusBadGateway, msg)
		return nil, false
	}
	return streamed.terminalComplete, true
}

func writeOpenAIErrorIfNoBody(c *gin.Context, wroteBody bool, status int, message string) {
	if wroteBody {
		return
	}
	writeOpenAIError(c, status, "server_error", message)
}

func writeLCPHeaders(c *gin.Context, peerID string, callID []byte, quote *lcpdv1.RequestQuoteResponse) {
	q := quote.GetQuote()
	if q == nil {
		return
	}
	price := q.GetPriceMsat()
	callIDHex := hex.EncodeToString(callID)
	termsHashHex := hex.EncodeToString(q.GetTermsHash())
	c.Header("X-Lcp-Peer-Id", peerID)
	c.Header("X-Lcp-Call-Id", callIDHex)
	c.Header("X-Lcp-Price-Msat", strconv.FormatUint(price, 10))
	c.Header("X-Lcp-Terms-Hash", termsHashHex)
}

func (s *Server) logStreamResult(
	c *gin.Context,
	operation string,
	started time.Time,
	model string,
	peerID string,
	callID []byte,
	quote *lcpdv1.RequestQuoteResponse,
	requestBytes int,
	requestTokens int,
	quoteLatency time.Duration,
	execLatency time.Duration,
	streamedBytes int,
) {
	ctx := c.Request.Context()

	q := quote.GetQuote()
	if q == nil {
		return
	}
	price := q.GetPriceMsat()
	totalLatency := time.Since(started)

	streamedTokens := openai.ApproxTokensFromBytes(streamedBytes)
	totalTokens := requestTokens + streamedTokens

	callIDHex := hex.EncodeToString(callID)
	termsHashHex := hex.EncodeToString(q.GetTermsHash())

	s.log.InfoContext(
		ctx,
		operation,
		"model", model,
		"peer_id", peerID,
		"call_id", callIDHex,
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

func (s *Server) resolvePeerForMethodAndModel(
	c *gin.Context,
	method string,
	model string,
) (string, bool) {
	peersResp, err := s.listPeers(c)
	if err != nil {
		writeOpenAIError(c, httpStatusFromGRPC(err), "server_error", grpcErrMessage(err))
		return "", false
	}
	if modelErr := s.validateModelForMethod(model, method, peersResp); modelErr != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", modelErr.Error())
		return "", false
	}
	peerID, err := s.resolvePeerIDForMethod(model, method, peersResp)
	if err != nil {
		writeOpenAIError(c, http.StatusServiceUnavailable, "server_error", err.Error())
		return "", false
	}
	return peerID, true
}

func (s *Server) buildLCPOpenAIChatCompletionsV1Call(
	c *gin.Context,
	model string,
	reqBytes []byte,
) (*lcpdv1.CallSpec, bool) {
	call, err := buildLCPOpenAIChatCompletionsV1Call(model, reqBytes)
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return nil, false
	}
	return call, true
}

func (s *Server) requestQuoteAndValidatePrice(
	c *gin.Context,
	peerID string,
	call *lcpdv1.CallSpec,
) (*lcpdv1.RequestQuoteResponse, bool) {
	quote, err := s.requestQuote(c, peerID, call)
	if err != nil {
		writeOpenAIError(c, httpStatusFromGRPC(err), "server_error", grpcErrMessage(err))
		return nil, false
	}

	q := quote.GetQuote()
	if q == nil {
		writeOpenAIError(c, http.StatusBadGateway, "server_error", "provider returned no quote")
		return nil, false
	}
	price := q.GetPriceMsat()
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
	callID []byte,
) (*lcpdv1.AcceptAndExecuteResponse, bool) {
	execResp, err := s.acceptAndExecute(c, peerID, callID)
	if err != nil {
		if shouldCancelAfterExecuteError(c, err) {
			s.cancelJobAsync(peerID, callID)
		}
		writeOpenAIError(c, httpStatusFromGRPC(err), "server_error", grpcErrMessage(err))
		return nil, false
	}
	return execResp, true
}

func validateResult(
	c *gin.Context,
	execResp *lcpdv1.AcceptAndExecuteResponse,
) (*lcpdv1.Complete, bool) {
	result := execResp.GetComplete()
	if result == nil {
		writeOpenAIError(c, http.StatusBadGateway, "server_error", providerReturnedNoResultMessage)
		return nil, false
	}
	if result.GetStatus() != lcpdv1.Complete_STATUS_OK {
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
	callID []byte,
	quote *lcpdv1.RequestQuoteResponse,
	requestBytes int,
	requestTokens int,
	quoteLatency time.Duration,
	execLatency time.Duration,
	result *lcpdv1.Complete,
) bool {
	ctx := c.Request.Context()

	q := quote.GetQuote()
	if q == nil {
		writeOpenAIError(c, http.StatusBadGateway, "server_error", "provider returned no quote")
		return false
	}
	price := q.GetPriceMsat()
	totalLatency := time.Since(started)

	resultBytes := result.GetResponseBytes()
	completionBytes := len(resultBytes)
	completionTokens := openai.ApproxTokensFromBytes(completionBytes)
	totalTokens := requestTokens + completionTokens

	callIDHex := hex.EncodeToString(callID)
	termsHashHex := hex.EncodeToString(q.GetTermsHash())

	s.log.InfoContext(
		ctx,
		operation,
		"model", model,
		"peer_id", peerID,
		"call_id", callIDHex,
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

	writeLCPHeaders(c, peerID, callID, quote)

	if enc := strings.TrimSpace(result.GetResponseContentEncoding()); enc != "" && enc != contentEncodingIdentity {
		writeOpenAIError(c, http.StatusBadGateway, "server_error", fmt.Sprintf("unsupported content encoding: %q", enc))
		return false
	}

	contentType := strings.TrimSpace(result.GetResponseContentType())
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

func (s *Server) requestQuote(
	c *gin.Context,
	peerID string,
	call *lcpdv1.CallSpec,
) (*lcpdv1.RequestQuoteResponse, error) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), s.cfg.TimeoutQuote)
	defer cancel()
	return s.lcpd.RequestQuote(ctx, &lcpdv1.RequestQuoteRequest{
		PeerId: peerID,
		Call:   call,
	})
}

func (s *Server) acceptAndExecute(
	c *gin.Context,
	peerID string,
	callID []byte,
) (*lcpdv1.AcceptAndExecuteResponse, error) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), s.cfg.TimeoutExecute)
	defer cancel()
	return s.lcpd.AcceptAndExecute(ctx, &lcpdv1.AcceptAndExecuteRequest{
		PeerId:     peerID,
		CallId:     callID,
		PayInvoice: true,
	})
}

func (s *Server) cancelJobAsync(peerID string, callID []byte) {
	callIDCopy := copyBytes(callID)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), cancelJobTimeout)
		defer cancel()
		_, err := s.lcpd.CancelJob(ctx, &lcpdv1.CancelJobRequest{
			PeerId: peerID,
			CallId: callIDCopy,
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

func buildLCPOpenAIChatCompletionsV1Call(model string, requestJSON []byte) (*lcpdv1.CallSpec, error) {
	if strings.TrimSpace(model) == "" {
		return nil, errors.New("model is required")
	}
	if len(requestJSON) == 0 {
		return nil, errors.New("request body is required")
	}

	paramsBytes, err := encodeOpenAIModelParamsTLV(model)
	if err != nil {
		return nil, err
	}

	return &lcpdv1.CallSpec{
		Method:                 lcpMethodOpenAIChatCompletionsV1,
		Params:                 paramsBytes,
		RequestBytes:           requestJSON,
		RequestContentType:     requestContentTypeJSONUTF8,
		RequestContentEncoding: contentEncodingIdentity,
	}, nil
}

func copyBytes(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
