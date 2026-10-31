package main

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
)

func TestRequiredLiterals(t *testing.T) {
	tests := []struct {
		pattern string
		want    [][]string
	}{
		{`hello`, [][]string{{"hello"}}},
		{`a|b|c`, [][]string{{"a", "b", "c"}}},
		{`[ab]`, [][]string{{"a", "b"}}},
		{`[a-z]`, nil},
		{`graft (commit|gate)`, [][]string{{"graft "}, {"commit", "gate"}}},
		{`.|\w+|x{2}`, nil},
		{`foo.*bar`, [][]string{{"foo"}, {"bar"}}},
		{`a"b`, [][]string{{"a"}, {"b"}}},
	}
	for _, tt := range tests {
		opts := options{grep: tt.pattern}
		f, err := newFilter(opts)
		if err != nil {
			t.Fatalf("%q: %v", tt.pattern, err)
		}
		if !equalNeedles(f.needles, tt.want) {
			t.Errorf("%q: got %#v, want %#v", tt.pattern, f.needles, tt.want)
		}
	}
}

func equalNeedles(a, b [][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return false
		}
		for j := range a[i] {
			if a[i][j] != b[i][j] {
				return false
			}
		}
	}
	return true
}

func FuzzKeepRawSuperset(f *testing.F) {
	patterns := []string{`K`, `ſ`, `ß`, `ks`, `sk`, `ЗАВИС`, `ожидани`, `Kelvin`, `a|b|c`, `graft (commit|gate)`, `foo.*bar`, `.`, `\w+`, `x{2}`}
	lines := []string{"K", "K", "s", "ſ", "ß", "ẞ", "kſ", "ſK", "Завис", "ЗАВИС", "ОЖИДАНИЕ", "привет", "графт", "graft gate", "xxy", "🙂K", "я"}
	for _, p := range patterns {
		for _, line := range lines {
			f.Add(p, line)
		}
	}
	f.Fuzz(func(t *testing.T, pattern, line string) {
		if len(pattern) > 80 || len(line) > 4096 {
			t.Skip()
		}
		re, err := regexp.Compile("(?i)" + pattern)
		if err != nil {
			if _, filterErr := newFilter(options{grep: pattern}); filterErr == nil {
				t.Fatalf("filter accepted invalid regexp %q", pattern)
			}
			return
		}
		filter, err := newFilter(options{grep: pattern})
		if err != nil {
			t.Fatal(err)
		}
		if re.MatchString(line) && !filter.keepRaw([]byte(line)) {
			t.Fatalf("raw filter rejected match: %q in %q", pattern, line)
		}
	})
}

func BenchmarkKeepRaw(b *testing.B) {
	fixtures := []struct {
		name    string
		pattern string
		text    string
	}{
		{
			name:    "russian",
			pattern: `ожидани`,
			text:    "ответы и общие сведения: русский текст без совпадения и пояснения ",
		},
		{
			name:    "ascii-sub-n",
			pattern: `sub -n`,
			text:    "status session settings source stream summary sample ",
		},
		{
			name:    "ascii-wfor",
			pattern: `wfor`,
			text:    "word workflow window work wrapper ",
		},
	}
	for _, fixture := range fixtures {
		b.Run(fixture.name, func(b *testing.B) {
			f, err := newFilter(options{grep: fixture.pattern})
			if err != nil {
				b.Fatal(err)
			}
			raw := []byte(`{"type":"assistant","message":"` + strings.Repeat(fixture.text, 1<<20/len(fixture.text)) + `"}`)
			needle := []byte(strings.ToLower(fixture.pattern))
			b.SetBytes(int64(len(raw)))
			b.Run("fold-search", func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					f.keepRaw(raw)
				}
			})
			b.Run("lower-copy-baseline", func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					bytes.Contains(bytes.ToLower(raw), needle)
				}
			})
		})
	}
}
