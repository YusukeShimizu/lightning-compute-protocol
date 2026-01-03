package lcpd

import (
	"context"
	"encoding/hex"
	"time"

	"github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"
	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
	"github.com/bruwbird/lcp/go-lcpd/internal/requesterjobstore"
	lcpdv1 "github.com/bruwbird/lcp/proto-go/lcpd/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const taskKindLLMChat = "llm.chat"

type taskLogSummary struct {
	taskKind         string
	profile          string
	promptBytes      int
	maxOutputTokens  uint32
	temperatureMilli uint32
}

func summarizeTask(task *lcpdv1.Task) taskLogSummary {
	if task == nil {
		return taskLogSummary{}
	}

	chat := task.GetLlmChat()
	if chat == nil {
		return taskLogSummary{}
	}

	return taskLogSummary{
		taskKind:         taskKindLLMChat,
		profile:          chat.GetParams().GetProfile(),
		promptBytes:      len(chat.GetPrompt()),
		maxOutputTokens:  chat.GetParams().GetMaxOutputTokens(),
		temperatureMilli: chat.GetParams().GetTemperatureMilli(),
	}
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
		"profile", summary.profile,
		"prompt_bytes", summary.promptBytes,
		"max_output_tokens", summary.maxOutputTokens,
		"temperature_milli", summary.temperatureMilli,
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
		resultBytes = len(res.GetResult())
	}

	s.logger.Infow(
		"result received",
		"peer_id", peerID,
		"job_id", jobID.String(),
		"task_kind", summary.taskKind,
		"profile", summary.profile,
		"prompt_bytes", summary.promptBytes,
		"max_output_tokens", summary.maxOutputTokens,
		"temperature_milli", summary.temperatureMilli,
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
		"profile", summary.profile,
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
		"profile", summary.profile,
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
			"profile", summary.profile,
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
		"profile", summary.profile,
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
		"profile", summary.profile,
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
		"profile", summary.profile,
		"lcp_error_code", lcpErr.Code,
	)
	return grpcStatusFromLCPError(*lcpErr)
}
