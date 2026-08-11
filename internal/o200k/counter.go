package o200k

import (
	"bufio"
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/base64"
	"fmt"
	"math"
	"sync"
	"unicode"
	"unicode/utf8"
)

//go:embed data/o200k_base.tiktoken.gz
var ranksGz []byte

var (
	ranksOnce sync.Once
	ranks     map[string]uint
	ranksErr  error
)

type bpePart struct {
	offset int
	rank   uint
}

type rankRecord struct {
	start int
	end   int
	rank  uint
}

func vocabulary() (map[string]uint, error) {
	ranksOnce.Do(func() {
		ranks, ranksErr = readVocabulary()
	})
	return ranks, ranksErr
}

func readVocabulary() (map[string]uint, error) {
	zr, err := gzip.NewReader(bytes.NewReader(ranksGz))
	if err != nil {
		return nil, err
	}
	defer zr.Close()

	decoded := make([]byte, 0, 2<<20)
	records := make([]rankRecord, 0, 200_000)
	scanner := bufio.NewScanner(zr)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		space := bytes.IndexByte(line, ' ')
		if space < 0 {
			return nil, fmt.Errorf("o200k: malformed rank line %q", line)
		}
		start := len(decoded)
		decoded = append(decoded, make([]byte, base64.StdEncoding.DecodedLen(space))...)
		n, err := base64.StdEncoding.Decode(decoded[start:], line[:space])
		if err != nil {
			return nil, fmt.Errorf("o200k: decode rank token: %w", err)
		}
		decoded = decoded[:start+n]
		rank, err := parseRank(line[space+1:])
		if err != nil {
			return nil, err
		}
		records = append(records, rankRecord{start: start, end: start + n, rank: rank})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("o200k: read ranks: %w", err)
	}
	tokens := string(decoded)
	vocab := make(map[string]uint, len(records))
	for _, record := range records {
		vocab[tokens[record.start:record.end]] = record.rank
	}
	return vocab, nil
}

func parseRank(text []byte) (uint, error) {
	if len(text) == 0 {
		return 0, fmt.Errorf("o200k: empty rank")
	}
	var rank uint
	for _, b := range text {
		if b < '0' || b > '9' {
			return 0, fmt.Errorf("o200k: malformed rank %q", text)
		}
		rank = rank*10 + uint(b-'0')
	}
	return rank, nil
}

func countTokensUncached(text string) (int, error) {
	text = normalizeText(text)
	vocab, err := vocabulary()
	if err != nil {
		return 0, err
	}
	count := 0
	var scratch []bpePart
	for start := 0; start < len(text); {
		end := nextPieceEnd(text, start)
		pieceCount, nextScratch := countPiece(text[start:end], vocab, scratch)
		count += pieceCount
		scratch = nextScratch
		start = end
	}
	return count, nil
}

func normalizeText(text string) string {
	if utf8.ValidString(text) {
		return text
	}
	return string([]rune(text))
}

func countPiece(piece string, vocab map[string]uint, scratch []bpePart) (int, []bpePart) {
	if _, ok := vocab[piece]; ok {
		return 1, scratch
	}
	n := len(piece) + 1
	if cap(scratch) < n {
		scratch = make([]bpePart, n)
	} else {
		scratch = scratch[:n]
	}
	for i := range scratch {
		scratch[i] = bpePart{offset: i, rank: math.MaxUint}
	}
	for i := 0; i < len(scratch)-2; i++ {
		scratch[i].rank = pairRank(piece, vocab, scratch, i, 0)
	}

	parts := scratch
	for {
		minRank := uint(math.MaxUint)
		minIndex := 0
		for i := range parts[:len(parts)-1] {
			if parts[i].rank < minRank {
				minRank = parts[i].rank
				minIndex = i
			}
		}
		if minRank == math.MaxUint {
			return len(parts) - 1, parts
		}
		parts[minIndex].rank = pairRank(piece, vocab, parts, minIndex, 1)
		if minIndex > 0 {
			parts[minIndex-1].rank = pairRank(piece, vocab, parts, minIndex-1, 1)
		}
		copy(parts[minIndex+1:], parts[minIndex+2:])
		parts = parts[:len(parts)-1]
	}
}

func pairRank(piece string, vocab map[string]uint, parts []bpePart, index, skip int) uint {
	if index+skip+2 < len(parts) {
		if rank, ok := vocab[piece[parts[index].offset:parts[index+skip+2].offset]]; ok {
			return rank
		}
	}
	return math.MaxUint
}

func nextPieceEnd(text string, start int) int {
	if end, ok := wordPieceEnd(text, start); ok {
		return end
	}
	if end, ok := numberPieceEnd(text, start); ok {
		return end
	}
	if end, ok := symbolPieceEnd(text, start); ok {
		return end
	}
	if end, ok := newlinePieceEnd(text, start); ok {
		return end
	}
	if end, ok := whitespacePieceEnd(text, start); ok {
		return end
	}
	_, size := utf8.DecodeRuneInString(text[start:])
	return start + size
}

func wordPieceEnd(text string, start int) (int, bool) {
	r, size := utf8.DecodeRuneInString(text[start:])
	prefixEnd := start
	if r != '\r' && r != '\n' && !isLetterOrNumber(r) {
		prefixEnd += size
	}
	if prefixEnd != start {
		if end, ok := lowerWordEnd(text, prefixEnd); ok {
			return contractionEnd(text, end), true
		}
	}
	if end, ok := lowerWordEnd(text, start); ok {
		return contractionEnd(text, end), true
	}
	if prefixEnd != start {
		if end, ok := upperWordEnd(text, prefixEnd); ok {
			return contractionEnd(text, end), true
		}
	}
	if end, ok := upperWordEnd(text, start); ok {
		return contractionEnd(text, end), true
	}
	return 0, false
}

