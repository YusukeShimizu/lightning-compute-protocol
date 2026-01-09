package lcpd

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"math"
	"time"

	"github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"
	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
	"github.com/bruwbird/lcp/go-lcpd/internal/requesterjobstore"
	lcpdv1 "github.com/bruwbird/lcp/proto-go/lcpd/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const taskKindOpenAIChatCompletionsV1 = "openai.chat_completions.v1"

type callLogSummary struct {
	method          string
	model           string
	requestBytes    int
	maxOutputTokens uint32
}

func summarizeCall(call *lcpdv1.CallSpec) callLogSummary {
	if call == nil {
		return callLogSummary{}
	}

	method := call.GetMethod()
	summary := callLogSummary{
		method:       method,
		requestBytes: len(call.GetRequestBytes()),
	}

	if method == taskKindOpenAIChatCompletionsV1 {
		model, maxTokens := openAIChatCompletionsLogInfoFromRequestJSON(call.GetRequestBytes())
		summary.model = model
		summary.maxOutputTokens = maxTokens
	}

	return summary
}

func maxOutputTokensFromOpenAIChatCompletionsRequestJSON(b []byte) uint32 {
	var parsed struct {
		Model               string  `json:"model,omitempty"`
		MaxCompletionTokens *uint32 `json:"max_completion_tokens,omitempty"`
		MaxTokens           *uint32 `json:"max_tokens,omitempty"`
		MaxOutputTokens     *uint32 `json:"max_output_tokens,omitempty"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		return 0
	}
	if parsed.MaxCompletionTokens != nil {
		return *parsed.MaxCompletionTokens
	}
	if parsed.MaxTokens != nil {
		return *parsed.MaxTokens
	}
	if parsed.MaxOutputTokens != nil {
		return *parsed.MaxOutputTokens
	}
	return 0
}

func openAIChatCompletionsLogInfoFromRequestJSON(b []byte) (string, uint32) {
	var parsed struct {
		Model               string  `json:"model,omitempty"`
		MaxCompletionTokens *uint32 `json:"max_completion_tokens,omitempty"`
		MaxTokens           *uint32 `json:"max_tokens,omitempty"`
		MaxOutputTokens     *uint32 `json:"max_output_tokens,omitempty"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		return "", 0
	}

	maxTokens := uint32(0)
	switch {
	case parsed.MaxCompletionTokens != nil:
		maxTokens = *parsed.MaxCompletionTokens
	case parsed.MaxTokens != nil:
		maxTokens = *parsed.MaxTokens
	case parsed.MaxOutputTokens != nil:
		maxTokens = *parsed.MaxOutputTokens
	}
	return parsed.Model, maxTokens
}

func (s *Service) summarizeStoredCall(peerID string, jobID lcp.JobID) callLogSummary {
	if s.jobs == nil {
		return callLogSummary{}
	}
	job, ok := s.jobs.Get(peerID, jobID)
	if !ok {
		return callLogSummary{}
	}
	return summarizeCall(job.Call)
}

func unixSeconds(ts *timestamppb.Timestamp) int64 {
	if ts == nil {
		return 0
	}
	return ts.AsTime().Unix()
}

func (s *Service) logQuoteReceived(
	peerID string,
	jobID lcp.JobID,
	summary callLogSummary,
	payloadBytes int,
	quote *lcpdv1.Quote,
	started time.Time,
) {
	s.logger.Infow(
		"quote received",
		"peer_id", peerID,
		"call_id", jobID.String(),
		"method", summary.method,
		"model", summary.model,
		"request_bytes", summary.requestBytes,
		"max_output_tokens", summary.maxOutputTokens,
		"payload_bytes", payloadBytes,
		"price_msat", quote.GetPriceMsat(),
		"quote_expiry_unix", unixSeconds(quote.GetQuoteExpiry()),
		"terms_hash", hex.EncodeToString(quote.GetTermsHash()),
		"total_ms", time.Since(started).Milliseconds(),
	)
}

