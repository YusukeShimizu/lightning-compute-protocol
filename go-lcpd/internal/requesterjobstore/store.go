package requesterjobstore

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"
	"github.com/bruwbird/lcp/go-lcpd/internal/envconfig"
	lcpdv1 "github.com/bruwbird/lcp/proto-go/lcpd/v1"
	"google.golang.org/protobuf/proto"
)

type State string

const (
	StateQuoted         State = "quoted"
	StatePaying         State = "paying"
	StateAwaitingResult State = "awaiting_result"
	StateDone           State = "done"
	StateCancelled      State = "cancelled"
	StateFailed         State = "failed"
	StateExpired        State = "expired"
)

var (
	ErrPeerIDRequired      = errors.New("peer_id is required")
	ErrCallRequired        = errors.New("call is required")
	ErrQuoteRequired       = errors.New("quote is required")
	ErrInvalidCallID       = errors.New("call_id must be 32 bytes")
	ErrQuoteExpiryRequired = errors.New("quote_expiry is required")
	ErrInvalidState        = errors.New("invalid state")
	ErrNotFound            = errors.New("job not found")
	ErrExpired             = errors.New("job expired")
)

type key struct {
	peerID string
	jobID  lcp.JobID
}

type jobEntry struct {
	peerID      string
	jobID       lcp.JobID
	call        *lcpdv1.CallSpec
	quote       *lcpdv1.Quote
	state       State
	quoteExpiry time.Time
	createdAt   time.Time
	updatedAt   time.Time
}

type Job struct {
	PeerID    string
	JobID     lcp.JobID
	Call      *lcpdv1.CallSpec
	Quote     *lcpdv1.Quote
	State     State
	CreatedAt time.Time
	UpdatedAt time.Time
}

type Store struct {
	mu         sync.RWMutex
	jobs       map[key]jobEntry
	clock      func() time.Time
	maxEntries int
}

const defaultMaxEntries = 1024

func New() *Store {
	return NewWithClock(time.Now)
}

func NewWithClock(now func() time.Time) *Store {
	maxEntries := envconfig.Int("LCP_DEFAULT_MAX_STORE_ENTRIES", defaultMaxEntries)
	return NewWithClockAndMaxEntries(now, maxEntries)
}

func NewWithClockAndMaxEntries(now func() time.Time, maxEntries int) *Store {
	if now == nil {
		now = time.Now
	}
	if maxEntries <= 0 {
		maxEntries = defaultMaxEntries
	}
	return &Store{
		jobs:       make(map[key]jobEntry),
		clock:      now,
		maxEntries: maxEntries,
	}
}

// SetClock overrides the clock used for timestamps (intended for tests).
func (s *Store) SetClock(now func() time.Time) {
	if now == nil {
		now = time.Now
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clock = now
}

// PutQuote stores a quoted call. Existing entries for the same peer+call_id are replaced.
func (s *Store) PutQuote(peerID string, call *lcpdv1.CallSpec, quote *lcpdv1.Quote) error {
	if peerID == "" {
		return ErrPeerIDRequired
	}
	if call == nil {
		return ErrCallRequired
	}
	if quote == nil {
		return ErrQuoteRequired
	}

	jobID, err := toJobID(quote.GetCallId())
	if err != nil {
		return err
	}

	quoteExpiry := quote.GetQuoteExpiry()
	if quoteExpiry == nil {
		return ErrQuoteExpiryRequired
	}

	now := s.now()

	callClone := proto.Clone(call)
	callCopy, ok := callClone.(*lcpdv1.CallSpec)
	if !ok {
		return fmt.Errorf("clone call: unexpected type %T", callClone)
	}

	quoteClone := proto.Clone(quote)
	quoteCopy, ok := quoteClone.(*lcpdv1.Quote)
	if !ok {
		return fmt.Errorf("clone quote: unexpected type %T", quoteClone)
	}

	k := key{peerID: peerID, jobID: jobID}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.gcLocked(s.nowLocked())
	s.evictIfOverCapacityLocked()

	createdAt := now
	if existing, found := s.jobs[k]; found {
		if !existing.createdAt.IsZero() {
			createdAt = existing.createdAt
		}
	}

	entry := jobEntry{
		peerID:      peerID,
		jobID:       jobID,
		call:        callCopy,
		quote:       quoteCopy,
		state:       StateQuoted,
		quoteExpiry: quoteExpiry.AsTime(),
		createdAt:   createdAt,
		updatedAt:   now,
	}

	s.jobs[k] = entry
	s.evictIfOverCapacityLocked()
	return nil
}

// GetQuote returns a copy of the stored quote for the call. If the quote_expiry
// has passed, the job is marked expired and ErrExpired is returned.
func (s *Store) GetQuote(peerID string, jobID lcp.JobID) (*lcpdv1.Quote, error) {
	k := key{peerID: peerID, jobID: jobID}

	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.jobs[k]
	if !ok {
		return nil, ErrNotFound
	}

	now := s.nowLocked()
	if entry.state == StateQuoted && !entry.quoteExpiry.IsZero() && !now.Before(entry.quoteExpiry) {
		entry.state = StateExpired
		entry.updatedAt = now
		s.jobs[k] = entry
		return nil, ErrExpired
	}
	if entry.state == StateExpired {
		return nil, ErrExpired
	}

	quoteClone := proto.Clone(entry.quote)
	quoteCopy, ok := quoteClone.(*lcpdv1.Quote)
	if !ok {
		return nil, fmt.Errorf("clone quote: unexpected type %T", quoteClone)
	}
	return quoteCopy, nil
}

// MarkState sets the job state. Returns ErrNotFound if the job does not exist.
func (s *Store) MarkState(peerID string, jobID lcp.JobID, state State) error {
	if !isValidState(state) {
		return ErrInvalidState
	}

	k := key{peerID: peerID, jobID: jobID}

	now := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.jobs[k]
	if !ok {
		return ErrNotFound
	}

	entry.state = state
	entry.updatedAt = now
	s.jobs[k] = entry
	return nil
}

// GC marks quoted jobs whose quote_expiry has passed as expired.
func (s *Store) GC() {
	now := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.gcLocked(now)
	s.evictIfOverCapacityLocked()
}

// Get returns a copy of the stored Job if present.
func (s *Store) Get(peerID string, jobID lcp.JobID) (Job, bool) {
	k := key{peerID: peerID, jobID: jobID}

	s.mu.RLock()
	defer s.mu.RUnlock()

	entry, ok := s.jobs[k]
	if !ok {
		return Job{}, false
	}

	return cloneJob(entry), true
}

func (s *Store) now() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.clock()
}

