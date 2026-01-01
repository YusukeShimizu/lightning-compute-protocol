package requesterjobstore_test

import (
	"errors"
	"testing"
	"time"

	lcpdv1 "github.com/bruwbird/lcp/go-lcpd/gen/go/lcpd/v1"
	"github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"
	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
	"github.com/bruwbird/lcp/go-lcpd/internal/requesterjobstore"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestPutQuoteAndGetTerms(t *testing.T) {
	t.Parallel()

	now := time.Unix(100, 0)
	store := newStoreWithClock(now)

	jobID := newJobID(0x01)
	terms := &lcpdv1.Terms{
		ProtocolVersion: uint32(lcpwire.ProtocolVersionV02),
		JobId:           jobID[:],
		PriceMsat:       10,
		QuoteExpiry:     timestamppb.New(now.Add(5 * time.Minute)),
	}
	task := &lcpdv1.Task{
		Spec: &lcpdv1.Task_LlmChat{
			LlmChat: &lcpdv1.LLMChatTaskSpec{
				Prompt: "hello",
				Params: &lcpdv1.LLMChatParams{Profile: "p"},
			},
		},
	}

	if err := store.PutQuote("peer-a", task, terms); err != nil {
		t.Fatalf("PutQuote: %v", err)
	}

	gotTerms, err := store.GetTerms("peer-a", jobID)
	if err != nil {
		t.Fatalf("GetTerms: %v", err)
	}
	if diff := cmp.Diff(terms, gotTerms, protocmp.Transform()); diff != "" {
		t.Fatalf("terms mismatch (-want +got):\n%s", diff)
	}
	if gotTerms == terms {
		t.Fatalf("GetTerms returned the same pointer")
	}

	job, ok := store.Get("peer-a", jobID)
	if !ok {
		t.Fatalf("Get did not find stored job")
	}
	if job.State != requesterjobstore.StateQuoted {
		t.Fatalf("state mismatch: got %s want %s", job.State, requesterjobstore.StateQuoted)
	}
	if !job.CreatedAt.Equal(now) || !job.UpdatedAt.Equal(now) {
		t.Fatalf("timestamps not set to clock now")
	}
}

func TestGetTerms_Expired(t *testing.T) {
	t.Parallel()

	now := time.Unix(200, 0)
	store := newStoreWithClock(now)

	jobID := newJobID(0x02)
	terms := &lcpdv1.Terms{
		ProtocolVersion: uint32(lcpwire.ProtocolVersionV02),
		JobId:           jobID[:],
		PriceMsat:       20,
		QuoteExpiry:     timestamppb.New(now.Add(-time.Second)),
	}

	task := &lcpdv1.Task{
		Spec: &lcpdv1.Task_LlmChat{
			LlmChat: &lcpdv1.LLMChatTaskSpec{
				Prompt: "hello",
				Params: &lcpdv1.LLMChatParams{Profile: "p"},
			},
		},
	}

	if err := store.PutQuote("peer-b", task, terms); err != nil {
		t.Fatalf("PutQuote: %v", err)
	}

	_, err := store.GetTerms("peer-b", jobID)
	if !errors.Is(err, requesterjobstore.ErrExpired) {
		t.Fatalf(
			"GetTerms error mismatch (-want +got):\n%s",
			cmp.Diff(requesterjobstore.ErrExpired, err),
		)
	}

	job, ok := store.Get("peer-b", jobID)
	if !ok {
		t.Fatalf("job not found after expiry")
	}
	if job.State != requesterjobstore.StateExpired {
		t.Fatalf(
			"state mismatch after expiry: got %s want %s",
			job.State,
			requesterjobstore.StateExpired,
		)
	}
	if !job.UpdatedAt.Equal(now) {
		t.Fatalf("UpdatedAt not set on expiry: got %v want %v", job.UpdatedAt, now)
	}
}

func TestMarkState(t *testing.T) {
	t.Parallel()

	now := time.Unix(300, 0)
	store := newStoreWithClock(now)

	jobID := newJobID(0x03)
	terms := &lcpdv1.Terms{
		ProtocolVersion: uint32(lcpwire.ProtocolVersionV02),
		JobId:           jobID[:],
		PriceMsat:       30,
		QuoteExpiry:     timestamppb.New(now.Add(10 * time.Minute)),
	}
	task := &lcpdv1.Task{
		Spec: &lcpdv1.Task_LlmChat{
			LlmChat: &lcpdv1.LLMChatTaskSpec{
				Prompt: "hello",
				Params: &lcpdv1.LLMChatParams{Profile: "p"},
			},
		},
	}

	if err := store.PutQuote("peer-c", task, terms); err != nil {
		t.Fatalf("PutQuote: %v", err)
	}

	store.AdvanceClock(time.Minute)

	if err := store.MarkState("peer-c", jobID, requesterjobstore.StatePaying); err != nil {
		t.Fatalf("MarkState: %v", err)
	}

	job, ok := store.Get("peer-c", jobID)
	if !ok {
		t.Fatalf("job not found after mark state")
	}
	if job.State != requesterjobstore.StatePaying {
		t.Fatalf("state mismatch: got %s want %s", job.State, requesterjobstore.StatePaying)
	}
	if !job.UpdatedAt.Equal(time.Unix(300, 0).Add(time.Minute)) {
		t.Fatalf("UpdatedAt mismatch: got %v", job.UpdatedAt)
	}
}

func TestGC_ExpiresQuotedJobsOnly(t *testing.T) {
	t.Parallel()

	now := time.Unix(400, 0)
	store := newStoreWithClock(now)

	jobQuoted := newJobID(0x04)
	quotedTerms := &lcpdv1.Terms{
		ProtocolVersion: uint32(lcpwire.ProtocolVersionV02),
		JobId:           jobQuoted[:],
		PriceMsat:       40,
		QuoteExpiry:     timestamppb.New(now.Add(-time.Second)),
	}
	jobDone := newJobID(0x05)
	doneTerms := &lcpdv1.Terms{
		ProtocolVersion: uint32(lcpwire.ProtocolVersionV02),
		JobId:           jobDone[:],
		PriceMsat:       50,
		QuoteExpiry:     timestamppb.New(now.Add(-time.Hour)),
	}

	task := &lcpdv1.Task{
		Spec: &lcpdv1.Task_LlmChat{
			LlmChat: &lcpdv1.LLMChatTaskSpec{
				Prompt: "hello",
				Params: &lcpdv1.LLMChatParams{Profile: "p"},
			},
		},
	}

	if err := store.PutQuote("peer-d", task, quotedTerms); err != nil {
		t.Fatalf("PutQuote quoted: %v", err)
	}
	if err := store.PutQuote("peer-d", task, doneTerms); err != nil {
		t.Fatalf("PutQuote done: %v", err)
	}
	if err := store.MarkState("peer-d", jobDone, requesterjobstore.StateDone); err != nil {
		t.Fatalf("MarkState done: %v", err)
	}

	store.GC()

	job, _ := store.Get("peer-d", jobQuoted)
	if job.State != requesterjobstore.StateExpired {
		t.Fatalf("quoted job not expired by GC: %s", job.State)
	}

	job, _ = store.Get("peer-d", jobDone)
	if job.State != requesterjobstore.StateDone {
		t.Fatalf("done job state changed unexpectedly: %s", job.State)
	}
}

func TestPeerIsolation(t *testing.T) {
	t.Parallel()

	now := time.Unix(500, 0)
	store := newStoreWithClock(now)

	jobID := newJobID(0x06)
	terms := &lcpdv1.Terms{
		ProtocolVersion: uint32(lcpwire.ProtocolVersionV02),
		JobId:           jobID[:],
		PriceMsat:       60,
		QuoteExpiry:     timestamppb.New(now.Add(time.Hour)),
	}
	task := &lcpdv1.Task{
		Spec: &lcpdv1.Task_LlmChat{
			LlmChat: &lcpdv1.LLMChatTaskSpec{
				Prompt: "hello",
				Params: &lcpdv1.LLMChatParams{Profile: "p"},
			},
		},
	}

	if err := store.PutQuote("peer-e", task, terms); err != nil {
		t.Fatalf("PutQuote peer-e: %v", err)
	}

	// Same job_id but different peer.
	if err := store.PutQuote("peer-f", task, terms); err != nil {
		t.Fatalf("PutQuote peer-f: %v", err)
	}

	if err := store.MarkState("peer-f", jobID, requesterjobstore.StateAwaitingResult); err != nil {
		t.Fatalf("MarkState peer-f: %v", err)
	}

	jobE, _ := store.Get("peer-e", jobID)
	jobF, _ := store.Get("peer-f", jobID)

	if jobE.State != requesterjobstore.StateQuoted {
		t.Fatalf("peer-e state mutated: %s", jobE.State)
	}
	if jobF.State != requesterjobstore.StateAwaitingResult {
		t.Fatalf("peer-f state mismatch: %s", jobF.State)
	}
}

func TestPutQuote_Validation(t *testing.T) {
	t.Parallel()

	now := time.Unix(600, 0)
	store := newStoreWithClock(now)

	jobID := newJobID(0x07)
	validTerms := &lcpdv1.Terms{
		ProtocolVersion: uint32(lcpwire.ProtocolVersionV02),
		JobId:           jobID[:],
		PriceMsat:       70,
		QuoteExpiry:     timestamppb.New(now.Add(time.Hour)),
	}
	task := &lcpdv1.Task{
		Spec: &lcpdv1.Task_LlmChat{
			LlmChat: &lcpdv1.LLMChatTaskSpec{
				Prompt: "hello",
				Params: &lcpdv1.LLMChatParams{Profile: "p"},
			},
		},
	}

	tests := []struct {
		name    string
		peerID  string
		task    *lcpdv1.Task
		terms   *lcpdv1.Terms
		wantErr error
	}{
		{
			name:    "missing peer id",
			peerID:  "",
			task:    task,
			terms:   validTerms,
			wantErr: requesterjobstore.ErrPeerIDRequired,
		},
		{
			name:    "nil task",
			peerID:  "peer",
			task:    nil,
			terms:   validTerms,
			wantErr: requesterjobstore.ErrTaskRequired,
		},
		{
			name:    "nil terms",
			peerID:  "peer",
			task:    task,
			terms:   nil,
			wantErr: requesterjobstore.ErrTermsRequired,
		},
		{
			name:   "job_id wrong length",
			peerID: "peer",
			task:   task,
			terms: &lcpdv1.Terms{
				ProtocolVersion: uint32(lcpwire.ProtocolVersionV02),
				JobId:           []byte{0x01, 0x02},
				QuoteExpiry:     timestamppb.New(now.Add(time.Hour)),
			},
			wantErr: requesterjobstore.ErrInvalidJobID,
		},
		{
			name:   "quote expiry nil",
			peerID: "peer",
			task:   task,
			terms: &lcpdv1.Terms{
				ProtocolVersion: uint32(lcpwire.ProtocolVersionV02),
				JobId:           jobID[:],
			},
			wantErr: requesterjobstore.ErrQuoteExpiryRequired,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := store.PutQuote(tc.peerID, tc.task, tc.terms)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("PutQuote error mismatch (-want +got):\n%s", cmp.Diff(tc.wantErr, err))
			}
		})
	}
}

// newStoreWithClock creates a store whose clock starts at base and can be advanced.
func newStoreWithClock(base time.Time) *testableStore {
	return &testableStore{
		Store: requesterjobstore.NewWithClock(func() time.Time { return base }),
		clock: &base,
	}
}

type testableStore struct {
	*requesterjobstore.Store

	clock *time.Time
}

func (s *testableStore) AdvanceClock(d time.Duration) {
	*s.clock = s.clock.Add(d)
	s.SetClock(func() time.Time { return *s.clock })
}

func newJobID(fill byte) lcp.JobID {
	var id lcp.JobID
	for i := range id {
		id[i] = fill
	}
	return id
}
