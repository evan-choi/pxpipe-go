package pxpipe

import (
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func budgetEntriesReference(all []string, counts map[string]int, collapse bool) []FactSheetEntry {
	candidates := all
	if collapse {
		ordered := all
		sort.SliceStable(ordered, func(i, j int) bool {
			li, lj := u16len(ordered[i]), u16len(ordered[j])
			if li != lj {
				return li > lj
			}
			return ordered[i] < ordered[j]
		})
		specific := ordered[:0]
		for _, token := range ordered {
			sub := false
			for _, container := range specific {
				if len(container) == len(token) {
					continue
				}
				if strings.Contains(container, token) {
					sub = true
					break
				}
			}
			if !sub {
				specific = append(specific, token)
			}
		}
		candidates = specific
	}
	type ranked struct {
		token string
		tier  int
	}
	rankedTokens := make([]ranked, len(candidates))
	for i, token := range candidates {
		rankedTokens[i] = ranked{token, priorityTier(token)}
	}
	sort.SliceStable(rankedTokens, func(i, j int) bool {
		if rankedTokens[i].tier != rankedTokens[j].tier {
			return rankedTokens[i].tier < rankedTokens[j].tier
		}
		li, lj := u16len(rankedTokens[i].token), u16len(rankedTokens[j].token)
		if li != lj {
			return li > lj
		}
		return rankedTokens[i].token < rankedTokens[j].token
	})
	var kept []FactSheetEntry
	urls := 0
	for _, rankedToken := range rankedTokens {
		if len(kept) >= FactSheetMaxTokens {
			break
		}
		if rankedToken.tier == 2 {
			urls++
			if urls > fsMaxURLs {
				continue
			}
		}
		count := counts[rankedToken.token]
		if count == 0 {
			count = 1
		}
		kept = append(kept, FactSheetEntry{Token: rankedToken.token, Count: count})
	}
	return kept
}

func TestBudgetEntriesMatchesReference(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	alphabet := []rune("abcXYZ019_-/.:@\x00한글😀")
	for iteration := 0; iteration < 200; iteration++ {
		count := rng.Intn(96)
		tokens := make([]string, 0, count)
		counts := make(map[string]int, count)
		for len(tokens) < count {
			var token string
			if len(tokens) > 0 && rng.Intn(4) == 0 {
				container := []rune(tokens[rng.Intn(len(tokens))])
				start := rng.Intn(len(container) + 1)
				token = string(container[start : start+rng.Intn(len(container)-start+1)])
			} else {
				chars := make([]rune, rng.Intn(24))
				for i := range chars {
					chars[i] = alphabet[rng.Intn(len(alphabet))]
				}
				token = string(chars)
			}
			if _, exists := counts[token]; exists {
				continue
			}
			tokens = append(tokens, token)
			counts[token] = rng.Intn(4)
		}
		for _, collapse := range []bool{false, true} {
			want := budgetEntriesReference(append([]string(nil), tokens...), counts, collapse)
			got := budgetEntries(append([]string(nil), tokens...), counts, collapse)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("iteration %d collapse=%v: got %#v, want %#v", iteration, collapse, got, want)
			}
		}
	}
}
