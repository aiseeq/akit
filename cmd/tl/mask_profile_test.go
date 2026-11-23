package main

import (
	"io"
	"os"
	"testing"
	"time"
)

func TestProbeMaskCost(t *testing.T) {
	p := os.Getenv("PROBE_MASK_FILE")
	if p == "" {
		t.Skip("no file")
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := writeMasked(io.Discard, string(data)); err != nil {
		t.Fatal(err)
	}
	t.Logf("writeMasked %d bytes: %v", len(data), time.Since(start))
}
