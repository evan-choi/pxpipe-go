// Package o200k provides an offline o200k_base token counter.
//
// The BPE ranks file is embedded (gzipped) so counting never touches the
// network — the GPT profitability gate depends on exact o200k counts, making
// the tokenizer a functional dependency, not telemetry.
package o200k

import (
	"bufio"
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/pkoukk/tiktoken-go"
)

//go:embed data/o200k_base.tiktoken.gz
var ranksGz []byte

type embeddedLoader struct{}

func (embeddedLoader) LoadTiktokenBpe(_ string) (map[string]int, error) {
	zr, err := gzip.NewReader(bytes.NewReader(ranksGz))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	ranks := make(map[string]int, 200_000)
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 1<<16), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		tok, rankStr, ok := strings.Cut(line, " ")
		if !ok {
			return nil, fmt.Errorf("o200k: malformed rank line %q", line)
		}
		raw, err := base64.StdEncoding.DecodeString(tok)
		if err != nil {
			return nil, err
		}
		rank, err := strconv.Atoi(rankStr)
		if err != nil {
			return nil, err
		}
		ranks[string(raw)] = rank
	}
	return ranks, sc.Err()
}

var (
	once sync.Once
	enc  *tiktoken.Tiktoken
	err  error
)

func encoding() (*tiktoken.Tiktoken, error) {
	once.Do(func() {
		tiktoken.SetBpeLoader(embeddedLoader{})
		enc, err = tiktoken.GetEncoding("o200k_base")
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
	return len(e.EncodeOrdinary(text))
}
