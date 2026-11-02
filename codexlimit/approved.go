package codexlimit

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"
)

// ApprovalTolerance is how far a reset time may be from an approval line and still
// match it: the app server reports the same window with seconds that drift by one,
// while a reset moves the window by days.
const ApprovalTolerance = time.Minute

// Approvals maps the planned reset times of approval lines, in UTC, to their sources.
type Approvals map[time.Time]string

// ReadApproved reads reset approvals from path.
func ReadApproved(path string) (Approvals, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Approvals{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("codexlimit: read approvals: %w", err)
	}

	approvals := Approvals{}
	// The whole file is read at once, so a line of any length still gets its number in an error.
	for i, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		lineNumber := i + 1
		if line == "" {
			continue
		}
		atText, source, ok := strings.Cut(line, "\t")
		at, parseErr := time.Parse(time.RFC3339, atText)
		if !ok || parseErr != nil || strings.TrimSpace(source) == "" {
			return nil, fmt.Errorf("%s:%d: want RFC3339<TAB>source, got %q", path, lineNumber, line)
		}
		approvals[at.UTC()] = source
	}
	return approvals, nil
}

// For finds the source of the approval line nearest to resetsAt within
// ApprovalTolerance; of two lines equally near, the earlier one.
func (approvals Approvals) For(resetsAt time.Time) (string, bool) {
	var (
		source string
		best   time.Duration
		bestAt time.Time
		ok     bool
	)
	for at, src := range approvals {
		// Compare times first: Sub saturates for lines centuries away.
		if !at.After(resetsAt.Add(-ApprovalTolerance)) || !at.Before(resetsAt.Add(ApprovalTolerance)) {
			continue
		}
		d := resetsAt.Sub(at)
		if d < 0 {
			d = -d
		}
		if !ok || d < best || d == best && at.Before(bestAt) {
			source, best, bestAt, ok = src, d, at, true
		}
	}
	return source, ok
}
