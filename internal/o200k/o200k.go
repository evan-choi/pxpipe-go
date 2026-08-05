// Package o200k provides an offline o200k_base token counter.
//
// The BPE ranks file is embedded (gzipped) so counting never touches the
// network — the GPT profitability gate depends on exact o200k counts, making
// the tokenizer a functional dependency, not telemetry.
package o200k

import (
	"sync"

	"github.com/tiktoken-go/tokenizer"
)

var (
	once sync.Once
	enc  tokenizer.Codec
	err  error
)

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
	e, err := encoding()
	if err != nil {
		return 0
	}
	n, err := e.Count(text)
	if err != nil {
		return 0
	}
	return n
}
