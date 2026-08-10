package pxpipe

import (
	"hash/maphash"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Fact-sheet extraction: precision-critical tokens (paths, URLs, SHAs, version
// numbers, CONST_IDS…) are pulled out of imaged content so they ride next to
// the image as plain text. Deterministic by construction (fixed pattern order,
// total-order sorts) so the emitted text is byte-stable across turns.

type fsFeature uint16

const (
	fsHasEqual fsFeature = 1 << iota
	fsHasAt
	fsHasDot
	fsHasSlash
	fsHasDash
	fsHasUnderscore
	fsHasDigit
	fsHasUpper
	fsHasLower
	fsHasColon
)

type fsPattern struct {
	re   *regexp.Regexp
	scan func(string, int) (int, int)
	// verify emulates a JS lookahead at the match start: it must match the
	// chunk remainder beginning at the token's start offset.
	verify   *regexp.Regexp
	required fsFeature
}

var fsPatterns = []fsPattern{
	{re: regexp.MustCompile(`\b[A-Z][A-Z0-9_]{2,}=[^\s)"'<>]+`), required: fsHasEqual | fsHasUpper},
	{scan: nextFactSheetAssignment, required: fsHasEqual},
	{re: regexp.MustCompile(`\bhttps?://[^\s)"'<>]+`), required: fsHasColon | fsHasSlash | fsHasLower},
	{re: regexp.MustCompile("\\b[A-Za-z0-9.!#$%&'*+/=?^_`{|}~-]+@[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?(?:\\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)+\\b"), required: fsHasAt | fsHasDot},
	{re: regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`), required: fsHasDash},
	{scan: nextFactSheetIBAN, required: fsHasUpper | fsHasDigit},
	{scan: nextFactSheetCurrency, required: fsHasDigit},
	{re: regexp.MustCompile(`(?:[\w@~+-]+)?(?:/[\w.@+-]+)+\.[A-Za-z]\w{0,8}\b`), required: fsHasSlash | fsHasDot},
	{re: regexp.MustCompile(`/[\w.@+-]+(?:/[\w.@+-]+)+/?`), required: fsHasSlash},
	// git sha / long hex: JS `(?=[0-9a-f]*\d)` emulated via verify.
	{scan: nextFactSheetHex, required: fsHasDigit},
	{scan: nextFactSheetVersion, required: fsHasDigit | fsHasDot},
	{scan: nextFactSheetFlag, required: fsHasDash},
	{scan: nextFactSheetLargeNumber, required: fsHasDigit},
	{scan: nextFactSheetDecimal, required: fsHasDigit | fsHasDot},
	{re: regexp.MustCompile(`\b[A-Z][A-Z0-9]{2,}(?:_[A-Z0-9]+)+\b`), required: fsHasUpper | fsHasUnderscore},
	{re: regexp.MustCompile(`\b(?:[a-z]+|[A-Z][a-z0-9]+)(?:[A-Z][a-z0-9]*)+\b`), required: fsHasUpper | fsHasLower},
	// ticket/advisory codes: JS `(?=[A-Z0-9-]{0,119}\d)` emulated via verify.
	{scan: nextFactSheetTicket, required: fsHasUpper | fsHasDash | fsHasDigit},
}

func isFactSheetASCIIAlpha(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z'
}

func isFactSheetASCIIWord(c byte) bool {
	return isFactSheetASCIIAlpha(c) || c >= '0' && c <= '9' || c == '_'
}

func isFactSheetASCIIUpper(c byte) bool {
	return c >= 'A' && c <= 'Z'
}

func isFactSheetASCIIDigit(c byte) bool {
	return c >= '0' && c <= '9'
}

func isFactSheetASCIIUpperDigit(c byte) bool {
	return isFactSheetASCIIUpper(c) || isFactSheetASCIIDigit(c)
}

func isFactSheetWordBoundary(s string, i int) bool {
	left := i > 0 && isFactSheetASCIIWord(s[i-1])
	right := i < len(s) && isFactSheetASCIIWord(s[i])
	return left != right
}

func nextFactSheetIBAN(s string, from int) (int, int) {
	for i := from; i+12 <= len(s); i++ {
		if !isFactSheetASCIIUpper(s[i]) || i > 0 && isFactSheetASCIIWord(s[i-1]) ||
			!isFactSheetASCIIUpper(s[i+1]) || !isFactSheetASCIIDigit(s[i+2]) || !isFactSheetASCIIDigit(s[i+3]) {
			continue
		}
		end := i + 4
		for end < len(s) && end-i < 34 && isFactSheetASCIIUpperDigit(s[end]) {
			end++
		}
		if end-i >= 12 && isFactSheetWordBoundary(s, end) {
			return i, end
		}
	}
	return -1, -1
}

func factSheetVersionEnd(s string, end int) int {
	if end < len(s) && (s[end] == '-' || s[end] == '+') && end+1 < len(s) &&
		(isFactSheetASCIIWord(s[end+1]) || s[end+1] == '.') {
		last := end + 2
		for last < len(s) && (isFactSheetASCIIWord(s[last]) || s[last] == '.') {
			last++
		}
		for last > end+1 {
			if isFactSheetWordBoundary(s, last) {
				return last
			}
			last--
		}
	}
	if isFactSheetWordBoundary(s, end) {
		return end
	}
	return -1
}

func nextFactSheetVersion(s string, from int) (int, int) {
	for i := from; i < len(s); i++ {
		if i > 0 && isFactSheetASCIIWord(s[i-1]) {
			continue
		}
		j := i
		if s[j] == 'v' {
			j++
		}
		if j == len(s) || !isFactSheetASCIIDigit(s[j]) {
			continue
		}
		for j < len(s) && isFactSheetASCIIDigit(s[j]) {
			j++
		}
		if j+1 >= len(s) || s[j] != '.' || !isFactSheetASCIIDigit(s[j+1]) {
			continue
		}
		j += 2
		for j < len(s) && isFactSheetASCIIDigit(s[j]) {
			j++
		}
		baseEnd := j
		if j+1 < len(s) && s[j] == '.' && isFactSheetASCIIDigit(s[j+1]) {
			j += 2
			for j < len(s) && isFactSheetASCIIDigit(s[j]) {
				j++
			}
			if end := factSheetVersionEnd(s, j); end >= 0 {
				return i, end
			}
		}
		if end := factSheetVersionEnd(s, baseEnd); end >= 0 {
			return i, end
		}
	}
	return -1, -1
}

func nextFactSheetDecimal(s string, from int) (int, int) {
	for i := from; i < len(s); i++ {
		if !isFactSheetASCIIDigit(s[i]) || i > 0 && isFactSheetASCIIWord(s[i-1]) {
			continue
		}
		end := i + 1
		for end < len(s) && isFactSheetASCIIDigit(s[end]) {
			end++
		}
		if end+1 >= len(s) || s[end] != '.' || !isFactSheetASCIIDigit(s[end+1]) {
			continue
		}
		end += 2
		for end < len(s) && isFactSheetASCIIDigit(s[end]) {
			end++
		}
		if isFactSheetWordBoundary(s, end) {
			return i, end
		}
	}
	return -1, -1
}

func nextFactSheetTicket(s string, from int) (int, int) {
	for i := from; i+4 <= len(s); i++ {
		if !isFactSheetASCIIUpper(s[i]) || i > 0 && isFactSheetASCIIWord(s[i-1]) {
			continue
		}
		end := i + 1
		for end < len(s) && isFactSheetASCIIUpperDigit(s[end]) {
			end++
		}
		if end-i < 2 {
			continue
		}
		matchEnd := -1
		for end+1 < len(s) && s[end] == '-' && isFactSheetASCIIUpperDigit(s[end+1]) {
			end += 2
			for end < len(s) && isFactSheetASCIIUpperDigit(s[end]) {
				end++
			}
			if isFactSheetWordBoundary(s, end) {
				matchEnd = end
			}
		}
		if matchEnd < 0 {
			continue
		}
		limit := min(len(s), i+120)
		for j := i; j < limit && (isFactSheetASCIIUpperDigit(s[j]) || s[j] == '-'); j++ {
			if isFactSheetASCIIDigit(s[j]) {
				return i, matchEnd
			}
		}
	}
	return -1, -1
}

func isFactSheetAssignmentValue(c byte) bool {
	return isFactSheetASCIIWord(c) || c == '.' || c == ':' || c == '/' || c == '+' || c == '-'
}

func nextFactSheetAssignment(s string, from int) (int, int) {
	for i := from; i < len(s); i++ {
		if !isFactSheetASCIIAlpha(s[i]) || i > 0 && isFactSheetASCIIWord(s[i-1]) {
			continue
		}
		nameEnd := i + 1
		for nameEnd < len(s) && isFactSheetASCIIWord(s[nameEnd]) {
			nameEnd++
		}
		if nameEnd-i < 3 || nameEnd == len(s) || s[nameEnd] != '=' {
			continue
		}
		valueStart := nameEnd + 1
		if valueStart == len(s) || !isFactSheetAssignmentValue(s[valueStart]) {
			continue
		}
		end := valueStart + 1
		for end < len(s) && end-valueStart < 64 && isFactSheetAssignmentValue(s[end]) {
			end++
		}
		return i, end
	}
	return -1, -1
}

func factSheetCurrencyMarkerLen(s string, i int) int {
	switch s[i] {
	case '$':
		return 1
	case 0xc2:
		if i+1 < len(s) && (s[i+1] == 0xa3 || s[i+1] == 0xa5) { // £, ¥
			return 2
		}
	case 0xe2:
		if i+2 < len(s) && s[i+1] == 0x82 && s[i+2] == 0xac { // €
			return 3
		}
	}
	if i+3 > len(s) {
		return 0
	}
	switch s[i : i+3] {
	case "USD", "EUR", "GBP", "CAD", "AUD", "CHF", "JPY":
		return 3
	}
	return 0
}

func nextFactSheetCurrency(s string, from int) (int, int) {
	for i := from; i < len(s); i++ {
		markerLen := factSheetCurrencyMarkerLen(s, i)
		digitStart := i + markerLen
		if markerLen == 0 || digitStart == len(s) || s[digitStart] < '0' || s[digitStart] > '9' {
			continue
		}
		runEnd := digitStart + 1
		for runEnd < len(s) {
			c := s[runEnd]
			if c < '0' || c > '9' {
				if c != ',' && c != '_' {
					break
				}
			}
			runEnd++
		}
		for end := runEnd; end > digitStart; end-- {
			if s[end-1] < '0' || s[end-1] > '9' || end < len(s) && isFactSheetASCIIWord(s[end]) {
				continue
			}
			decimalEnd := end + 3
			if decimalEnd <= len(s) && s[end] == '.' &&
				s[end+1] >= '0' && s[end+1] <= '9' && s[end+2] >= '0' && s[end+2] <= '9' &&
				(decimalEnd == len(s) || !isFactSheetASCIIWord(s[decimalEnd])) {
				return i, decimalEnd
			}
			return i, end
		}
	}
	return -1, -1
}

func nextFactSheetHex(s string, from int) (int, int) {
	for i := from; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) ||
			i > 0 && isFactSheetASCIIWord(s[i-1]) {
			continue
		}
		end := i
		hasDigit := false
		for end < len(s) {
			c = s[end]
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
				break
			}
			hasDigit = hasDigit || c >= '0' && c <= '9'
			end++
		}
		if n := end - i; n >= 7 && n <= 40 && hasDigit &&
			(end == len(s) || !isFactSheetASCIIWord(s[end])) {
			return i, end
		}
	}
	return -1, -1
}

func nextFactSheetLargeNumber(s string, from int) (int, int) {
	for i := from; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' || i > 0 && isFactSheetASCIIWord(s[i-1]) {
			continue
		}
		end := i + 1
		for end < len(s) && (s[end] >= '0' && s[end] <= '9' || s[end] == ',' || s[end] == '_') {
			end++
		}
		for end >= i+4 {
			leftWord := isFactSheetASCIIWord(s[end-1])
			rightWord := end < len(s) && isFactSheetASCIIWord(s[end])
			if leftWord != rightWord {
				return i, end
			}
			end--
		}
	}
	return -1, -1
}

func nextFactSheetFlag(s string, from int) (int, int) {
	for i := from; i < len(s); i++ {
		if s[i] != '-' || i > 0 && (isFactSheetASCIIWord(s[i-1]) || s[i-1] == '-') {
			continue
		}
		letter := i + 1
		if letter < len(s) && s[letter] == '-' {
			letter++
		}
		if letter == len(s) || !isFactSheetASCIIAlpha(s[letter]) {
			continue
		}
		end := letter + 1
		for end < len(s) && (isFactSheetASCIIWord(s[end]) || s[end] == '-') {
			end++
		}
		if end > letter+1 {
			return i, end
		}
	}
	return -1, -1
}

const (
	fsMinLen = 3
	fsMaxLen = 120
	// FactSheetMaxTokens caps the budget; highest-priority tokens kept first.
	FactSheetMaxTokens = 96
	fsMaxURLs          = 8
	fsMaxSeen          = 2048
	fsMaxScan          = 262_144
	fsPageChars        = 28_080
	fsMaxChunk         = 512
	fsCacheSlots       = 64
	fsCacheSeenSlots   = 256
	fsCacheMaxKeyBytes = 256 << 10
)

type factSheetTextCacheEntry struct {
	hash    uint64
	text    string
	value   string
	compact bool
}

var (
	factSheetTextCacheSeed = maphash.MakeSeed()
	factSheetTextCache     [fsCacheSlots]atomic.Pointer[factSheetTextCacheEntry]
	factSheetTextCacheSeen [fsCacheSeenSlots]atomic.Uint64
)

func factSheetTextCacheHash(text string, compact bool) uint64 {
	hash := maphash.String(factSheetTextCacheSeed, text)
	if compact {
		hash ^= 0x9e3779b97f4a7c15
	}
	return hash
}

func loadFactSheetTextCache(text string, compact bool) (string, bool) {
	if len(text) > fsCacheMaxKeyBytes {
		return "", false
	}
	hash := factSheetTextCacheHash(text, compact)
	entry := factSheetTextCache[hash&(fsCacheSlots-1)].Load()
	if entry == nil || entry.hash != hash || entry.compact != compact || entry.text != text {
		return "", false
	}
	return entry.value, true
}

func storeFactSheetTextCache(text string, compact bool, value string) {
	if len(text) > fsCacheMaxKeyBytes {
		return
	}
	hash := factSheetTextCacheHash(text, compact)
	seen := hash
	if seen == 0 {
		seen = 1
	}
	if factSheetTextCacheSeen[hash&(fsCacheSeenSlots-1)].Swap(seen) != seen {
		return
	}
	// ponytail: direct-mapped slots favor a lock-free hit path; move to a
	// byte-budgeted LRU only if production metrics show collision churn.
	factSheetTextCache[hash&(fsCacheSlots-1)].Store(&factSheetTextCacheEntry{
		hash:    hash,
		text:    strings.Clone(text),
		value:   value,
		compact: compact,
	})
}

func clearFactSheetTextCache() {
	for i := range factSheetTextCache {
		factSheetTextCache[i].Store(nil)
	}
	for i := range factSheetTextCacheSeen {
		factSheetTextCacheSeen[i].Store(0)
	}
}

var (
	shapeUUID       = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	shapeEmail      = regexp.MustCompile("^[A-Za-z0-9.!#$%&'*+/=?^_`{|}~-]+@[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?(?:\\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)+$")
	shapeIBAN       = regexp.MustCompile(`^[A-Z]{2}\d{2}[A-Z0-9]{8,30}$`)
	shapeCurrency   = regexp.MustCompile(`^(?:[$€£¥]|(?:USD|EUR|GBP|CAD|AUD|CHF|JPY))\d(?:[\d,_]*\d)?(?:\.\d{2})?$`)
	shapeHex        = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
	shapeHexDigit   = regexp.MustCompile(`^[0-9a-f]*\d`)
	shapeConst      = regexp.MustCompile(`^[A-Z][A-Z0-9]{2,}(?:_[A-Z0-9]+)+$`)
	shapeTicket     = regexp.MustCompile(`^[A-Z][A-Z0-9]+(?:-[A-Z0-9]+)+$`)
	shapeTicketDig  = regexp.MustCompile(`^[A-Z0-9-]*\d`)
	shapeFlag       = regexp.MustCompile(`^--?[A-Za-z][\w-]+$`)
	shapeURL        = regexp.MustCompile(`^https?://`)
	shapeCamel      = regexp.MustCompile(`^(?:[a-z]+|[A-Z][a-z0-9]+)(?:[A-Z][a-z0-9]*)+$`)
	shapeAssignment = regexp.MustCompile(`^[A-Z][A-Z0-9_]{2,}=\S+$`)
)

func factSheetFeatures(s string) fsFeature {
	var features fsFeature
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '=':
			features |= fsHasEqual
		case '@':
			features |= fsHasAt
		case '.':
			features |= fsHasDot
		case '/':
			features |= fsHasSlash
		case '-':
			features |= fsHasDash
		case '_':
			features |= fsHasUnderscore
		case ':':
			features |= fsHasColon
		default:
			switch {
			case c >= '0' && c <= '9':
				features |= fsHasDigit
			case c >= 'A' && c <= 'Z':
				features |= fsHasUpper
			case c >= 'a' && c <= 'z':
				features |= fsHasLower
			}
		}
	}
	return features
}

func isFactSheetNumber(s string) bool {
	if len(s) == 0 || !isFactSheetASCIIDigit(s[0]) {
		return false
	}
	digitsOnly := true
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case ',', '_':
			digitsOnly = false
		case '.':
			if !digitsOnly {
				return false
			}
			i++
			if i == len(s) {
				return false
			}
			for ; i < len(s); i++ {
				if !isFactSheetASCIIDigit(s[i]) {
					return false
				}
			}
			return true
		default:
			if !isFactSheetASCIIDigit(s[i]) {
				return false
			}
		}
	}
	return true
}

