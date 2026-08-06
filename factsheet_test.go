package pxpipe

import (
	"regexp"
	"strings"
	"testing"
)

func TestFactSheetScannersMatchRegex(t *testing.T) {
	tests := []struct {
		name   string
		re     *regexp.Regexp
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

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, input := range tt.inputs {
				want := tt.re.FindAllStringIndex(input, -1)
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
