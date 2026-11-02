package codexlimit

import (
	"bufio"
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
	file, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Approvals{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("codexlimit: open approvals: %w", err)
	}
	defer file.Close()

	approvals := Approvals{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := scanner.Text()
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
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%s: read approvals: %w", path, err)
	}
	return approvals, nil
}

// For finds the source approving the UTC minute containing resetsAt.
func (approvals Approvals) For(resetsAt time.Time) (string, bool) {
	source, ok := approvals[resetsAt.UTC().Truncate(time.Minute)]
	return source, ok
}