func priorityTier(tok string) int {
	features := factSheetFeatures(tok)
	switch {
	case features&(fsHasEqual|fsHasUpper) == fsHasEqual|fsHasUpper && shapeAssignment.MatchString(tok),
		features&fsHasDigit != 0 && shapeHex.MatchString(tok) && shapeHexDigit.MatchString(tok),
		features&fsHasDash != 0 && shapeUUID.MatchString(tok),
		features&(fsHasAt|fsHasDot) == fsHasAt|fsHasDot && shapeEmail.MatchString(tok),
		features&(fsHasUpper|fsHasDigit) == fsHasUpper|fsHasDigit && shapeIBAN.MatchString(tok),
		features&fsHasDigit != 0 && shapeCurrency.MatchString(tok),
		features&(fsHasUpper|fsHasUnderscore) == fsHasUpper|fsHasUnderscore && shapeConst.MatchString(tok),
		features&(fsHasUpper|fsHasDash|fsHasDigit) == fsHasUpper|fsHasDash|fsHasDigit && shapeTicket.MatchString(tok) && shapeTicketDig.MatchString(tok),
		features&fsHasDash != 0 && shapeFlag.MatchString(tok),
		features&fsHasDigit != 0 && isFactSheetNumber(tok),
		features&(fsHasUpper|fsHasLower) == fsHasUpper|fsHasLower && shapeCamel.MatchString(tok) && u16len(tok) >= 8:
		return 0
	case features&(fsHasColon|fsHasSlash|fsHasLower) == fsHasColon|fsHasSlash|fsHasLower && shapeURL.MatchString(tok):
		return 2
	}
	return 1
}

