package web

import "testing"

// TestLangHint (owner, 2026-11-24): the request's Accept-Language maps
// onto the best UI language the shell can show — q-order, base tags,
// zh variants, q=0 and garbage rejected, header order breaking ties.
func TestLangHint(t *testing.T) {
	cases := []struct {
		header string
		want   string
	}{
		{"", ""},
		{"en", "en"},
		{"de-DE,de;q=0.9,en;q=0.5", "de"},
		{"fr;q=0.5,es", "es"},
		{"ES-MX,pt", "es"},
		{"zh-CN,zh;q=0.9,en;q=0.8", "zh-Hans"},
		{"xx,de;q=0.4", "de"},
		{"de;q=0", ""},
		{"junk,, ,de;q=0.3", "de"},
		{"de,en", "de"}, // tie: the header's order decides
		{"en;q=0.8, fr", "fr"},
		{"de-CH, it", "de"},
		{"* , de;q=0.1", "de"}, // "*" names no language: skipped
	}
	for _, tc := range cases {
		t.Run("["+tc.header+"]", func(t *testing.T) {
			if got := langHint(tc.header); got != tc.want {
				t.Errorf("langHint(%q) = %q, want %q", tc.header, got, tc.want)
			}
		})
	}
}