func (s *Service) logCompleteReceived(
	peerID string,
	jobID lcp.JobID,
	summary callLogSummary,
	quote *lcpdv1.Quote,
	payDuration time.Duration,
	waitDuration time.Duration,
	res *lcpdv1.Complete,
	started time.Time,
) {
	contentType, resultBytes := completeLoggingFields(res)

	s.logger.Infow(
		"complete received",
		"peer_id", peerID,
		"call_id", jobID.String(),
		"method", summary.method,
		"model", summary.model,
		"request_bytes", summary.requestBytes,
		"max_output_tokens", summary.maxOutputTokens,
		"price_msat", quote.GetPriceMsat(),
		"quote_expiry_unix", unixSeconds(quote.GetQuoteExpiry()),
		"terms_hash", hex.EncodeToString(quote.GetTermsHash()),
		"status", res.GetStatus(),
		"result_bytes", resultBytes,
		"content_type", contentType,
		"pay_ms", payDuration.Milliseconds(),
		"wait_ms", waitDuration.Milliseconds(),
		"total_ms", time.Since(started).Milliseconds(),
	)
}

func completeLoggingFields(res *lcpdv1.Complete) (string, int) {
	if res == nil {
		return "", 0
	}

	if res.GetStatus() == lcpdv1.Complete_STATUS_OK && res.GetResponseLen() != 0 {
		resultLen := res.GetResponseLen()
		if resultLen > math.MaxInt {
			return res.GetResponseContentType(), math.MaxInt
		}
		return res.GetResponseContentType(), int(resultLen)
	}

	return res.GetResponseContentType(), len(res.GetResponseBytes())
}

func (s *Service) requestQuoteWaiterError(
	ctx context.Context,
	peerID string,
	jobID lcp.JobID,
	summary callLogSummary,
	err error,
) error {
	st := grpcStatusFromWaiterError(ctx, err)
	s.logger.Warnw(
		"request_quote failed",
		"peer_id", peerID,
		"call_id", jobID.String(),
		"grpc_code", status.Code(st),
		"method", summary.method,
		"model", summary.model,
	)
	return st
}

func (s *Service) requestQuoteLCPError(
	peerID string,
	jobID lcp.JobID,
	summary callLogSummary,
	lcpErr *lcpwire.Error,
) error {
	s.markJobState(peerID, jobID, requesterjobstore.StateFailed)
	s.logger.Warnw(
		"request_quote failed",
		"peer_id", peerID,
		"call_id", jobID.String(),
		"grpc_code", codes.FailedPrecondition,
		"method", summary.method,
		"model", summary.model,
		"lcp_error_code", lcpErr.Code,
	)
	return grpcStatusFromLCPError(*lcpErr)
}

func (s *Service) acceptAndExecuteVerifyError(
	peerID string,
	jobID lcp.JobID,
	summary callLogSummary,
	verifyErr error,
) error {
	s.markJobState(peerID, jobID, requesterjobstore.StateFailed)
	if st, ok := status.FromError(verifyErr); ok {
		s.logger.Warnw(
			"accept_and_execute failed",
			"peer_id", peerID,
			"call_id", jobID.String(),
			"grpc_code", st.Code(),
			"method", summary.method,
			"model", summary.model,
		)
	}
	return verifyErr
}

func (s *Service) acceptAndExecuteLightningError(
	ctx context.Context,
	peerID string,
	jobID lcp.JobID,
	summary callLogSummary,
	payErr error,
) error {
	s.markJobState(peerID, jobID, requesterjobstore.StateFailed)
	st := grpcStatusFromLightningError(ctx, payErr)
	s.logger.Warnw(
		"accept_and_execute failed",
		"peer_id", peerID,
		"call_id", jobID.String(),
		"grpc_code", status.Code(st),
		"method", summary.method,
		"model", summary.model,
	)
	return st
}

func (s *Service) acceptAndExecuteWaiterError(
	ctx context.Context,
	peerID string,
	jobID lcp.JobID,
	summary callLogSummary,
	err error,
) error {
	s.markJobState(peerID, jobID, requesterjobstore.StateFailed)
	st := grpcStatusFromWaiterError(ctx, err)
	s.logger.Warnw(
		"accept_and_execute failed",
		"peer_id", peerID,
		"call_id", jobID.String(),
		"grpc_code", status.Code(st),
		"method", summary.method,
		"model", summary.model,
	)
	return st
}

func (s *Service) acceptAndExecuteLCPError(
	peerID string,
	jobID lcp.JobID,
	summary callLogSummary,
	lcpErr *lcpwire.Error,
) error {
	s.markJobState(peerID, jobID, requesterjobstore.StateFailed)
	s.logger.Warnw(
		"accept_and_execute failed",
		"peer_id", peerID,
		"call_id", jobID.String(),
		"grpc_code", codes.FailedPrecondition,
		"method", summary.method,
		"model", summary.model,
		"lcp_error_code", lcpErr.Code,
	)
	return grpcStatusFromLCPError(*lcpErr)
}