func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// FactSheetEntry is a kept token plus its occurrence count in the scanned text.
type FactSheetEntry struct {
	Token string
	Count int
}

type orderedCounts struct {
	order  []string
	counts map[string]int
}

type factSheetSpan struct {
	offset int
	token  string
}

var orderedCountsPool = sync.Pool{New: func() any {
	return &orderedCounts{counts: make(map[string]int)}
}}

func newOrderedCounts() *orderedCounts {
	return orderedCountsPool.Get().(*orderedCounts)
}

func putOrderedCounts(o *orderedCounts) {
	clear(o.order)
	o.order = o.order[:0]
	clear(o.counts)
	orderedCountsPool.Put(o)
}

func (o *orderedCounts) add(tok string, n int) {
	if _, ok := o.counts[tok]; !ok {
		o.order = append(o.order, tok)
	}
	o.counts[tok] += n
}

func addFactSheetSpan(oc *orderedCounts, seen map[factSheetSpan]struct{}, chunk string, offset, start, end int) {
	tok := strings.TrimRight(strings.TrimSpace(chunk[start:end]), ".,;:!?")
	tl := u16len(tok)
	if tl < fsMinLen || tl > fsMaxLen {
		return
	}
	key := factSheetSpan{offset, tok}
	if _, ok := seen[key]; ok {
		return
	}
	seen[key] = struct{}{}
	oc.add(tok, 1)
}

