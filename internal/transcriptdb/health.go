package transcriptdb

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// State is the loader heartbeat and the latest committed event boundary.
type State struct {
	Heartbeat string    `json:"heartbeat"`
	DBThrough time.Time `json:"db_through"`
	Home      string    `json:"home"`
	Error     string    `json:"error"`
}

// CheckHealth validates the same tlfeed state file used by every transcript database client.
func CheckHealth(home string) (State, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		base = filepath.Join(home, ".local", "state")
	}
	data, err := os.ReadFile(filepath.Join(base, "akit", "tlfeed", "state.json"))
	if err != nil {
		return State{}, fmt.Errorf("read tlfeed state: %w", err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return State{}, fmt.Errorf("read tlfeed state: %w", err)
	}
	heartbeat, err := time.Parse(time.RFC3339Nano, st.Heartbeat)
	if err != nil || time.Since(heartbeat) > 10*time.Minute || heartbeat.After(time.Now().Add(time.Minute)) {
		return State{}, errors.New("tlfeed heartbeat is stale or invalid")
	}
	if st.Error != "" {
		return State{}, fmt.Errorf("tlfeed reports an error: %s", st.Error)
	}
	if filepath.Clean(st.Home) != filepath.Clean(home) {
		return State{}, errors.New("tlfeed home does not match this home")
	}
	if st.DBThrough.IsZero() {
		return State{}, errors.New("tlfeed has no successful db_through boundary")
	}
	if time.Since(st.DBThrough) > 10*time.Minute {
		return State{}, errors.New("tlfeed db_through is stale")
	}
	return st, nil
}

// FreshnessLine formats the status line emitted after a successful database answer.
func FreshnessLine(tool string, through, now time.Time) string {
	format := "15:04:05"
	if y, m, d := through.Local().Date(); y != now.Local().Year() || m != now.Local().Month() || d != now.Local().Day() {
		format = "2006-01-02 15:04:05"
	}
	return fmt.Sprintf("%s: данные актуальны на %s\n", tool, through.Local().Format(format))
}
