// Package o200k provides an offline o200k_base token counter.
//
// The BPE ranks file is embedded (gzipped) so counting never touches the
// network — the GPT profitability gate depends on exact o200k counts, making
// the tokenizer a functional dependency, not telemetry.
package o200k

import (
	"crypto/sha256"
	"encoding/binary"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/tiktoken-go/tokenizer"
)

var (
	once sync.Once
	enc  tokenizer.Codec
	err  error
)

const tokenCountCacheSlots = 1 << 12

type tokenCountCacheEntry struct {
	key   [sha256.Size]byte
	count int
}

// The direct-mapped cache is bounded to roughly 224 KiB at capacity and never
// retains prompt text. A slot collision is a miss and replacement; the full
// digest comparison prevents returning another prompt's count. Hits are
// lock-free and allocation-free, which is the steady state for stable agent
// instructions and tool schemas shared by requests in one pod.
var tokenCountCache [tokenCountCacheSlots]atomic.Pointer[tokenCountCacheEntry]

func encoding() (tokenizer.Codec, error) {
	once.Do(func() {
		enc, err = tokenizer.Get(tokenizer.O200kBase)
	})
	return enc, err
}

// CountTokens returns the o200k_base token count of text, treating special
// tokens as plain text (mirrors gpt-tokenizer's countTokens). Returns 0 for
// empty input or on encoder failure (mirrors the TS try/catch → 0).
func CountTokens(text string) int {
	if text == "" {
		return 0
	}
	key := sha256.Sum256(unsafe.Slice(unsafe.StringData(text), len(text)))
	slot := &tokenCountCache[binary.LittleEndian.Uint64(key[:])&(tokenCountCacheSlots-1)]
	if cached := slot.Load(); cached != nil && cached.key == key {
		return cached.count
	}
	e, err := encoding()
	if err != nil {
		return 0
	}
	n, err := e.Count(text)
	if err != nil {
		return 0
	}
	slot.Store(&tokenCountCacheEntry{key: key, count: n})
	return n
}