// ExtractFactSheetEntries mirrors TS extractFactSheetEntries: whitespace-split
// chunks, fixed pattern order, offset-level dedup within a chunk, then
// substring collapse (length-desc) and tier-budgeted selection.
func ExtractFactSheetEntries(text string) []FactSheetEntry {
	scan := text
	if u16len(scan) > fsMaxScan {
		scan = u16Slice(scan, 0, fsMaxScan)
	}
	oc := newOrderedCounts()
	for chunk := range strings.FieldsFuncSeq(scan, isJSSpace) {
		cl := u16len(chunk)
		if cl < fsMinLen || cl > fsMaxChunk {
			continue
		}
		features := factSheetFeatures(chunk)
		spanSeen := map[factSheetSpan]struct{}{}
		for _, p := range fsPatterns {
			if features&p.required != p.required {
				continue
			}
			if p.scan != nil {
				for from := 0; ; {
					start, end := p.scan(chunk, from)
					if start < 0 {
						break
					}
					addFactSheetSpan(oc, spanSeen, chunk, start, start, end)
					from = end
				}
				continue
			}
			for _, idx := range p.re.FindAllStringIndex(chunk, -1) {
				if p.verify != nil && !p.verify.MatchString(chunk[idx[0]:]) {
					continue
				}
				addFactSheetSpan(oc, spanSeen, chunk, idx[0], idx[0], idx[1])
			}
		}
		if len(oc.counts) >= fsMaxSeen {
			break
		}
	}
	entries := budgetEntries(oc.order, oc.counts, true)
	putOrderedCounts(oc)
	return entries
}

