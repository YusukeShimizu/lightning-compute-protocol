package jobstore_test

import (
	"testing"
	"time"

	"github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"
	"github.com/bruwbird/lcp/go-lcpd/internal/jobstore"
	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
	"github.com/google/go-cmp/cmp"
)

func TestStore_UpsertAndGet(t *testing.T) {
	t.Parallel()

	store := jobstore.New()
	jobID := newJobID(0x01)
	createdAt := time.Unix(100, 0)

	job := jobstore.Job{
		PeerPubKey:      "peer-a",
		JobID:           jobID,
		State:           jobstore.StateQuoted,
		QuoteExpiry:     123,
		TermsHash:       hashPtr(0xaa),
		InvoiceAddIndex: ptrUint64(9),
		CreatedAt:       createdAt,
	}

	store.Upsert(job)

	got, ok := store.Get(jobstore.Key{PeerPubKey: "peer-a", JobID: jobID})
	if !ok {
		t.Fatalf("expected job to be found")
	}
	if diff := cmp.Diff(job, got); diff != "" {
		t.Fatalf("job mismatch (-want +got):\n%s", diff)
	}
}

func TestStore_UpdateState(t *testing.T) {
	t.Parallel()

	store := jobstore.New()
	jobID := newJobID(0x02)
	key := jobstore.Key{PeerPubKey: "peer-b", JobID: jobID}

	store.Upsert(jobstore.Job{
		PeerPubKey: key.PeerPubKey,
		JobID:      jobID,
		State:      jobstore.StateQuoted,
	})

	if ok := store.UpdateState(key, jobstore.StateExecuting); !ok {
		t.Fatalf("UpdateState returned false for existing job")
	}
	got, ok := store.Get(key)
	if !ok {
		t.Fatalf("job not found after update")
	}
	if got.State != jobstore.StateExecuting {
		t.Fatalf("state mismatch: got %v want %v", got.State, jobstore.StateExecuting)
	}

	if updated := store.UpdateState(jobstore.Key{PeerPubKey: "missing", JobID: jobID}, jobstore.StateDone); updated {
		t.Fatalf("UpdateState should return false for missing job")
	}
}

func TestStore_SetQuote(t *testing.T) {
	t.Parallel()

	store := jobstore.New()
	jobID := newJobID(0x03)
	msgID := newMsgID(0x10)
	key := jobstore.Key{PeerPubKey: "peer-c", JobID: jobID}

	store.Upsert(jobstore.Job{
		PeerPubKey: key.PeerPubKey,
		JobID:      jobID,
		State:      jobstore.StateQuoted,
	})

	quote := lcpwire.Quote{
		Envelope: lcpwire.CallEnvelope{
			ProtocolVersion: lcpwire.ProtocolVersionV03,
			CallID:          jobID,
			MsgID:           msgID,
			Expiry:          77,
		},
		PriceMsat:      42,
		QuoteExpiry:    55,
		TermsHash:      hash(0xbb),
		PaymentRequest: "bolt11",
	}

	if ok := store.SetQuote(key, quote); !ok {
		t.Fatalf("SetQuote returned false for existing job")
	}

	got, ok := store.Get(key)
	if !ok {
		t.Fatalf("job not found after SetQuote")
	}
	if got.QuoteExpiry != quote.QuoteExpiry {
		t.Fatalf("QuoteExpiry mismatch: got %d want %d", got.QuoteExpiry, quote.QuoteExpiry)
	}
	if got.Quote == nil {
		t.Fatalf("Quote not stored")
	}
	if diff := cmp.Diff(quote, *got.Quote); diff != "" {
		t.Fatalf("Quote mismatch (-want +got):\n%s", diff)
	}
	if got.TermsHash == nil {
		t.Fatalf("TermsHash not stored")
	}
	if diff := cmp.Diff(hashPtr(0xbb), got.TermsHash); diff != "" {
		t.Fatalf("TermsHash mismatch (-want +got):\n%s", diff)
	}
}

func TestStore_SetPaymentHash(t *testing.T) {
	t.Parallel()

	store := jobstore.New()
	jobID := newJobID(0x04)
	key := jobstore.Key{PeerPubKey: "peer-d", JobID: jobID}

	store.Upsert(jobstore.Job{
		PeerPubKey: key.PeerPubKey,
		JobID:      jobID,
		State:      jobstore.StateQuoted,
	})

	paymentHash := hash(0xcc)

	if ok := store.SetPaymentHash(key, paymentHash); !ok {
		t.Fatalf("SetPaymentHash returned false for existing job")
	}

	got, ok := store.Get(key)
	if !ok {
		t.Fatalf("job not found after SetPaymentHash")
	}
	if got.PaymentHash == nil {
		t.Fatalf("PaymentHash not stored")
	}
	if diff := cmp.Diff(paymentHash, *got.PaymentHash); diff != "" {
		t.Fatalf("PaymentHash mismatch (-want +got):\n%s", diff)
	}
}

func TestStore_DistinguishesPeersForSameJobID(t *testing.T) {
	t.Parallel()

	store := jobstore.New()
	jobID := newJobID(0x05)
	keyA := jobstore.Key{PeerPubKey: "peer-a", JobID: jobID}
	keyB := jobstore.Key{PeerPubKey: "peer-b", JobID: jobID}

	store.Upsert(
		jobstore.Job{PeerPubKey: keyA.PeerPubKey, JobID: jobID, State: jobstore.StateQuoted},
	)
	store.Upsert(
		jobstore.Job{PeerPubKey: keyB.PeerPubKey, JobID: jobID, State: jobstore.StateQuoted},
	)

	if ok := store.UpdateState(keyB, jobstore.StatePaid); !ok {
		t.Fatalf("UpdateState returned false for keyB")
	}

	jobA, _ := store.Get(keyA)
	jobB, _ := store.Get(keyB)

	if jobA.State != jobstore.StateQuoted {
		t.Fatalf("peer A state changed unexpectedly: got %v", jobA.State)
	}
	if jobB.State != jobstore.StatePaid {
		t.Fatalf("peer B state mismatch: got %v want %v", jobB.State, jobstore.StatePaid)
	}
}

func newJobID(fill byte) lcp.JobID {
	var id lcp.JobID
	for i := range id {
		id[i] = fill
	}
	return id
}

func newMsgID(fill byte) lcpwire.MsgID {
	var id lcpwire.MsgID
	for i := range id {
		id[i] = fill
	}
	return id
}

func hash(fill byte) lcp.Hash32 {
	var h lcp.Hash32
	for i := range h {
		h[i] = fill
	}
	return h
}

func hashPtr(fill byte) *lcp.Hash32 {
	h := hash(fill)
	return &h
}

func ptrUint64(v uint64) *uint64 {
	return &v
}
