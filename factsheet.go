package pxpipe

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
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
	// group is the submatch index carrying the token (0 = whole match).
	group int
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
	{re: regexp.MustCompile(`\b[A-Z]{2}\d{2}[A-Z0-9]{8,30}\b`), required: fsHasUpper | fsHasDigit},
	{re: regexp.MustCompile(`(?:[$€£¥]|(?:USD|EUR|GBP|CAD|AUD|CHF|JPY))\d(?:[\d,_]*\d)?(?:\.\d{2})?\b`), required: fsHasDigit},
	{re: regexp.MustCompile(`(?:[\w@~+-]+)?(?:/[\w.@+-]+)+\.[A-Za-z]\w{0,8}\b`), required: fsHasSlash | fsHasDot},
	{re: regexp.MustCompile(`/[\w.@+-]+(?:/[\w.@+-]+)+/?`), required: fsHasSlash},
	// git sha / long hex: JS `(?=[0-9a-f]*\d)` emulated via verify.
	{re: regexp.MustCompile(`\b[0-9a-f]{7,40}\b`), verify: regexp.MustCompile(`^[0-9a-f]*\d`), required: fsHasDigit},
	{re: regexp.MustCompile(`\bv?\d+\.\d+(?:\.\d+)?(?:[-+][\w.]+)?\b`), required: fsHasDigit | fsHasDot},
	{re: regexp.MustCompile(`(?:^|[^\w-])(--?[A-Za-z][\w-]+)`), group: 1, required: fsHasDash},
	{scan: nextFactSheetLargeNumber, required: fsHasDigit},
	{re: regexp.MustCompile(`\b\d+\.\d+\b`), required: fsHasDigit | fsHasDot},
	{re: regexp.MustCompile(`\b[A-Z][A-Z0-9]{2,}(?:_[A-Z0-9]+)+\b`), required: fsHasUpper | fsHasUnderscore},
	{re: regexp.MustCompile(`\b(?:[a-z]+|[A-Z][a-z0-9]+)(?:[A-Z][a-z0-9]*)+\b`), required: fsHasUpper | fsHasLower},
	// ticket/advisory codes: JS `(?=[A-Z0-9-]{0,119}\d)` emulated via verify.
	{re: regexp.MustCompile(`\b[A-Z][A-Z0-9]+(?:-[A-Z0-9]+)+\b`), verify: regexp.MustCompile(`^[A-Z0-9-]{0,119}\d`), required: fsHasUpper | fsHasDash | fsHasDigit},
}

func isFactSheetASCIIAlpha(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z'
}

func isFactSheetASCIIWord(c byte) bool {
	return isFactSheetASCIIAlpha(c) || c >= '0' && c <= '9' || c == '_'
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
)

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
	shapeNum        = regexp.MustCompile(`^\d[\d,_]*$|^\d+\.\d+$`)
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

func priorityTier(tok string) int {
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
		shapeNum.MatchString(tok),
		shapeCamel.MatchString(tok) && u16len(tok) >= 8:
		return 0
	case shapeURL.MatchString(tok):
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

func newOrderedCounts() *orderedCounts {
	return &orderedCounts{counts: map[string]int{}}
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
			for _, idx := range p.re.FindAllStringSubmatchIndex(chunk, -1) {
				gs, ge := idx[0], idx[1]
				if p.group > 0 {
					gs, ge = idx[2*p.group], idx[2*p.group+1]
					if gs < 0 {
						continue
					}
				}
				if p.verify != nil && !p.verify.MatchString(chunk[idx[0]:]) {
					continue
				}
				addFactSheetSpan(oc, spanSeen, chunk, idx[0], gs, ge)
			}
		}
		if len(oc.counts) >= fsMaxSeen {
			break
		}
	}
	return budgetEntries(oc.order, oc.counts, true)
}

func budgetEntries(all []string, counts map[string]int, collapse bool) []FactSheetEntry {
	candidates := all
	if collapse {
		ordered := append([]string(nil), all...)
		sort.SliceStable(ordered, func(i, j int) bool {
			li, lj := u16len(ordered[i]), u16len(ordered[j])
			if li != lj {
				return li > lj
			}
			return ordered[i] < ordered[j]
		})
		var specific []string
		for _, t := range ordered {
			sub := false
			for _, k := range specific {
				if strings.Contains(k, t) {
					sub = true
					break
				}
			}
			if !sub {
				specific = append(specific, t)
			}
		}
		candidates = specific
	}
	type ranked struct {
		t    string
		tier int
	}
	rs := make([]ranked, len(candidates))
	for i, t := range candidates {
		rs[i] = ranked{t, priorityTier(t)}
	}
	sort.SliceStable(rs, func(i, j int) bool {
		if rs[i].tier != rs[j].tier {
			return rs[i].tier < rs[j].tier
		}
		li, lj := u16len(rs[i].t), u16len(rs[j].t)
		if li != lj {
			return li > lj
		}
		return rs[i].t < rs[j].t
	})
	var kept []FactSheetEntry
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
	return kept, len(oc.order) - len(kept)
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
	if u16len(text) <= fsMaxScan {
		return factSheetTextFromEntries(ExtractFactSheetEntries(text), compact)
	}
	kept, _ := extractFactSheetEntriesAllPages(text, fsPageChars)
	return factSheetTextFromEntries(kept, compact)
}
