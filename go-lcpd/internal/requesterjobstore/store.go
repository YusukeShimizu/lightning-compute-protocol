package requesterjobstore

import (
	"errors"
	"fmt"
	"sync"
	"time"

	lcpdv1 "github.com/bruwbird/lcp/go-lcpd/gen/go/lcpd/v1"
	"github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"
	"github.com/bruwbird/lcp/go-lcpd/internal/envconfig"
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
	ErrTaskRequired        = errors.New("task is required")
	ErrTermsRequired       = errors.New("terms are required")
	ErrInvalidJobID        = errors.New("job_id must be 32 bytes")
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
	task        *lcpdv1.Task
	terms       *lcpdv1.Terms
	state       State
	quoteExpiry time.Time
	createdAt   time.Time
	updatedAt   time.Time
}

type Job struct {
	PeerID    string
	JobID     lcp.JobID
	Task      *lcpdv1.Task
	Terms     *lcpdv1.Terms
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

// PutQuote stores a quoted job. Existing entries for the same peer+job_id are replaced.
func (s *Store) PutQuote(peerID string, task *lcpdv1.Task, terms *lcpdv1.Terms) error {
	if peerID == "" {
		return ErrPeerIDRequired
	}
	if task == nil {
		return ErrTaskRequired
	}
	if terms == nil {
		return ErrTermsRequired
	}

	jobID, err := toJobID(terms.GetJobId())
	if err != nil {
		return err
	}

	quoteExpiry := terms.GetQuoteExpiry()
	if quoteExpiry == nil {
		return ErrQuoteExpiryRequired
	}

	now := s.now()

	taskClone := proto.Clone(task)
	taskCopy, ok := taskClone.(*lcpdv1.Task)
	if !ok {
		return fmt.Errorf("clone task: unexpected type %T", taskClone)
	}

	termsClone := proto.Clone(terms)
	termsCopy, ok := termsClone.(*lcpdv1.Terms)
	if !ok {
		return fmt.Errorf("clone terms: unexpected type %T", termsClone)
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
		task:        taskCopy,
		terms:       termsCopy,
		state:       StateQuoted,
		quoteExpiry: quoteExpiry.AsTime(),
		createdAt:   createdAt,
		updatedAt:   now,
	}

	s.jobs[k] = entry
	s.evictIfOverCapacityLocked()
	return nil
}

// GetTerms returns a copy of the stored terms for the job. If the quote_expiry
// has passed, the job is marked expired and ErrExpired is returned.
func (s *Store) GetTerms(peerID string, jobID lcp.JobID) (*lcpdv1.Terms, error) {
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

	termsClone := proto.Clone(entry.terms)
	termsCopy, ok := termsClone.(*lcpdv1.Terms)
	if !ok {
		return nil, fmt.Errorf("clone terms: unexpected type %T", termsClone)
	}
	return termsCopy, nil
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
		return lcp.JobID{}, ErrInvalidJobID
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
	var taskCopy *lcpdv1.Task
	if entry.task != nil {
		if cloned, ok := proto.Clone(entry.task).(*lcpdv1.Task); ok {
			taskCopy = cloned
		}
	}
	var termsCopy *lcpdv1.Terms
	if entry.terms != nil {
		if cloned, ok := proto.Clone(entry.terms).(*lcpdv1.Terms); ok {
			termsCopy = cloned
		}
	}
	return Job{
		PeerID:    entry.peerID,
		JobID:     entry.jobID,
		Task:      taskCopy,
		Terms:     termsCopy,
		State:     entry.state,
		CreatedAt: entry.createdAt,
		UpdatedAt: entry.updatedAt,
	}
}
