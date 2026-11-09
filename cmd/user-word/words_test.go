package main

import "testing"

// The first letter of the pattern can stand closer to the end than the pattern is long.
func TestRawContainsFoldedASCIIFirstLetterNearTheEnd(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{"length equals capacity", append(make([]byte, 0, 6), "aaaaas"...)},
		{"text after the end stays out", append([]byte(nil), "aaaaaswiss"...)[:6]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rawContainsFoldedASCII(tc.raw, []byte("swiss")) {
				t.Fatalf("found in %q", tc.raw)
			}
		})
	}
}

func TestRawContainsFoldedASCIIMatchAtTheEnd(t *testing.T) {
	raw := append(make([]byte, 0, 10), "aaaaaSWISS"...)
	if !rawContainsFoldedASCII(raw, []byte("swiss")) {
		t.Fatal("a match ending at the last byte must count")
	}
}
