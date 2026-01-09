package jobstore

import (
	"sync"
	"time"

	"github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"
	"github.com/bruwbird/lcp/go-lcpd/internal/envconfig"
	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
)

type State string

const (
	StateAwaitingInput   State = "awaiting_input"
	StateQuoted          State = "quoted"
	StateWaitingPayment  State = "waiting_payment"
	StatePaid            State = "paid"
	StateExecuting       State = "executing"
	StateStreamingResult State = "streaming_result"
	StateDone            State = "done"
	StateCanceled        State = "canceled"
	StateFailed          State = "failed"
)

type Key struct {
	PeerPubKey string
	JobID      lcp.JobID
}

type Job struct {
	PeerPubKey string
	JobID      lcp.JobID
	State      State

	Call *lcpwire.Call

	InputStream *InputStreamState

	QuoteExpiry     uint64
	TermsHash       *lcp.Hash32
	PaymentHash     *lcp.Hash32
	InvoiceAddIndex *uint64

	Quote *lcpwire.Quote

	CreatedAt time.Time
}

type InputStreamState struct {
	StreamID lcp.Hash32

	ContentType     string
	ContentEncoding string

	ExpectedSeq uint32
	Buf         []byte

	TotalLen uint64
	SHA256   lcp.Hash32

	Validated bool
}

type Store struct {
	mu         sync.RWMutex
	jobs       map[Key]Job
	maxEntries int
	clock      func() time.Time
}

const defaultMaxEntries = 1024

func New() *Store {
	return NewWithClock(time.Now)
}

func NewWithClock(now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	maxEntries := envconfig.Int("LCP_DEFAULT_MAX_STORE_ENTRIES", defaultMaxEntries)
	if maxEntries <= 0 {
		maxEntries = defaultMaxEntries
	}

	return &Store{
		jobs:       make(map[Key]Job),
		maxEntries: maxEntries,
		clock:      now,
	}
}

func (s *Store) Upsert(job Job) {
	job = cloneJob(job)

	key := Key{
		PeerPubKey: job.PeerPubKey,
		JobID:      job.JobID,
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.nowLocked()
	if job.CreatedAt.IsZero() {
		job.CreatedAt = now
	}
	nowUnix := now.Unix()
	if nowUnix >= 0 {
		s.pruneExpiredLocked(uint64(nowUnix))
	}

	s.jobs[key] = job
	s.evictIfOverCapacityLocked()
}

func (s *Store) Get(key Key) (Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	job, ok := s.jobs[key]
	if !ok {
		return Job{}, false
	}

	return cloneJob(job), true
}

func (s *Store) UpdateState(key Key, state State) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	job, ok := s.jobs[key]
	if !ok {
		return false
	}

	job.State = state
	s.jobs[key] = job
	return true
}

func (s *Store) SetQuote(key Key, quote lcpwire.Quote) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	job, ok := s.jobs[key]
	if !ok {
		return false
	}

	quoteCopy := quote
	job.Quote = &quoteCopy
	job.QuoteExpiry = quote.QuoteExpiry

	termsCopy := quote.TermsHash
	job.TermsHash = &termsCopy

	s.jobs[key] = job
	return true
}

func (s *Store) SetPaymentHash(key Key, paymentHash lcp.Hash32) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	job, ok := s.jobs[key]
	if !ok {
		return false
	}

	hashCopy := paymentHash
	job.PaymentHash = &hashCopy

	s.jobs[key] = job
	return true
}

func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.jobs)
}

func cloneJob(job Job) Job {
	if job.Call != nil {
		reqCopy := *job.Call
		if reqCopy.ParamsBytes != nil {
			paramsCopy := append([]byte(nil), (*reqCopy.ParamsBytes)...)
			reqCopy.ParamsBytes = &paramsCopy
		}
		job.Call = &reqCopy
	}
	if job.InputStream != nil {
		streamCopy := *job.InputStream
		if len(streamCopy.Buf) > 0 {
			streamCopy.Buf = append([]byte(nil), streamCopy.Buf...)
		}
		job.InputStream = &streamCopy
	}
	if job.TermsHash != nil {
		termsCopy := *job.TermsHash
		job.TermsHash = &termsCopy
	}
	if job.PaymentHash != nil {
		hashCopy := *job.PaymentHash
		job.PaymentHash = &hashCopy
	}
	if job.InvoiceAddIndex != nil {
		addIndexCopy := *job.InvoiceAddIndex
		job.InvoiceAddIndex = &addIndexCopy
	}
	if job.Quote != nil {
		quoteCopy := *job.Quote
		job.Quote = &quoteCopy
	}
	return job
}

func (s *Store) nowLocked() time.Time {
	if s.clock == nil {
		return time.Now()
	}
	return s.clock()
}

func (s *Store) pruneExpiredLocked(nowUnix uint64) {
	for k, job := range s.jobs {
		if job.QuoteExpiry == 0 {
			continue
		}
		if job.QuoteExpiry >= nowUnix {
			continue
		}

		switch job.State {
		case StateAwaitingInput, StateQuoted, StateWaitingPayment:
			delete(s.jobs, k)
		case StatePaid,
			StateExecuting,
			StateStreamingResult,
			StateDone,
			StateCanceled,
			StateFailed:
			continue
		}
	}
}

func (s *Store) evictIfOverCapacityLocked() {
	if s.maxEntries <= 0 {
		s.maxEntries = envconfig.Int("LCP_DEFAULT_MAX_STORE_ENTRIES", defaultMaxEntries)
		if s.maxEntries <= 0 {
			s.maxEntries = defaultMaxEntries
		}
	}

	for len(s.jobs) > s.maxEntries {
		if evicted := s.evictOldestLocked(true); evicted {
			continue
		}
		_ = s.evictOldestLocked(false)
	}
}

func (s *Store) evictOldestLocked(preferPending bool) bool {
	if len(s.jobs) == 0 {
		return false
	}

	var (
		oldestKey Key
		oldestJob Job
		found     bool
	)

	for k, job := range s.jobs {
		if preferPending {
			switch job.State {
			case StateAwaitingInput, StateQuoted, StateWaitingPayment:
				// ok
			case StatePaid,
				StateExecuting,
				StateStreamingResult,
				StateDone,
				StateCanceled,
				StateFailed:
				continue
			}
		}

		if !found || job.CreatedAt.Before(oldestJob.CreatedAt) {
			oldestKey = k
			oldestJob = job
			found = true
		}
	}

	if !found {
		return false
	}
	delete(s.jobs, oldestKey)
	return true
}
