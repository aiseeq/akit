package main

import (
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/transcriptdb"
)

func TestFreshnessLineUsesFullDateAcrossMidnight(t *testing.T) {
	now := time.Date(2026, 11, 25, 0, 1, 0, 0, time.Local)
	through := time.Date(2026, 11, 24, 23, 59, 0, 0, time.Local)
	got := transcriptdb.FreshnessLine("tl", through, now)
	want := "tl: данные актуальны на 2026-11-24 23:59:00\n"
	if !strings.Contains(got, want) {
		t.Fatalf("freshness line = %q, want date across midnight: %q", got, want)
	}
}