func budgetEntries(all []string, counts map[string]int, collapse bool) []FactSheetEntry {
	type ranked struct {
		t      string
		mask   uint32
		length uint16
		tier   uint8
	}
	rs := make([]ranked, len(all))
	for i, t := range all {
		var mask uint32
		for j := 0; j < len(t); j++ {
			mask |= 1 << (t[j] & 31)
		}
		rs[i] = ranked{t: t, mask: mask, length: uint16(u16len(t))}
	}
	if collapse {
		slices.SortFunc(rs, func(a, b ranked) int {
			if a.length > b.length {
				return -1
			}
			if a.length < b.length {
				return 1
			}
			return strings.Compare(a.t, b.t)
		})
		specific := rs[:0]
		for _, token := range rs {
			sub := false
			for _, container := range specific {
				if len(container.t) == len(token.t) {
					continue
				}
				if container.mask&token.mask != token.mask {
					continue
				}
				if strings.Contains(container.t, token.t) {
					sub = true
					break
				}
			}
			if !sub {
				specific = append(specific, token)
			}
		}
		rs = specific
	}
	for i := range rs {
		rs[i].tier = uint8(priorityTier(rs[i].t))
	}
	slices.SortFunc(rs, func(a, b ranked) int {
		if a.tier < b.tier {
			return -1
		}
		if a.tier > b.tier {
			return 1
		}
		if a.length > b.length {
			return -1
		}
		if a.length < b.length {
			return 1
		}
		return strings.Compare(a.t, b.t)
	})
	var kept []FactSheetEntry
	if len(rs) > 0 {
		kept = make([]FactSheetEntry, 0, min(len(rs), FactSheetMaxTokens))
	}
	urls := 0
	for _, r := range rs {
		if len(kept) >= FactSheetMaxTokens {
			break
		}
		if r.tier == 2 {
			urls++
			if urls > fsMaxURLs {
				continue
			}
		}
		c := counts[r.t]
		if c == 0 {
			c = 1
		}
		kept = append(kept, FactSheetEntry{Token: r.t, Count: c})
	}
	return kept
}

