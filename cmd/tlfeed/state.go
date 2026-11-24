package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// stateFile — what a supervisor reads to tell a working loader from a stuck one. Rewritten atomically.
type stateFile struct {
	Heartbeat string `json:"heartbeat"`   // when this file was written
	DBThrough string `json:"db_through"`  // start of the last successful pass with no unconfirmed complete lines
	Home      string `json:"home"`        // transcript home this loader follows
	LastRowAt string `json:"last_row_at"` // when the loader last inserted a line, by its own clock; empty — never
	LagBytes  int64  `json:"lag_bytes"`   // bytes of complete lines in known files the server has not confirmed
	Files     int    `json:"files"`       // files followed
	Error     string `json:"error"`       // the last error, empty when the last attempt succeeded
	PID       int    `json:"pid"`
}

const stampFormat = "2006-01-02T15:04:05.000Z07:00"

// status is shared by the loader and the writer of the state file.
type status struct {
	mu        sync.Mutex
	path      string
	pid       int
	lastRow   time.Time
	lag       int64
	files     int
	err       string
	dbThrough time.Time
	home      string
}

// newStatus keeps the last row time of a previous run: a restart is not a load.
func newStatus(path string) *status {
	s := &status{path: path, pid: os.Getpid()}
	if path == "" {
		return s
	}
	if data, err := os.ReadFile(path); err == nil {
		// a state file that does not parse is replaced by the first write; last_row_at starts empty then
		var old stateFile
		if json.Unmarshal(data, &old) == nil {
			if at, err := time.Parse(stampFormat, old.LastRowAt); err == nil {
				s.lastRow = at
			}
			if at, err := time.Parse(stampFormat, old.DBThrough); err == nil {
				s.dbThrough = at
			}
			s.home = old.Home
		}
	}
	return s
}

func (s *status) setError(text string) {
	s.mu.Lock()
	s.err = text
	s.mu.Unlock()
}

func (s *status) forHome(home string) {
	s.mu.Lock()
	if filepath.Clean(s.home) != filepath.Clean(home) {
		s.dbThrough = time.Time{}
	}
	s.home = home
	s.mu.Unlock()
}

func (s *status) rowsLoaded(at time.Time) {
	s.mu.Lock()
	s.lastRow = at
	s.mu.Unlock()
}

func (s *status) set(lag int64, files int) {
	s.mu.Lock()
	s.lag, s.files = lag, files
	s.mu.Unlock()
}

func (s *status) completedPass(start time.Time, lag int64) {
	if lag != 0 {
		return
	}
	s.mu.Lock()
	s.dbThrough = start
	s.mu.Unlock()
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(stampFormat)
}

// write replaces the state file: a temporary file in the same folder, then rename.
func (s *status) write(now time.Time) error {
	if s.path == "" {
		return nil
	}
	s.mu.Lock()
	st := stateFile{Heartbeat: stamp(now), DBThrough: stamp(s.dbThrough), Home: s.home, LastRowAt: stamp(s.lastRow), LagBytes: s.lag, Files: s.files, Error: s.err, PID: s.pid}
	s.mu.Unlock()
	data, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("state file: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".state-*.json")
	if err != nil {
		return fmt.Errorf("state file: %w", err)
	}
	_, werr := tmp.Write(append(data, '\n'))
	cerr := tmp.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), s.path)
	}
	if werr != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("state file: %w", werr)
	}
	return nil
}

// beat writes the state file every period until stop, idle or not, and once more at the end.
func (s *status) beat(period time.Duration, stop <-chan struct{}, done chan<- struct{}, now func() time.Time, logf func(string, ...any)) {
	defer close(done)
	t := time.NewTicker(period)
	defer t.Stop()
	failed := false
	for {
		err := s.write(now())
		if err != nil && !failed {
			logf("%v", err)
		}
		failed = err != nil
		select {
		case <-stop:
			if err := s.write(now()); err != nil {
				logf("%v", err)
			}
			return
		case <-t.C:
		}
	}
}

// defaultStatePath — $XDG_STATE_HOME/akit/tlfeed/state.json, the state folder defaulting to ~/.local/state.
func defaultStatePath(home string) string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "akit", "tlfeed", "state.json")
}
