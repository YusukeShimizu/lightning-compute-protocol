package lcpd

import (
	"context"
	"encoding/hex"
	"encoding/json"
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

type taskLogSummary struct {
	taskKind        string
	model           string
	requestBytes    int
	maxOutputTokens uint32
}

func summarizeTask(task *lcpdv1.Task) taskLogSummary {
	if task == nil {
		return taskLogSummary{}
	}

	spec := task.GetOpenaiChatCompletionsV1()
	if spec == nil {
		return taskLogSummary{}
	}

	model := ""
	if params := spec.GetParams(); params != nil {
		model = params.GetModel()
	}

	return taskLogSummary{
		taskKind:        taskKindOpenAIChatCompletionsV1,
		model:           model,
		requestBytes:    len(spec.GetRequestJson()),
		maxOutputTokens: maxOutputTokensFromOpenAIChatCompletionsRequestJSON(spec.GetRequestJson()),
	}
}

func maxOutputTokensFromOpenAIChatCompletionsRequestJSON(b []byte) uint32 {
	var parsed struct {
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

func (s *Service) summarizeStoredTask(peerID string, jobID lcp.JobID) taskLogSummary {
	if s.jobs == nil {
		return taskLogSummary{}
	}
	job, ok := s.jobs.Get(peerID, jobID)
	if !ok {
		return taskLogSummary{}
	}
	return summarizeTask(job.Task)
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
	summary taskLogSummary,
	payloadBytes int,
	terms *lcpdv1.Terms,
	started time.Time,
) {
	s.logger.Infow(
		"quote received",
		"peer_id", peerID,
		"job_id", jobID.String(),
		"task_kind", summary.taskKind,
		"model", summary.model,
		"request_bytes", summary.requestBytes,
		"max_output_tokens", summary.maxOutputTokens,
		"payload_bytes", payloadBytes,
		"price_msat", terms.GetPriceMsat(),
		"quote_expiry_unix", unixSeconds(terms.GetQuoteExpiry()),
		"terms_hash", hex.EncodeToString(terms.GetTermsHash()),
		"total_ms", time.Since(started).Milliseconds(),
	)
}

func (s *Service) logResultReceived(
	peerID string,
	jobID lcp.JobID,
	summary taskLogSummary,
	terms *lcpdv1.Terms,
	payDuration time.Duration,
	waitDuration time.Duration,
	res *lcpdv1.Result,
	started time.Time,
) {
	contentType := ""
	resultBytes := 0
	if res != nil {
		contentType = res.GetContentType()
		if res.GetStatus() == lcpdv1.Result_STATUS_OK && res.GetResultLen() != 0 {
			resultBytes = int(res.GetResultLen())
		} else {
			resultBytes = len(res.GetResult())
		}
	}

	s.logger.Infow(
		"result received",
		"peer_id", peerID,
		"job_id", jobID.String(),
		"task_kind", summary.taskKind,
		"model", summary.model,
		"request_bytes", summary.requestBytes,
		"max_output_tokens", summary.maxOutputTokens,
		"price_msat", terms.GetPriceMsat(),
		"quote_expiry_unix", unixSeconds(terms.GetQuoteExpiry()),
		"terms_hash", hex.EncodeToString(terms.GetTermsHash()),
		"result_bytes", resultBytes,
		"content_type", contentType,
		"pay_ms", payDuration.Milliseconds(),
		"wait_ms", waitDuration.Milliseconds(),
		"total_ms", time.Since(started).Milliseconds(),
	)
}

func (s *Service) requestQuoteWaiterError(
	ctx context.Context,
	peerID string,
	jobID lcp.JobID,
	summary taskLogSummary,
	err error,
) error {
	st := grpcStatusFromWaiterError(ctx, err)
	s.logger.Warnw(
		"request_quote failed",
		"peer_id", peerID,
		"job_id", jobID.String(),
		"grpc_code", status.Code(st),
		"task_kind", summary.taskKind,
		"model", summary.model,
	)
	return st
}

func (s *Service) requestQuoteLCPError(
	peerID string,
	jobID lcp.JobID,
	summary taskLogSummary,
	lcpErr *lcpwire.Error,
) error {
	s.markJobState(peerID, jobID, requesterjobstore.StateFailed)
	s.logger.Warnw(
		"request_quote failed",
		"peer_id", peerID,
		"job_id", jobID.String(),
		"grpc_code", codes.FailedPrecondition,
		"task_kind", summary.taskKind,
		"model", summary.model,
		"lcp_error_code", lcpErr.Code,
	)
	return grpcStatusFromLCPError(*lcpErr)
}

func (s *Service) acceptAndExecuteVerifyError(
	peerID string,
	jobID lcp.JobID,
	summary taskLogSummary,
	verifyErr error,
) error {
	s.markJobState(peerID, jobID, requesterjobstore.StateFailed)
	if st, ok := status.FromError(verifyErr); ok {
		s.logger.Warnw(
			"accept_and_execute failed",
			"peer_id", peerID,
			"job_id", jobID.String(),
			"grpc_code", st.Code(),
			"task_kind", summary.taskKind,
			"model", summary.model,
		)
	}
	return verifyErr
}

func (s *Service) acceptAndExecuteLightningError(
	ctx context.Context,
	peerID string,
	jobID lcp.JobID,
	summary taskLogSummary,
	payErr error,
) error {
	s.markJobState(peerID, jobID, requesterjobstore.StateFailed)
	st := grpcStatusFromLightningError(ctx, payErr)
	s.logger.Warnw(
		"accept_and_execute failed",
		"peer_id", peerID,
		"job_id", jobID.String(),
		"grpc_code", status.Code(st),
		"task_kind", summary.taskKind,
		"model", summary.model,
	)
	return st
}

func (s *Service) acceptAndExecuteWaiterError(
	ctx context.Context,
	peerID string,
	jobID lcp.JobID,
	summary taskLogSummary,
	err error,
) error {
	s.markJobState(peerID, jobID, requesterjobstore.StateFailed)
	st := grpcStatusFromWaiterError(ctx, err)
	s.logger.Warnw(
		"accept_and_execute failed",
		"peer_id", peerID,
		"job_id", jobID.String(),
		"grpc_code", status.Code(st),
		"task_kind", summary.taskKind,
		"model", summary.model,
	)
	return st
}

func (s *Service) acceptAndExecuteLCPError(
	peerID string,
	jobID lcp.JobID,
	summary taskLogSummary,
	lcpErr *lcpwire.Error,
) error {
	s.markJobState(peerID, jobID, requesterjobstore.StateFailed)
	s.logger.Warnw(
		"accept_and_execute failed",
		"peer_id", peerID,
		"job_id", jobID.String(),
		"grpc_code", codes.FailedPrecondition,
		"task_kind", summary.taskKind,
		"model", summary.model,
		"lcp_error_code", lcpErr.Code,
	)
	return grpcStatusFromLCPError(*lcpErr)
}
