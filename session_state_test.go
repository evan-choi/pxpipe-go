package pxpipe

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestSessionStateUnknownFallbackAndOneShotPackFill(t *testing.T) {
	s := newSessionStateStore()
	t0 := time.Unix(1_000, 0)
	if got := s.noteHistoryRequest("s", t0); got.cold {
		t.Fatal("new session was cold")
	}
	if got := s.noteHistoryRequest("s", t0.Add(unknownCacheColdHorizon)); got.cold {
		t.Fatal("fallback fired at the horizon instead of after it")
	}
	firstCold := t0.Add(2*unknownCacheColdHorizon + time.Second)
	if got := s.noteHistoryRequest("s", firstCold); !got.cold {
		t.Fatal("unknown session did not go cold after the fallback horizon")
	}
	if got := s.noteHistoryRequest("s", firstCold.Add(time.Second)); got.cold {
		t.Fatal("packFill signal was not consumed")
	}
	if got := s.noteHistoryRequest("s", firstCold.Add(unknownCacheColdHorizon+2*time.Second)); !got.cold {
		t.Fatal("a later unknown gap did not re-arm packFill")
	}
}

func TestSessionStateOutcomesAndFreezeFloor(t *testing.T) {
	s := newSessionStateStore()
	now := time.Unix(2_000, 0)
	s.noteHistoryRequest("s", now)

	// Zero usage before a cache ever exists must not churn uncached sessions.
	s.noteCacheOutcome("s", 0, 0)
	if got := s.noteHistoryRequest("s", now.Add(time.Minute)); got.cold {
		t.Fatal("never-cached session was marked dead")
	}

	s.noteCacheOutcome("s", 10, 0)
	if got := s.noteHistoryRequest("s", now.Add(3*time.Hour)); got.cold {
		t.Fatal("provider-confirmed live cache used the unknown fallback")
	}
	s.noteCacheOutcome("s", 0, 0)
	if got := s.noteHistoryRequest("s", now.Add(3*time.Hour+time.Minute)); !got.cold {
		t.Fatal("lost cache did not authorize packFill")
	}
	if got := s.noteHistoryRequest("s", now.Add(3*time.Hour+2*time.Minute)); !got.cold {
		t.Fatal("provider-confirmed missing cache should remain cold")
	}
	s.noteCacheOutcome("s", 10, 0)
	if got := s.noteHistoryRequest("s", now.Add(3*time.Hour+3*time.Minute)); got.cold {
		t.Fatal("new cache accounting did not restore warm state")
	}

	s.recordFreezeStep("s", 40)
	s.recordFreezeStep("s", 10)
	if got := s.noteHistoryRequest("s", now.Add(3*time.Hour+4*time.Minute)); got.minFreezeStep != 40 {
		t.Fatalf("minFreezeStep = %d, want 40", got.minFreezeStep)
	}
}

func TestSessionStateExplicitRejectionIsOneShot(t *testing.T) {
	s := newSessionStateStore()
	now := time.Unix(2_500, 0)
	s.noteHistoryRequest("s", now)
	s.noteCacheOutcome("s", 1, 0)
	s.markCacheDead("s")
	if got := s.noteHistoryRequest("s", now.Add(time.Minute)); !got.cold {
		t.Fatal("explicit rejection did not authorize pack-fill")
	}
	if got := s.noteHistoryRequest("s", now.Add(2*time.Minute)); got.cold {
		t.Fatal("explicit rejection signal was not consumed")
	}
}

func TestSessionStateLRUCapacity(t *testing.T) {
	s := newSessionStateStore()
	now := time.Unix(3_000, 0)
	for i := 0; i < sessionStateCapacity; i++ {
		s.noteHistoryRequest(fmt.Sprintf("s-%d", i), now)
	}
	// Refresh s-0, then force out s-1.
	s.noteHistoryRequest("s-0", now)
	s.noteHistoryRequest("new", now)
	if len(s.entries) != sessionStateCapacity {
		t.Fatalf("entries = %d, want %d", len(s.entries), sessionStateCapacity)
	}
	if s.entries["s-0"] == nil || s.entries["s-1"] != nil {
		t.Fatal("LRU did not retain the refreshed entry and evict the oldest")
	}
}

func TestSessionStateConcurrentAccess(t *testing.T) {
	s := newSessionStateStore()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("s-%d", i%4)
			for step := 1; step <= 100; step++ {
				s.noteHistoryRequest(key, time.Unix(int64(step), 0))
				s.recordFreezeStep(key, step)
				s.noteCacheOutcome(key, int64(step%2), 0)
			}
		}(i)
	}
	wg.Wait()
	for i := 0; i < 4; i++ {
		got := s.noteHistoryRequest(fmt.Sprintf("s-%d", i), time.Unix(200, 0))
		if got.minFreezeStep != 100 {
			t.Fatalf("session %d freeze floor = %d, want 100", i, got.minFreezeStep)
		}
	}
}

func TestSessionStateLastSeenDoesNotMoveBackward(t *testing.T) {
	s := newSessionStateStore()
	later := time.Unix(20_000, 0)
	s.noteHistoryRequest("s", later)
	s.noteHistoryRequest("s", later.Add(-time.Hour))

	s.mu.Lock()
	got := s.entries["s"].Value.(*sessionStateEntry).record.lastSeen
	s.mu.Unlock()
	if !got.Equal(later) {
		t.Fatalf("lastSeen = %v, want %v", got, later)
	}
}