func (s *Store) nowLocked() time.Time {
	return s.clock()
}

func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.jobs)
}

func (s *Store) gcLocked(now time.Time) {
	for k, entry := range s.jobs {
		if entry.state != StateQuoted {
			continue
		}
		if entry.quoteExpiry.IsZero() {
			continue
		}
		if now.Before(entry.quoteExpiry) {
			continue
		}
		entry.state = StateExpired
		entry.updatedAt = now
		s.jobs[k] = entry
	}
}

func (s *Store) evictIfOverCapacityLocked() {
	if s.maxEntries <= 0 {
		s.maxEntries = defaultMaxEntries
	}

	for len(s.jobs) > s.maxEntries {
		if evicted := s.evictOldestLocked(true); evicted {
			continue
		}
		_ = s.evictOldestLocked(false)
	}
}

func (s *Store) evictOldestLocked(preferExpired bool) bool {
	if len(s.jobs) == 0 {
		return false
	}

	var (
		oldestKey key
		oldest    jobEntry
		found     bool
	)

	for k, entry := range s.jobs {
		if preferExpired && entry.state != StateExpired {
			continue
		}
		if !found || entry.createdAt.Before(oldest.createdAt) {
			oldestKey = k
			oldest = entry
			found = true
		}
	}

	if !found {
		return false
	}
	delete(s.jobs, oldestKey)
	return true
}

func toJobID(b []byte) (lcp.JobID, error) {
	if len(b) != lcp.Hash32Len {
		return lcp.JobID{}, ErrInvalidCallID
	}
	var id lcp.JobID
	copy(id[:], b)
	return id, nil
}

func isValidState(state State) bool {
	switch state {
	case StateQuoted,
		StatePaying,
		StateAwaitingResult,
		StateDone,
		StateCancelled,
		StateFailed,
		StateExpired:
		return true
	default:
		return false
	}
}

func cloneJob(entry jobEntry) Job {
	var callCopy *lcpdv1.CallSpec
	if entry.call != nil {
		if cloned, ok := proto.Clone(entry.call).(*lcpdv1.CallSpec); ok {
			callCopy = cloned
		}
	}
	var quoteCopy *lcpdv1.Quote
	if entry.quote != nil {
		if cloned, ok := proto.Clone(entry.quote).(*lcpdv1.Quote); ok {
			quoteCopy = cloned
		}
	}
	return Job{
		PeerID:    entry.peerID,
		JobID:     entry.jobID,
		Call:      callCopy,
		Quote:     quoteCopy,
		State:     entry.state,
		CreatedAt: entry.createdAt,
		UpdatedAt: entry.updatedAt,
	}
}
