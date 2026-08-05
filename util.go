package pxpipe

import (
	"crypto/sha256"
	"encoding/hex"
	"unicode/utf16"
	"unicode/utf8"
)

// u16len counts UTF-16 code units, matching TS String.prototype.length —
// every char-count gate and info field in the reference is UTF-16 based.
func u16len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// u16Slice mirrors TS String.prototype.slice(start, end) in UTF-16 units.
// A slice boundary inside a surrogate pair keeps the unit via utf16 round-trip,
// matching JS (which would keep a lone surrogate).
func u16Slice(s string, start, end int) string {
	if isASCII(s) {
		if start < 0 {
			start = 0
		}
		if end > len(s) {
			end = len(s)
		}
		if start >= end {
			return ""
		}
		return s[start:end]
	}
	units := utf16.Encode([]rune(s))
	if start < 0 {
		start = 0
	}
	if end > len(units) {
		end = len(units)
	}
	if start >= end {
		return ""
	}
	return string(utf16.Decode(units[start:end]))
}

func sha8(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:4])
}