func extractFactSheetEntriesAllPages(text string, charsPerPage int) (kept []FactSheetEntry, dropped int) {
	oc := newOrderedCounts()
	total := u16len(text)
	pageCount := (total + charsPerPage - 1) / charsPerPage
	if pageCount < 1 {
		pageCount = 1
	}
	for i := 0; i < pageCount; i++ {
		chunk := u16Slice(text, i*charsPerPage, (i+1)*charsPerPage)
		for _, e := range ExtractFactSheetEntries(chunk) {
			oc.add(e.Token, e.Count)
		}
	}
	kept = budgetEntries(oc.order, oc.counts, false)
	dropped = len(oc.order) - len(kept)
	putOrderedCounts(oc)
	return kept, dropped
}

const (
	fsOpen         = "[Exact identifiers from the rendered context above (paths, ids, versions, numbers) — quote these verbatim instead of transcribing them from the image: "
	fsOpenCounts   = "[Exact identifiers from the rendered context above (paths, ids, versions, numbers) — quote these verbatim instead of transcribing them from the image; ×N marks a token that occurs N times within the imaged content: "
	fsOpenCompact  = "[Exact rendered identifiers—quote verbatim: "
	fsOpenCompactN = "[Exact rendered identifiers—quote verbatim; ×N=count: "
)

