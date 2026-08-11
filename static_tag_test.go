package pxpipe

import (
	"strconv"
	"strings"
	"sync"
	"testing"
)

func resetTagObservations() {
	tagObsMu.Lock()
	clear(tagObsMap)
	tagObsList.Init()
	tagObsMu.Unlock()
}

func TestObserveStaticTagChurn(t *testing.T) {
	t.Cleanup(resetTagObservations)

	t.Run("semantics", func(t *testing.T) {
		resetTagObservations()
		tags := []string{"skill", "rules"}
		contents := map[string]string{"skill": "stable skill", "rules": "stable rules"}
		if churned := observeStaticTagChurn("session", tags, contents); len(churned) != 0 {
			t.Fatalf("first observation reported churn: %v", churned)
		}
		if churned := observeStaticTagChurn("session", tags, contents); len(churned) != 0 {
			t.Fatalf("unchanged observation reported churn: %v", churned)
		}
		contents["skill"] = "changed skill"
		contents["rules"] = "changed rules"
		churned := observeStaticTagChurn("session", tags, contents)
		if len(churned) != len(tags) || churned[0] != tags[0] || churned[1] != tags[1] {
			t.Fatalf("changed observation = %v, want %v", churned, tags)
		}
		if churned := observeStaticTagChurn("session", tags, contents); len(churned) != 0 {
			t.Fatalf("updated observation reported churn: %v", churned)
		}
	})

	t.Run("lru eviction", func(t *testing.T) {
		resetTagObservations()
		tags := []string{"skill"}
		contents := map[string]string{"skill": "stable"}
		for i := range tagObservationsMax {
			observeStaticTagChurn("session-"+strconv.Itoa(i), tags, contents)
		}
		observeStaticTagChurn("session-0", tags, contents)
		observeStaticTagChurn("overflow", tags, contents)

		tagObsMu.Lock()
		entries, listEntries := len(tagObsMap), tagObsList.Len()
		_, keptRecent := tagObsMap[tagObsKey{session: "session-0", tag: "skill"}]
		_, keptOldest := tagObsMap[tagObsKey{session: "session-1", tag: "skill"}]
		tagObsMu.Unlock()
		if entries != tagObservationsMax || listEntries != entries {
			t.Fatalf("cache size = map %d/list %d, want %d", entries, listEntries, tagObservationsMax)
		}
		if !keptRecent || keptOldest {
			t.Fatalf("LRU eviction = recent %v/oldest %v, want true/false", keptRecent, keptOldest)
		}
	})

	t.Run("large batch", func(t *testing.T) {
		resetTagObservations()
		tags := make([]string, 33)
		contents := make(map[string]string, len(tags))
		for i := range tags {
			tags[i] = "tag-" + strconv.Itoa(i)
			contents[tags[i]] = "stable"
		}
		if churned := observeStaticTagChurn("session", tags, contents); len(churned) != 0 {
			t.Fatalf("first observation reported churn: %v", churned)
		}
		contents[tags[len(tags)-1]] = "changed"
		churned := observeStaticTagChurn("session", tags, contents)
		if len(churned) != 1 || churned[0] != tags[len(tags)-1] {
			t.Fatalf("large batch observation = %v, want [%s]", churned, tags[len(tags)-1])
		}
	})

	t.Run("concurrent hit", func(t *testing.T) {
		resetTagObservations()
		tags := []string{"skill"}
		contents := map[string]string{"skill": strings.Repeat("stable", 64)}
		var wg sync.WaitGroup
		errs := make(chan []string, 8)
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range 1_000 {
					if churned := observeStaticTagChurn("session", tags, contents); len(churned) != 0 {
						errs <- churned
						return
					}
				}
			}()
		}
		wg.Wait()
		close(errs)
		if churned := <-errs; len(churned) != 0 {
			t.Fatalf("concurrent unchanged observation reported churn: %v", churned)
		}
	})
}

func TestTotalTokensIsDynamic(t *testing.T) {
	got := splitStaticDynamic("<rules>stable</rules><total_tokens>123</total_tokens>")
	if got.staticText != "<rules>stable</rules>" || got.dynamicText != "<total_tokens>123</total_tokens>" {
		t.Fatalf("splitStaticDynamic() = static %q, dynamic %q", got.staticText, got.dynamicText)
	}
	if len(got.unknownTags) != 0 {
		t.Fatalf("total_tokens reported as unknown static tag: %v", got.unknownTags)
	}
}

func TestSplitStaticDynamicStaticOnly(t *testing.T) {
	const text = "<rules>stable</rules>\nplain text"
	got := splitStaticDynamic(text)
	if got.staticText != text || got.dynamicText != "" || got.blockCount != 0 {
		t.Fatalf("splitStaticDynamic() = static %q, dynamic %q, blocks %d", got.staticText, got.dynamicText, got.blockCount)
	}
}

func TestHasStaticSystemTextMatchesSplit(t *testing.T) {
	for _, text := range []string{
		"",
		" \n\t",
		"<env>dynamic</env>",
		" \n<total_tokens>123</total_tokens>\n ",
		"stable",
		"<rules>stable</rules><env>dynamic</env>",
		"<env>dynamic</env>tail",
		"<env>missing closer",
	} {
		want := splitStaticDynamic(text).staticText != ""
		if got := hasStaticSystemText(text); got != want {
			t.Fatalf("hasStaticSystemText(%q) = %v, want %v", text, got, want)
		}
	}
}

func BenchmarkObserveStaticTagChurn(b *testing.B) {
	tags := []string{"skill"}
	contents := map[string]string{"skill": strings.Repeat("stable static tag content ", 64)}

	b.Run("WarmHit", func(b *testing.B) {
		resetTagObservations()
		observeStaticTagChurn("session", tags, contents)
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			if churned := observeStaticTagChurn("session", tags, contents); len(churned) != 0 {
				b.Fatal("unchanged tag reported as churning")
			}
		}
	})

	b.Run("WarmHitParallel", func(b *testing.B) {
		resetTagObservations()
		observeStaticTagChurn("session", tags, contents)
		b.ReportAllocs()
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				if churned := observeStaticTagChurn("session", tags, contents); len(churned) != 0 {
					b.Error("unchanged tag reported as churning")
					return
				}
			}
		})
	})

	b.Run("UniqueMiss", func(b *testing.B) {
		sessions := make([]string, tagObservationsMax*2)
		for i := range sessions {
			sessions[i] = "session-" + strconv.Itoa(i)
		}
		resetTagObservations()
		b.ReportAllocs()
		b.ResetTimer()
		for i := range b.N {
			if churned := observeStaticTagChurn(sessions[i%len(sessions)], tags, contents); len(churned) != 0 {
				b.Fatal("new tag reported as churning")
			}
		}
	})
}
