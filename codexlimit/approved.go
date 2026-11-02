package codexlimit

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"
)

// Approvals maps planned reset times, rounded down to the UTC minute, to their sources.
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
		approvals[at.UTC().Truncate(time.Minute)] = source
	}
	return approvals, nil
}

// For finds the source approving the UTC minute containing resetsAt.
func (approvals Approvals) For(resetsAt time.Time) (string, bool) {
	source, ok := approvals[resetsAt.UTC().Truncate(time.Minute)]
	return source, ok
}
