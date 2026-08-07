package pxpipe

import (
	"container/list"
	"sync"
	"time"
)

const (
	sessionStateCapacity    = 512
	unknownCacheColdHorizon = time.Hour + 30*time.Second
)

type sessionRecord struct {
	key            string
	lastSeen       time.Time
	freezeStep     int
	cacheDead      bool
	cacheObserved  bool
	lastCacheAlive bool
	everCacheAlive bool
}

type sessionStateEntry struct {
	record sessionRecord
}

// sessionStateStore is intended to be owned by one handler. Its lock also
// makes response accounting safe to apply while another request transforms.
type sessionStateStore struct {
	mu             sync.Mutex
	entries        map[string]*list.Element
	lru            list.List
	capacity       int
	unknownHorizon time.Duration
}

type historySessionState struct {
	cold          bool
	minFreezeStep int
}

func newSessionStateStore() *sessionStateStore {
	return &sessionStateStore{
		entries:        make(map[string]*list.Element),
		capacity:       sessionStateCapacity,
		unknownHorizon: unknownCacheColdHorizon,
	}
}

func (s *sessionStateStore) touchLocked(key string) *sessionRecord {
	if elem := s.entries[key]; elem != nil {
		s.lru.MoveToFront(elem)
		return &elem.Value.(*sessionStateEntry).record
	}

	entry := &sessionStateEntry{record: sessionRecord{key: key}}
	elem := s.lru.PushFront(entry)
	s.entries[key] = elem
	for s.lru.Len() > s.capacity {
		oldest := s.lru.Back()
		if oldest == nil {
			break
		}
		delete(s.entries, oldest.Value.(*sessionStateEntry).record.key)
		s.lru.Remove(oldest)
	}
	return &entry.record
}

// noteHistoryRequest advances the request clock and returns the history-grid
// constraints for this turn. A cold signal authorizes exactly one packFill.
func (s *sessionStateStore) noteHistoryRequest(sessionKey string, now time.Time) historySessionState {
	if s == nil || sessionKey == "" {
		return historySessionState{}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.touchLocked(sessionKey)
	known := !rec.lastSeen.IsZero()
	serverSaysAlive := rec.cacheObserved && rec.lastCacheAlive
	serverSaysGone := rec.cacheObserved && !rec.lastCacheAlive && rec.everCacheAlive
	beyondHorizon := known && now.Sub(rec.lastSeen) > s.unknownHorizon
	cold := rec.cacheDead || (!serverSaysAlive && (serverSaysGone || beyondHorizon))
	if rec.lastSeen.IsZero() || now.After(rec.lastSeen) {
		rec.lastSeen = now
	}
	rec.cacheDead = false
	return historySessionState{cold: cold, minFreezeStep: rec.freezeStep}
}

// noteCacheOutcome records provider accounting for an accepted response. A
// zero/zero outcome proves a cache dead only if this session previously had one;
// sessions that never cache stay on the stable, conservative grid.
func (s *sessionStateStore) noteCacheOutcome(sessionKey string, cacheReadTokens, cacheCreateTokens int64) {
	if s == nil || sessionKey == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	elem := s.entries[sessionKey]
	if elem == nil {
		return
	}
	rec := &elem.Value.(*sessionStateEntry).record
	rec.cacheObserved = true
	rec.lastCacheAlive = cacheReadTokens > 0 || cacheCreateTokens > 0
	if rec.lastCacheAlive {
		rec.everCacheAlive = true
	}
}

// recordFreezeStep pins a monotonic floor. A finer future grid would re-key
// every chunk that had already been cached.
func (s *sessionStateStore) recordFreezeStep(sessionKey string, step int) {
	if s == nil || sessionKey == "" || step <= 0 {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.touchLocked(sessionKey)
	if step > rec.freezeStep {
		rec.freezeStep = step
	}
}

func (s *sessionStateStore) markCacheDead(sessionKey string) {
	if s == nil || sessionKey == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.touchLocked(sessionKey).cacheDead = true
}
