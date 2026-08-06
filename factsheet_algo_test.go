package pxpipe

import (
	"math/rand"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var shapeNumReference = regexp.MustCompile(`^\d[\d,_]*$|^\d+\.\d+$`)

func priorityTierReference(tok string) int {
	switch {
	case shapeAssignment.MatchString(tok),
		shapeHex.MatchString(tok) && shapeHexDigit.MatchString(tok),
		shapeUUID.MatchString(tok),
		shapeEmail.MatchString(tok),
		shapeIBAN.MatchString(tok),
		shapeCurrency.MatchString(tok),
		shapeConst.MatchString(tok),
		shapeTicket.MatchString(tok) && shapeTicketDig.MatchString(tok),
		shapeFlag.MatchString(tok),
		shapeNumReference.MatchString(tok),
		shapeCamel.MatchString(tok) && u16len(tok) >= 8:
		return 0
	case shapeURL.MatchString(tok):
		return 2
	}
	return 1
}

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
		rankedTokens[i] = ranked{token, priorityTierReference(token)}
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

func TestPriorityTierMatchesReference(t *testing.T) {
	tokens := []string{
		"ABC=1", "abc1234", "123e4567-e89b-12d3-a456-426614174000",
		"dev@example.com", "GB82WEST12345698765432", "$1,234.56",
		"CONST_ID", "CVE-2026-1234", "--verbose", "1234", "CamelCaseName",
		"https://example.com/path", "plain", "한글😀", "0", "1,", "1_",
		"1,2", "1__2", "1.2", "1,.2", "1_2.3", "1.", "1.2.3", "1a", "١",
	}
	rng := rand.New(rand.NewSource(2))
	alphabet := []rune("abcXYZ019_-/.:@=$\x00한글😀")
	for range 1000 {
		chars := make([]rune, rng.Intn(64))
		for i := range chars {
			chars[i] = alphabet[rng.Intn(len(alphabet))]
		}
		tokens = append(tokens, string(chars))
	}
	numericAlphabet := "01,_.a"
	for range 10000 {
		chars := make([]byte, rng.Intn(32))
		for i := range chars {
			chars[i] = numericAlphabet[rng.Intn(len(numericAlphabet))]
		}
		token := string(chars)
		if got, want := isFactSheetNumber(token), shapeNumReference.MatchString(token); got != want {
			t.Fatalf("isFactSheetNumber(%q) = %v, want %v", token, got, want)
		}
		tokens = append(tokens, token)
	}
	for _, token := range tokens {
		if got, want := priorityTier(token), priorityTierReference(token); got != want {
			t.Fatalf("priorityTier(%q) = %d, want %d", token, got, want)
		}
	}
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
