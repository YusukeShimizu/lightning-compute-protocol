package replaystore_test

import (
	"testing"

	"github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"
	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
	"github.com/bruwbird/lcp/go-lcpd/internal/replaystore"
	"github.com/google/go-cmp/cmp"
)

func TestStore_CheckAndRemember_RejectsExpired(t *testing.T) {
	t.Parallel()

	store := replaystore.New(0)
	key := newKey("peer-expired", 0x01, 0x02)

	if got := store.CheckAndRemember(key, 9, 10); got != replaystore.CheckResultExpired {
		t.Fatalf("CheckAndRemember expired: got %v want %v", got, replaystore.CheckResultExpired)
	}
	if diff := cmp.Diff(0, store.Len()); diff != "" {
		t.Fatalf("len mismatch (-want +got):\n%s", diff)
	}

	if got := store.CheckAndRemember(key, 20, 10); got != replaystore.CheckResultOK {
		t.Fatalf(
			"CheckAndRemember after expiry rejection: got %v want %v",
			got,
			replaystore.CheckResultOK,
		)
	}
}

func TestStore_CheckAndRemember_DuplicateWithinExpiry(t *testing.T) {
	t.Parallel()

	store := replaystore.New(0)
	key := newKey("peer-dup", 0x03, 0x04)

	if got := store.CheckAndRemember(key, 20, 5); got != replaystore.CheckResultOK {
		t.Fatalf("first insert: got %v want %v", got, replaystore.CheckResultOK)
	}
	if got := store.CheckAndRemember(key, 20, 6); got != replaystore.CheckResultDuplicate {
		t.Fatalf("duplicate insert: got %v want %v", got, replaystore.CheckResultDuplicate)
	}
	if diff := cmp.Diff(1, store.Len()); diff != "" {
		t.Fatalf("len mismatch (-want +got):\n%s", diff)
	}
}

func TestStore_CheckAndRemember_AllowsAfterPrunedExpiry(t *testing.T) {
	t.Parallel()

	store := replaystore.New(0)
	key := newKey("peer-prune", 0x05, 0x06)

	if got := store.CheckAndRemember(key, 7, 1); got != replaystore.CheckResultOK {
		t.Fatalf("first insert: got %v want %v", got, replaystore.CheckResultOK)
	}

	store.Prune(10)
	if store.Contains(key) {
		t.Fatalf("expected key to be pruned after expiry")
	}

	if got := store.CheckAndRemember(key, 20, 10); got != replaystore.CheckResultOK {
		t.Fatalf("after prune insert: got %v want %v", got, replaystore.CheckResultOK)
	}
}

func TestStore_EnforcesMaxEntriesWithOldestEviction(t *testing.T) {
	t.Parallel()

	store := replaystore.New(2)

	keyA := newKey("peer", 0x0a, 0x0a)
	keyB := newKey("peer", 0x0b, 0x0b)
	keyC := newKey("peer", 0x0c, 0x0c)

	if got := store.CheckAndRemember(keyA, 50, 0); got != replaystore.CheckResultOK {
		t.Fatalf("insert A: got %v want %v", got, replaystore.CheckResultOK)
	}
	if got := store.CheckAndRemember(keyB, 20, 0); got != replaystore.CheckResultOK {
		t.Fatalf("insert B: got %v want %v", got, replaystore.CheckResultOK)
	}
	if got := store.CheckAndRemember(keyC, 70, 0); got != replaystore.CheckResultOK {
		t.Fatalf("insert C: got %v want %v", got, replaystore.CheckResultOK)
	}

	if diff := cmp.Diff(2, store.Len()); diff != "" {
		t.Fatalf("len mismatch (-want +got):\n%s", diff)
	}
	if store.Contains(keyB) {
		t.Fatalf("expected oldest entry to be evicted")
	}
	if !store.Contains(keyA) || !store.Contains(keyC) {
		t.Fatalf("expected newer entries to remain")
	}
}

func newKey(peer string, jobByte byte, msgByte byte) replaystore.Key {
	var jobID lcp.JobID
	for i := range jobID {
		jobID[i] = jobByte
	}
	var msgID lcpwire.MsgID
	for i := range msgID {
		msgID[i] = msgByte
	}

	return replaystore.Key{
		PeerPubKey: peer,
		JobID:      jobID,
		MsgID:      msgID,
	}
}