func factSheetTextFromEntries(entries []FactSheetEntry, compact bool) string {
	if len(entries) == 0 {
		return ""
	}
	anyRepeat := false
	parts := make([]string, len(entries))
	for i, e := range entries {
		if e.Count >= 2 {
			anyRepeat = true
			parts[i] = e.Token + " ×" + strconv.Itoa(e.Count)
		} else {
			parts[i] = e.Token
		}
	}
	opener := fsOpen
	switch {
	case compact && anyRepeat:
		opener = fsOpenCompactN
	case compact:
		opener = fsOpenCompact
	case anyRepeat:
		opener = fsOpenCounts
	}
	return opener + strings.Join(parts, " · ") + "]"
}

// FactSheetText builds the one-line fact sheet for text, or "" when nothing
// notable was found.
func FactSheetText(text string) string {
	return factSheetText(text, false)
}

func factSheetText(text string, compact bool) string {
	if text == "" {
		return ""
	}
	if cached, ok := loadFactSheetTextCache(text, compact); ok {
		return cached
	}
	var result string
	if u16len(text) <= fsMaxScan {
		result = factSheetTextFromEntries(ExtractFactSheetEntries(text), compact)
	} else {
		kept, _ := extractFactSheetEntriesAllPages(text, fsPageChars)
		result = factSheetTextFromEntries(kept, compact)
	}
	storeFactSheetTextCache(text, compact, result)
	return result
}
