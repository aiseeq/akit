package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// An assistant record carries its timestamp after a long content: the first 4 KiB of such a line hold no stamp.
// The search of the start offset must not skip such records as if they were before the target.
func TestTranscriptOffsetKeepsRecordsWithLateStamp(t *testing.T) {
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	var b strings.Builder
	offsets := map[int]int64{}
	stamp := func(i int) string { return base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339Nano) }
	for i := range 400 {
		offsets[i] = int64(b.Len())
		if i%10 == 0 {
			fmt.Fprintf(&b, `{"type":"user","timestamp":%q,"message":{"role":"user","content":"word %d"}}`+"\n", stamp(i), i)
			continue
		}
		fmt.Fprintf(&b, `{"message":{"role":"assistant","content":[{"type":"text","text":%q}]},"type":"assistant","timestamp":%q}`+"\n",
			strings.Repeat("x", 6000), stamp(i))
	}
	path := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	size := int64(b.Len())
	ordered, samples, err := transcriptChronological(f, size)
	if err != nil || !ordered {
		t.Fatalf("chronological: %v %v", ordered, err)
	}
	for _, target := range []int{10, 40, 150, 390} {
		got, err := transcriptOffset(f, size, base.Add(time.Duration(target)*time.Minute), samples)
		if err != nil {
			t.Fatal(err)
		}
		if got > offsets[target] {
			t.Errorf("target record %d at %d: start %d is past it", target, offsets[target], got)
		}
		if got < offsets[max(0, target-20)] {
			t.Errorf("target record %d: start %d is more than 20 records before it", target, got)
		}
	}
}

// Long user records stamp after their content too; when no record up to the upper bound has a stamp in its first
// bytes, the search keeps them: one of them may be the quote.
func TestTranscriptOffsetKeepsUnstampedRunUpToBound(t *testing.T) {
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	stamp := func(i int) string { return base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339Nano) }
	var b strings.Builder
	offsets := map[int]int64{}
	fmt.Fprintf(&b, `{"type":"user","timestamp":%q,"message":{"role":"user","content":"start"}}`+"\n", stamp(0))
	for i := 1; i <= 30; i++ {
		offsets[i] = int64(b.Len())
		fmt.Fprintf(&b, `{"type":"user","message":{"role":"user","content":%q},"timestamp":%q}`+"\n", strings.Repeat("y", 6000), stamp(i))
	}
	bound := int64(b.Len())
	fmt.Fprintf(&b, `{"type":"user","timestamp":%q,"message":{"role":"user","content":"end"}}`+"\n", stamp(31))
	path := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	samples := []transcriptSample{{offset: 0, stamp: base}, {offset: bound, stamp: base.Add(31 * time.Minute)}}
	got, err := transcriptOffset(f, int64(b.Len()), base.Add(5*time.Minute), samples)
	if err != nil {
		t.Fatal(err)
	}
	if got > offsets[5] {
		t.Fatalf("start %d is past record 5 at %d", got, offsets[5])
	}
}
