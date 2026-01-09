package requesterjobstore_test

import (
	"errors"
	"testing"
	"time"

	"github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"
	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
	"github.com/bruwbird/lcp/go-lcpd/internal/requesterjobstore"
	lcpdv1 "github.com/bruwbird/lcp/proto-go/lcpd/v1"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestPutQuoteAndGetTerms(t *testing.T) {
	t.Parallel()

	now := time.Unix(100, 0)
	store := newStoreWithClock(now)

	jobID := newJobID(0x01)
	quote := &lcpdv1.Quote{
		ProtocolVersion: uint32(lcpwire.ProtocolVersionV03),
		CallId:          jobID[:],
		PriceMsat:       10,
		QuoteExpiry:     timestamppb.New(now.Add(5 * time.Minute)),
	}
	call := newTestCall()

	if err := store.PutQuote("peer-a", call, quote); err != nil {
		t.Fatalf("PutQuote: %v", err)
	}

	gotQuote, err := store.GetQuote("peer-a", jobID)
	if err != nil {
		t.Fatalf("GetQuote: %v", err)
	}
	if diff := cmp.Diff(quote, gotQuote, protocmp.Transform()); diff != "" {
		t.Fatalf("quote mismatch (-want +got):\n%s", diff)
	}
	if gotQuote == quote {
		t.Fatalf("GetQuote returned the same pointer")
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
	quote := &lcpdv1.Quote{
		ProtocolVersion: uint32(lcpwire.ProtocolVersionV03),
		CallId:          jobID[:],
		PriceMsat:       20,
		QuoteExpiry:     timestamppb.New(now.Add(-time.Second)),
	}

	call := newTestCall()

	if err := store.PutQuote("peer-b", call, quote); err != nil {
		t.Fatalf("PutQuote: %v", err)
	}

	_, err := store.GetQuote("peer-b", jobID)
	if !errors.Is(err, requesterjobstore.ErrExpired) {
		t.Fatalf(
			"GetQuote error mismatch (-want +got):\n%s",
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
	quote := &lcpdv1.Quote{
		ProtocolVersion: uint32(lcpwire.ProtocolVersionV03),
		CallId:          jobID[:],
		PriceMsat:       30,
		QuoteExpiry:     timestamppb.New(now.Add(10 * time.Minute)),
	}
	call := newTestCall()

	if err := store.PutQuote("peer-c", call, quote); err != nil {
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
	quotedQuote := &lcpdv1.Quote{
		ProtocolVersion: uint32(lcpwire.ProtocolVersionV03),
		CallId:          jobQuoted[:],
		PriceMsat:       40,
		QuoteExpiry:     timestamppb.New(now.Add(-time.Second)),
	}
	jobDone := newJobID(0x05)
	doneQuote := &lcpdv1.Quote{
		ProtocolVersion: uint32(lcpwire.ProtocolVersionV03),
		CallId:          jobDone[:],
		PriceMsat:       50,
		QuoteExpiry:     timestamppb.New(now.Add(-time.Hour)),
	}

	call := newTestCall()

	if err := store.PutQuote("peer-d", call, quotedQuote); err != nil {
		t.Fatalf("PutQuote quoted: %v", err)
	}
	if err := store.PutQuote("peer-d", call, doneQuote); err != nil {
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
	quote := &lcpdv1.Quote{
		ProtocolVersion: uint32(lcpwire.ProtocolVersionV03),
		CallId:          jobID[:],
		PriceMsat:       60,
		QuoteExpiry:     timestamppb.New(now.Add(time.Hour)),
	}
	call := newTestCall()

	if err := store.PutQuote("peer-e", call, quote); err != nil {
		t.Fatalf("PutQuote peer-e: %v", err)
	}

	// Same job_id but different peer.
	if err := store.PutQuote("peer-f", call, quote); err != nil {
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
	validQuote := &lcpdv1.Quote{
		ProtocolVersion: uint32(lcpwire.ProtocolVersionV03),
		CallId:          jobID[:],
		PriceMsat:       70,
		QuoteExpiry:     timestamppb.New(now.Add(time.Hour)),
	}
	call := newTestCall()

	tests := []struct {
		name    string
		peerID  string
		call    *lcpdv1.CallSpec
		quote   *lcpdv1.Quote
		wantErr error
	}{
		{
			name:    "missing peer id",
			peerID:  "",
			call:    call,
			quote:   validQuote,
			wantErr: requesterjobstore.ErrPeerIDRequired,
		},
		{
			name:    "nil call",
			peerID:  "peer",
			call:    nil,
			quote:   validQuote,
			wantErr: requesterjobstore.ErrCallRequired,
		},
		{
			name:    "nil quote",
			peerID:  "peer",
			call:    call,
			quote:   nil,
			wantErr: requesterjobstore.ErrQuoteRequired,
		},
		{
			name:   "call_id wrong length",
			peerID: "peer",
			call:   call,
			quote: &lcpdv1.Quote{
				ProtocolVersion: uint32(lcpwire.ProtocolVersionV03),
				CallId:          []byte{0x01, 0x02},
				QuoteExpiry:     timestamppb.New(now.Add(time.Hour)),
			},
			wantErr: requesterjobstore.ErrInvalidCallID,
		},
		{
			name:   "quote expiry nil",
			peerID: "peer",
			call:   call,
			quote: &lcpdv1.Quote{
				ProtocolVersion: uint32(lcpwire.ProtocolVersionV03),
				CallId:          jobID[:],
			},
			wantErr: requesterjobstore.ErrQuoteExpiryRequired,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := store.PutQuote(tc.peerID, tc.call, tc.quote)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("PutQuote error mismatch (-want +got):\n%s", cmp.Diff(tc.wantErr, err))
			}
		})
	}
}

func newTestCall() *lcpdv1.CallSpec {
	return &lcpdv1.CallSpec{
		Method:                 "openai.chat_completions.v1",
		Params:                 []byte{0x01, 0x05, 'm', 'o', 'd', 'e', 'l'},
		RequestBytes:           []byte(`{"model":"model","messages":[{"role":"user","content":"hi"}]}`),
		RequestContentType:     "application/json; charset=utf-8",
		RequestContentEncoding: "identity",
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
