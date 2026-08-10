package pxpipe

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"
)

// u16len counts UTF-16 code units, matching TS String.prototype.length —
// every char-count gate and info field in the reference is UTF-16 based.
func u16len(s string) int {
	const highBits = uint64(0x8080808080808080)
	n := 0
	for len(s) >= 8 {
		if *(*uint64)(unsafe.Pointer(unsafe.StringData(s)))&highBits == 0 {
			n += 8
			s = s[8:]
			continue
		}
		if s[0] < utf8.RuneSelf {
			n++
			s = s[1:]
			continue
		}
		r, size := utf8.DecodeRuneInString(s)
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
		s = s[size:]
	}
	for _, r := range s {
		if r >= 0x10000 {
			n++
		}
		n++
	}
	return n
}

func isASCII(s string) bool {
	const highBits = uint64(0x8080808080808080)
	for len(s) >= 8 {
		if *(*uint64)(unsafe.Pointer(unsafe.StringData(s)))&highBits != 0 {
			return false
		}
		s = s[8:]
	}
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
	if start < 0 {
		start = 0
	}
	if start >= end {
		return ""
	}

	startByte, endByte := -1, -1
	startSplit, endSplit := false, false
	units := 0
	for i := 0; i < len(s); {
		if len(s)-i >= 8 && *(*uint64)(unsafe.Pointer(unsafe.StringData(s[i:])))&uint64(0x8080808080808080) == 0 {
			nextUnits := units + 8
			if startByte < 0 && start >= units && start <= nextUnits {
				startByte = i + start - units
			}
			if endByte < 0 && end >= units && end <= nextUnits {
				endByte = i + end - units
			}
			units = nextUnits
			i += 8
			if startByte >= 0 && endByte >= 0 {
				break
			}
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		runeUnits := 1
		if r >= 0x10000 {
			runeUnits = 2
		}
		next := i + size

		if startByte < 0 {
			switch {
			case start == units:
				startByte = i
			case runeUnits == 2 && start == units+1:
				startByte = next
				startSplit = true
			}
		}
		if endByte < 0 {
			switch {
			case end == units:
				endByte = i
			case runeUnits == 2 && end == units+1:
				endByte = i
				endSplit = true
			}
		}
		if r == utf8.RuneError && size == 1 && start < units+1 && end > units {
			return u16SliceFallback(s, start, end)
		}

		units += runeUnits
		if endByte < 0 && end == units {
			endByte = next
		}
		if startByte >= 0 && endByte >= 0 {
			break
		}
		i = next
	}
	if startByte < 0 {
		if start != units {
			return ""
		}
		startByte = len(s)
	}
	if endByte < 0 {
		endByte = len(s)
	}
	if !startSplit && !endSplit {
		return s[startByte:endByte]
	}

	var b strings.Builder
	b.Grow(endByte - startByte + 2*utf8.RuneLen(utf8.RuneError))
	if startSplit {
		b.WriteRune(utf8.RuneError)
	}
	b.WriteString(s[startByte:endByte])
	if endSplit {
		b.WriteRune(utf8.RuneError)
	}
	return b.String()
}

func u16SliceFallback(s string, start, end int) string {
	units := utf16.Encode([]rune(s))
	if end > len(units) {
		end = len(units)
	}
	if start >= end {
		return ""
	}
	return string(utf16.Decode(units[start:end]))
}

func sha8(text string) string {
	// Sum256 only reads its input; avoid copying large rendered contexts.
	sum := sha256.Sum256(unsafe.Slice(unsafe.StringData(text), len(text)))
	return hex.EncodeToString(sum[:4])
}
