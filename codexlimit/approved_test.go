package codexlimit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadApprovedMissingFile(t *testing.T) {
	approvals, err := ReadApproved(filepath.Join(t.TempDir(), "missing.tsv"))
	if err != nil || len(approvals) != 0 {
		t.Fatalf("ReadApproved = %#v, %v", approvals, err)
	}
}

func TestApprovalsForMinuteUTC(t *testing.T) {
	path := filepath.Join(t.TempDir(), "approvals.tsv")
	// The second line carries seconds and a zone: the file side is truncated to the UTC minute too.
	data := "2026-11-02T15:39:00Z\tsynthetic source\n\n2026-11-03T18:12:41+01:00\tsecond source\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	approvals, err := ReadApproved(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		at   time.Time
		want string
	}{
		{time.Date(2026, 11, 2, 15, 39, 23, 0, time.UTC), "synthetic source"},
		{time.Date(2026, 11, 2, 16, 39, 23, 0, time.FixedZone("synthetic", 3600)), "synthetic source"},
		{time.Date(2026, 11, 3, 17, 12, 5, 0, time.UTC), "second source"},
	} {
		if source, ok := approvals.For(c.at); !ok || source != c.want {
			t.Fatalf("For(%s) = %q, %v; want %q", c.at, source, ok, c.want)
		}
	}
}

func TestApprovalsForNeighbourMinute(t *testing.T) {
	path := filepath.Join(t.TempDir(), "approvals.tsv")
	if err := os.WriteFile(path, []byte("2026-11-02T15:39:00Z\tsynthetic source\n"), 0600); err != nil {
		t.Fatal(err)
	}
	approvals, err := ReadApproved(path)
	if err != nil {
		t.Fatal(err)
	}
	if source, ok := approvals.For(time.Date(2026, 11, 2, 15, 40, 0, 0, time.UTC)); ok {
		t.Fatalf("For neighbour minute = %q, %v", source, ok)
	}
}

func TestReadApprovedLongSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "approvals.tsv")
	source := strings.Repeat("s", 2<<20)
	if err := os.WriteFile(path, []byte("2026-11-02T15:39:00Z\t"+source+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	approvals, err := ReadApproved(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := approvals.For(time.Date(2026, 11, 2, 15, 39, 0, 0, time.UTC)); !ok || got != source {
		t.Fatalf("For: ok %v, source length %d", ok, len(got))
	}
}

func TestReadApprovedBadLines(t *testing.T) {
	for name, badLine := range map[string]string{
		"no-tab":       "2026-11-02T15:39:00Z source",
		"not-rfc3339":  "not-a-time\tsynthetic source",
		"empty-source": "2026-11-02T15:39:00Z\t  ",
		"long-no-tab":  strings.Repeat("x", 2<<20),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "approvals.tsv")
			if err := os.WriteFile(path, []byte("\n"+badLine+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			approvals, err := ReadApproved(path)
			if err == nil || !strings.Contains(err.Error(), path+":2:") || approvals != nil {
				t.Fatalf("ReadApproved = %#v, %v; want line 2 error", approvals, err)
			}
		})
	}
}