func lowerWordEnd(text string, start int) (int, bool) {
	if start < len(text) && text[start] >= 'a' && text[start] <= 'z' {
		pos := start
		for pos < len(text) {
			if b := text[pos]; b < utf8.RuneSelf {
				if b < 'a' || b > 'z' {
					break
				}
				pos++
				continue
			}
			r, size := utf8.DecodeRuneInString(text[pos:])
			if !isLowerWordRune(r) {
				break
			}
			pos += size
		}
		return pos, true
	}
	pos := start
	lastLower := -1
	for pos < len(text) {
		r, size := utf8.DecodeRuneInString(text[pos:])
		if !isUpperWordRune(r) {
			break
		}
		if isLowerWordRune(r) {
			lastLower = pos
		}
		pos += size
	}
	if pos < len(text) {
		r, _ := utf8.DecodeRuneInString(text[pos:])
		if isLowerWordRune(r) {
			lastLower = pos
		}
	}
	if lastLower < start {
		return 0, false
	}
	pos = lastLower
	for pos < len(text) {
		r, size := utf8.DecodeRuneInString(text[pos:])
		if !isLowerWordRune(r) {
			break
		}
		pos += size
	}
	return pos, true
}

func upperWordEnd(text string, start int) (int, bool) {
	pos := start
	for pos < len(text) {
		r, size := utf8.DecodeRuneInString(text[pos:])
		if !isUpperWordRune(r) {
			break
		}
		pos += size
	}
	if pos == start {
		return 0, false
	}
	for pos < len(text) {
		r, size := utf8.DecodeRuneInString(text[pos:])
		if !isLowerWordRune(r) {
			break
		}
		pos += size
	}
	return pos, true
}

func contractionEnd(text string, start int) int {
	if start+1 >= len(text) || text[start] != '\'' {
		return start
	}
	switch text[start+1] {
	case 's', 'S', 't', 'T', 'm', 'M', 'd', 'D':
		return start + 2
	case 'r', 'R':
		if start+2 < len(text) && text[start+2]|0x20 == 'e' {
			return start + 3
		}
	case 'v', 'V':
		if start+2 < len(text) && text[start+2]|0x20 == 'e' {
			return start + 3
		}
	case 'l', 'L':
		if start+2 < len(text) && text[start+2]|0x20 == 'l' {
			return start + 3
		}
	case 0xc5:
		if start+2 < len(text) && text[start+2] == 0xbf {
			return start + 3
		}
	}
	return start
}

func numberPieceEnd(text string, start int) (int, bool) {
	pos := start
	for range 3 {
		if pos == len(text) {
			break
		}
		r, size := utf8.DecodeRuneInString(text[pos:])
		if !isNumber(r) {
			break
		}
		pos += size
	}
	return pos, pos != start
}

func symbolPieceEnd(text string, start int) (int, bool) {
	pos := start
	if text[pos] == ' ' {
		pos++
	}
	symbolStart := pos
	for pos < len(text) {
		r, size := utf8.DecodeRuneInString(text[pos:])
		if isSpace(r) || isLetterOrNumber(r) {
			break
		}
		pos += size
	}
	if pos == symbolStart {
		return 0, false
	}
	for pos < len(text) && (text[pos] == '\r' || text[pos] == '\n' || text[pos] == '/') {
		pos++
	}
	return pos, true
}

func newlinePieceEnd(text string, start int) (int, bool) {
	pos := start
	for pos < len(text) {
		r, size := utf8.DecodeRuneInString(text[pos:])
		if !isSpace(r) {
			return 0, false
		}
		if r == '\r' || r == '\n' {
			for pos < len(text) && (text[pos] == '\r' || text[pos] == '\n') {
				pos++
			}
			return pos, true
		}
		pos += size
	}
	return 0, false
}

func whitespacePieceEnd(text string, start int) (int, bool) {
	pos := start
	last := start
	count := 0
	for pos < len(text) {
		r, size := utf8.DecodeRuneInString(text[pos:])
		if !isSpace(r) {
			break
		}
		last = pos
		pos += size
		count++
	}
	if count == 0 {
		return 0, false
	}
	if pos < len(text) && count > 1 {
		return last, true
	}
	return pos, true
}

func isLetterOrNumber(r rune) bool {
	if r < utf8.RuneSelf {
		return r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
	}
	return unicode.IsLetter(r) || unicode.IsNumber(r)
}

func isNumber(r rune) bool {
	if r < utf8.RuneSelf {
		return r >= '0' && r <= '9'
	}
	return unicode.IsNumber(r)
}

func isUpperWordRune(r rune) bool {
	if r < utf8.RuneSelf {
		return r >= 'A' && r <= 'Z'
	}
	return unicode.In(r, unicode.Lu, unicode.Lt, unicode.Lm, unicode.Lo, unicode.Mn, unicode.Mc, unicode.Me)
}

func isLowerWordRune(r rune) bool {
	if r < utf8.RuneSelf {
		return r >= 'a' && r <= 'z'
	}
	return unicode.In(r, unicode.Ll, unicode.Lm, unicode.Lo, unicode.Mn, unicode.Mc, unicode.Me)
}

func isSpace(r rune) bool {
	if r < utf8.RuneSelf {
		return r == ' ' || r >= '\t' && r <= '\r'
	}
	return unicode.IsSpace(r)
}
