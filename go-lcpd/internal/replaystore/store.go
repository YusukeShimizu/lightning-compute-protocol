package replaystore

import (
	"sync"

	"github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"
	"github.com/bruwbird/lcp/go-lcpd/internal/envconfig"
	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
)

type CheckResult string

const (
	CheckResultOK        CheckResult = "ok"
	CheckResultDuplicate CheckResult = "duplicate"
	CheckResultExpired   CheckResult = "expired"

	defaultMaxEntries = 1024
)

type Key struct {
	PeerPubKey string
	JobID      lcp.JobID
	MsgID      lcpwire.MsgID
}

type Store struct {
	mu         sync.RWMutex
	entries    map[Key]uint64
	maxEntries int
}

func New(maxEntries int) *Store {
	if maxEntries <= 0 {
		maxEntries = envconfig.Int("LCP_DEFAULT_MAX_STORE_ENTRIES", defaultMaxEntries)
		if maxEntries <= 0 {
			maxEntries = defaultMaxEntries
		}
	}

	return &Store{
		entries:    make(map[Key]uint64),
		maxEntries: maxEntries,
	}
}

func (s *Store) CheckAndRemember(key Key, expiry uint64, now uint64) CheckResult {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.pruneLocked(now)

	if expiry < now {
		return CheckResultExpired
	}

	if currentExpiry, ok := s.entries[key]; ok {
		if currentExpiry >= now {
			return CheckResultDuplicate
		}
		delete(s.entries, key)
	}

	if len(s.entries) >= s.maxEntries {
		s.evictOldestLocked()
	}

	s.entries[key] = expiry
	return CheckResultOK
}

func (s *Store) Prune(now uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
}

func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.entries)
}

func (s *Store) Contains(key Key) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.entries[key]
	return ok
}

func (s *Store) pruneLocked(now uint64) {
	for key, expiry := range s.entries {
		if expiry < now {
			delete(s.entries, key)
		}
	}
}

func (s *Store) evictOldestLocked() {
	if len(s.entries) == 0 {
		return
	}

	var (
		oldestKey    Key
		oldestExpiry uint64
		first        = true
	)

	for key, expiry := range s.entries {
		if first || expiry < oldestExpiry {
			oldestKey = key
			oldestExpiry = expiry
			first = false
		}
	}

	delete(s.entries, oldestKey)
}
