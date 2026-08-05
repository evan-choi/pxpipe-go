package pxpipe

import "testing"

func TestU16Slice(t *testing.T) {
	input := "a😀b"
	for _, tc := range []struct {
		name       string
		start, end int
		want       string
	}{
		{"whole", 0, 4, input},
		{"aligned astral", 1, 3, "😀"},
		{"high surrogate", 1, 2, "�"},
		{"low surrogate", 2, 3, "�"},
		{"low surrogate suffix", 2, 4, "�b"},
		{"past end", 3, 99, "b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := u16Slice(input, tc.start, tc.end); got != tc.want {
				t.Fatalf("u16Slice() = %q, want %q", got, tc.want)
			}
		})
	}

	var got string
	if allocs := testing.AllocsPerRun(100, func() { got = u16Slice(input, 1, 3) }); allocs != 0 {
		t.Fatalf("aligned slice allocated %v times: %q", allocs, got)
	}
}
