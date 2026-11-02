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
	// The second line carries seconds and a zone: it is kept to the second in UTC and
	// matches a reset time less than a minute away.
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

func approvalsFrom(t *testing.T, data string) Approvals {
	t.Helper()
	path := filepath.Join(t.TempDir(), "approvals.tsv")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	approvals, err := ReadApproved(path)
	if err != nil {
		t.Fatal(err)
	}
	return approvals
}

// The server reports one window with seconds that drift by one; across a minute
// boundary that must still match the approval line.
func TestApprovalsForAcrossMinuteBoundary(t *testing.T) {
	approvals := approvalsFrom(t, "2026-11-09T18:41:59+01:00\tsynthetic source\n")
	for _, at := range []time.Time{
		time.Date(2026, 11, 9, 17, 41, 59, 0, time.UTC),
		time.Date(2026, 11, 9, 17, 42, 0, 0, time.UTC),
		time.Date(2026, 11, 9, 18, 42, 1, 0, time.FixedZone("synthetic", 3600)),
		time.Date(2026, 11, 9, 17, 41, 58, 0, time.UTC),
	} {
		if source, ok := approvals.For(at); !ok || source != "synthetic source" {
			t.Fatalf("For(%s) = %q, %v", at, source, ok)
		}
	}
	reverse := approvalsFrom(t, "2026-11-09T17:42:00Z\tsynthetic source\n")
	if source, ok := reverse.For(time.Date(2026, 11, 9, 17, 41, 59, 0, time.UTC)); !ok || source != "synthetic source" {
		t.Fatalf("For one second before the line = %q, %v", source, ok)
	}
}

func TestApprovalsForToleranceEdge(t *testing.T) {
	approvals := approvalsFrom(t, "2026-11-09T17:42:00Z\tsynthetic source\n")
	line := time.Date(2026, 11, 9, 17, 42, 0, 0, time.UTC)
	for _, at := range []time.Time{line.Add(ApprovalTolerance), line.Add(-ApprovalTolerance), line.Add(ApprovalTolerance + time.Second), line.Add(72 * time.Hour)} {
		if source, ok := approvals.For(at); ok {
			t.Fatalf("For(%s) = %q, want no match", at, source)
		}
	}
	if _, ok := approvals.For(line.Add(ApprovalTolerance - time.Nanosecond)); !ok {
		t.Fatal("just inside the tolerance must match")
	}
}

func TestApprovalsForNearestLine(t *testing.T) {
	approvals := approvalsFrom(t, "2026-11-09T17:42:00Z\tfirst\n2026-11-09T17:42:40Z\tsecond\n")
	at := time.Date(2026, 11, 9, 17, 42, 30, 0, time.UTC)
	for range 50 { // map order is random: the nearest must win every time
		if source, ok := approvals.For(at); !ok || source != "second" {
			t.Fatalf("For(%s) = %q, %v; want second", at, source, ok)
		}
	}
	tie := time.Date(2026, 11, 9, 17, 42, 20, 0, time.UTC)
	for range 50 {
		if source, ok := approvals.For(tie); !ok || source != "first" {
			t.Fatalf("For(%s) = %q, %v; want the earlier line on a tie", tie, source, ok)
		}
	}
}

func TestApprovalsForFarLines(t *testing.T) {
	// More than ~292 years apart, Time.Sub saturates; such lines must still be far.
	approvals := approvalsFrom(t, "2500-01-01T00:00:00Z\tfuture\n1700-01-01T00:00:00Z\tpast\n")
	if source, ok := approvals.For(time.Date(2026, 11, 9, 17, 42, 0, 0, time.UTC)); ok {
		t.Fatalf("For = %q, want no match for lines centuries away", source)
	}
}
