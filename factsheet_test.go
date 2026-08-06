package pxpipe

import (
	"math/rand"
	"regexp"
	"strings"
	"testing"
)

func TestFactSheetScannersMatchRegex(t *testing.T) {
	tests := []struct {
		name   string
		re     *regexp.Regexp
		verify *regexp.Regexp
		scan   func(string, int) (int, int)
		inputs []string
	}{
		{
			name: "assignment",
			re:   regexp.MustCompile(`\b[A-Za-z][A-Za-z0-9_]{2,}=[A-Za-z0-9_.:/+-]{1,64}`),
			scan: nextFactSheetAssignment,
			inputs: []string{
				"", "foo=1", "ABC_12=/srv/app-v1.2", "fo=1", "foo=", "foo=β",
				"0foo=1", "_foo=1", "-foo=1", "éfoo=1", "foo=abc!bar=2",
				"foo=1,bar=2", "foo=" + strings.Repeat("a", 64),
				"foo=" + strings.Repeat("a", 65) + "-bar=2",
			},
		},
		{
			name: "currency",
			re:   regexp.MustCompile(`(?:[$€£¥]|(?:USD|EUR|GBP|CAD|AUD|CHF|JPY))\d(?:[\d,_]*\d)?(?:\.\d{2})?\b`),
			scan: nextFactSheetCurrency,
			inputs: []string{
				"", "$1", "$12", "$1,234", "$1,234.56", "$1,234.5",
				"$1,234.567", "$1,234x", "$1,234x.56", "USD1", "abcUSD1",
				"CHF9_999.00", "€42", "£1,", "¥1__2", "CAD1_", "AUD1,234x",
				"USD1.23x", "USD1.23-", "USD1.23_", "US1", "USDx", "€x", "USD1 EUR2",
			},
		},
		{
			name:   "hex",
			re:     regexp.MustCompile(`\b[0-9a-f]{7,40}\b`),
			verify: regexp.MustCompile(`^[0-9a-f]*\d`),
			scan:   nextFactSheetHex,
			inputs: []string{
				"", "abcdefg", "abcde1f", "1234567", "0abcdef", "x1234567",
				"é1234567", "1234567g", "1234567-", strings.Repeat("a", 39) + "1",
				strings.Repeat("a", 40) + "1", "ABC1234", "abc1234 def5678",
			},
		},
		{
			name: "iban",
			re:   regexp.MustCompile(`\b[A-Z]{2}\d{2}[A-Z0-9]{8,30}\b`),
			scan: nextFactSheetIBAN,
			inputs: []string{
				"", "GB82WEST12345698765432", "GB82ABCDEFGH", "GB8ABCDEFGH",
				"xGB82WEST12345678", "éGB82WEST12345678", "GB82WEST12345678x",
				"GB82" + strings.Repeat("A", 30), "GB82" + strings.Repeat("A", 31),
				"GB82ABCDEFGH DE12ABCDEFGHIJ",
			},
		},
		{
			name: "version",
			re:   regexp.MustCompile(`\bv?\d+\.\d+(?:\.\d+)?(?:[-+][\w.]+)?\b`),
			scan: nextFactSheetVersion,
			inputs: []string{
				"", "1.2", "v1.2", "1.2.3", "1.2.3-rc.1", "1.2+build.7",
				"x1.2", "V1.2", "é1.2", "1.2x", "1.2_", "1.2.3x",
				"1.2-.x.", "1.2-...", "1.2 3.4.5+meta.1",
			},
		},
		{
			name: "decimal",
			re:   regexp.MustCompile(`\b\d+\.\d+\b`),
			scan: nextFactSheetDecimal,
			inputs: []string{
				"", "1.2", "01.002", "v1.2", "x1.2", "é1.2", "1.2x",
				"1.2_", "1.2.3", "1.2 3.4",
			},
		},
		{
			name:   "ticket",
			re:     regexp.MustCompile(`\b[A-Z][A-Z0-9]+(?:-[A-Z0-9]+)+\b`),
			verify: regexp.MustCompile(`^[A-Z0-9-]{0,119}\d`),
			scan:   nextFactSheetTicket,
			inputs: []string{
				"", "A1-B", "AB-C1", "CVE-2026-1234", "AB-CD", "AB-CD--1",
				"xCVE-2026-1234", "éCVE-2026-1234", "CVE-2026-1234x",
				"CVE-2026-1234 CVE-2027-5678",
			},
		},
		{
			name: "large-number",
			re:   regexp.MustCompile(`\b\d[\d,_]{3,}\b`),
			scan: nextFactSheetLargeNumber,
			inputs: []string{
				"", "123", "1234", "x1234", "é1234", "1234x", "1234,",
				"1234,a", "1,234", "1___", "1,,,", "1,,,_a", ",1234, 5678",
				"1234.5678", "123456,abc", "1234/5678",
			},
		},
	}
	rng := rand.New(rand.NewSource(3))
	parts := []string{
		"a", "Z", "0", "_", "-", "+", ".", "/", " ", "\t", "\n", "\x00", "é", "한", "😀",
		"GB82WEST12345698765432", "v1.2.3-rc.1", "123.45", "CVE-2026-1234", "AB-CD--1",
	}
	for range 5_000 {
		var input strings.Builder
		for range rng.Intn(32) {
			input.WriteString(parts[rng.Intn(len(parts))])
		}
		for i := range tests {
			tests[i].inputs = append(tests[i].inputs, input.String())
		}
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, input := range tt.inputs {
				all := tt.re.FindAllStringIndex(input, -1)
				want := all[:0]
				for _, match := range all {
					if tt.verify == nil || tt.verify.MatchString(input[match[0]:]) {
						want = append(want, match)
					}
				}
				var got [][]int
				for from := 0; ; {
					start, end := tt.scan(input, from)
					if start < 0 {
						break
					}
					got = append(got, []int{start, end})
					from = end
				}
				if len(got) != len(want) {
					t.Fatalf("%q: got indexes %v, want %v", input, got, want)
				}
				for i := range want {
					if got[i][0] != want[i][0] || got[i][1] != want[i][1] {
						t.Fatalf("%q: got indexes %v, want %v", input, got, want)
					}
				}
			}
		})
	}
}

func TestFactSheetFlagScannerMatchesRegex(t *testing.T) {
	re := regexp.MustCompile(`(?:^|[^\w-])(--?[A-Za-z][\w-]+)`)
	inputs := []string{
		"", "-a", "-ab", "--a", "--ab", "---abc", "!-flag", "x-flag",
		"_-flag", "é-flag", "😀--flag_1", "-a-", "-a! -bc", "--a1 -z_",
	}
	rng := rand.New(rand.NewSource(1))
	alphabet := []rune("abXY09_-!.:/\x00é한😀")
	for range 500 {
		chars := make([]rune, rng.Intn(96))
		for i := range chars {
			chars[i] = alphabet[rng.Intn(len(alphabet))]
		}
		inputs = append(inputs, string(chars))
	}
	for _, input := range inputs {
		matches := re.FindAllStringSubmatchIndex(input, -1)
		var got [][2]int
		for from := 0; ; {
			start, end := nextFactSheetFlag(input, from)
			if start < 0 {
				break
			}
			got = append(got, [2]int{start, end})
			from = end
		}
		if len(got) != len(matches) {
			t.Fatalf("%q: got indexes %v, want %v", input, got, matches)
		}
		for i, match := range matches {
			if got[i] != [2]int{match[2], match[3]} {
				t.Fatalf("%q: got indexes %v, want group %v", input, got, matches)
			}
		}
	}
}
