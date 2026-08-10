// Package o200k provides an offline o200k_base token counter.
//
// The BPE ranks file is embedded (gzipped) so counting never touches the
// network — the GPT profitability gate depends on exact o200k counts, making
// the tokenizer a functional dependency, not telemetry.
package o200k

import (
	"crypto/sha256"
	"encoding/binary"
	"strings"
	"sync/atomic"
	"unicode"
	"unicode/utf8"
	"unsafe"
)

const (
	tokenCountCacheSlots         = 1 << 12
	exactTokenCountCacheSlots    = 1 << 7
	exactTokenCountCacheMaxBytes = 128 << 10
)

type exactTokenCountCacheEntry struct {
	fingerprint uint64
	text        string
	count       int
}

type tokenCountCacheEntry struct {
	key   [sha256.Size]byte
	count int
}

type digitTokenCountCacheEntry struct {
	fingerprint uint64
	key         [sha256.Size]byte
	count       int
}

// The direct-mapped cache is bounded to roughly 224 KiB at capacity and never
// retains prompt text. A slot collision is a miss and replacement; the full
// digest comparison prevents returning another prompt's count. Hits are
// lock-free and allocation-free, which is the steady state for stable agent
// instructions and tool schemas shared by requests in one pod.
var tokenCountCache [tokenCountCacheSlots]atomic.Pointer[tokenCountCacheEntry]

// Repeated bounded inputs skip the cryptographic digest after a verified
// digest-cache hit. Exact string comparison makes sampled collisions misses;
// non-replacing slots cap retained text at 16 MiB and avoid churn.
var exactTokenCountCache [exactTokenCountCacheSlots]atomic.Pointer[exactTokenCountCacheEntry]

// o200k splits ASCII digit runs into 1-3 digit pieces, and every such piece is
// one token. A sampled fingerprint only admits entries; the full normalized
// digest still verifies every hit, so fingerprint collisions cannot alter counts.
var digitTokenCountCache [tokenCountCacheSlots]atomic.Pointer[digitTokenCountCacheEntry]
var digitTokenCountCandidates [tokenCountCacheSlots]atomic.Uint64

func tokenCountFingerprint(text string) uint64 {
	const (
		offset = uint64(14695981039346656037)
		prime  = uint64(1099511628211)
		sample = 128
	)
	h := offset ^ uint64(len(text))
	hash := func(s string) {
		for i := 0; i < len(s); i++ {
			h = (h ^ uint64(s[i])) * prime
		}
	}
	if len(text) <= 3*sample {
		hash(text)
		return h
	}
	hash(text[:sample])
	mid := len(text)/2 - sample/2
	hash(text[mid : mid+sample])
	hash(text[len(text)-sample:])
	return h
}

func canNormalizeDigits(text string) bool {
	hasDigits := false
	for i := 0; i < len(text); {
		b := text[i]
		if b < utf8.RuneSelf {
			if b >= '0' && b <= '9' {
				hasDigits = true
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(text[i:])
		if unicode.IsNumber(r) {
			return false
		}
		i += size
	}
	return hasDigits
}

func digitNormalizedFingerprint(text string) uint64 {
	const (
		offset = uint64(14695981039346656037)
		prime  = uint64(1099511628211)
		sample = 128
	)
	h := offset ^ uint64(len(text))
	hash := func(s string) {
		for i := 0; i < len(s); i++ {
			b := s[i]
			if b >= '0' && b <= '9' {
				b = '0'
			}
			h = (h ^ uint64(b)) * prime
		}
	}
	if len(text) <= 3*sample {
		hash(text)
		return h
	}
	hash(text[:sample])
	mid := len(text)/2 - sample/2
	hash(text[mid : mid+sample])
	hash(text[len(text)-sample:])
	return h
}

func digitNormalizedKey(text string) [sha256.Size]byte {
	normalized := []byte(text)
	for i, b := range normalized {
		if b >= '0' && b <= '9' {
			normalized[i] = '0'
		}
	}
	return sha256.Sum256(normalized)
}

// CountTokens returns the o200k_base token count of text, treating special
// tokens as plain text (mirrors gpt-tokenizer's countTokens). Returns 0 for
// empty input or on encoder failure (mirrors the TS try/catch → 0).
func CountTokens(text string) int {
	if text == "" {
		return 0
	}
	var exactFingerprint uint64
	var exactSlot *atomic.Pointer[exactTokenCountCacheEntry]
	if len(text) <= exactTokenCountCacheMaxBytes {
		exactFingerprint = tokenCountFingerprint(text)
		exactSlot = &exactTokenCountCache[exactFingerprint&(exactTokenCountCacheSlots-1)]
		if cached := exactSlot.Load(); cached != nil && cached.fingerprint == exactFingerprint && cached.text == text {
			return cached.count
		}
	}
	key := sha256.Sum256(unsafe.Slice(unsafe.StringData(text), len(text)))
	slot := &tokenCountCache[binary.LittleEndian.Uint64(key[:])&(tokenCountCacheSlots-1)]
	if cached := slot.Load(); cached != nil && cached.key == key {
		if exactSlot != nil && exactSlot.Load() == nil {
			exactSlot.CompareAndSwap(nil, &exactTokenCountCacheEntry{
				fingerprint: exactFingerprint,
				text:        strings.Clone(text),
				count:       cached.count,
			})
		}
		return cached.count
	}
	var digitKey [sha256.Size]byte
	var digitSlot *atomic.Pointer[digitTokenCountCacheEntry]
	digitFingerprint, admitDigitKey := uint64(0), false
	if canNormalizeDigits(text) {
		digitFingerprint = digitNormalizedFingerprint(text)
		digitSlot = &digitTokenCountCache[digitFingerprint&(tokenCountCacheSlots-1)]
		if cached := digitSlot.Load(); cached != nil && cached.fingerprint == digitFingerprint {
			digitKey = digitNormalizedKey(text)
			if cached.key == digitKey {
				slot.Store(&tokenCountCacheEntry{key: key, count: cached.count})
				return cached.count
			}
			admitDigitKey = true
		} else {
			candidate := &digitTokenCountCandidates[digitFingerprint&(tokenCountCacheSlots-1)]
			admitDigitKey = candidate.Swap(digitFingerprint) == digitFingerprint
		}
	}
	n, err := countTokensUncached(text)
	if err != nil {
		return 0
	}
	slot.Store(&tokenCountCacheEntry{key: key, count: n})
	if admitDigitKey {
		if digitKey == ([sha256.Size]byte{}) {
			digitKey = digitNormalizedKey(text)
		}
		digitSlot.Store(&digitTokenCountCacheEntry{fingerprint: digitFingerprint, key: digitKey, count: n})
	}
	return n
}
