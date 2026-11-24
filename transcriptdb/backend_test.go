package transcriptdb

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSourceFromEnv(t *testing.T) {
	for env, want := range map[string]Source{"": Files, "files": Files, "db": DB} {
		t.Setenv("TL_BACKEND", env)
		if got, err := SourceFromEnv(); err != nil || got != want {
			t.Fatalf("TL_BACKEND=%q: %q %v", env, got, err)
		}
	}
	t.Setenv("TL_BACKEND", "bogus")
	if got, err := SourceFromEnv(); err == nil || got != Files {
		t.Fatalf("unknown source accepted: %q", got)
	}
}

// loaderState writes the tlfeed state of home with the given heartbeat.
func loaderState(t *testing.T, home string, heartbeat time.Time) time.Time {
	t.Helper()
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	dir := filepath.Join(stateHome, "akit", "tlfeed")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	through := time.Now().Add(-time.Minute).UTC()
	data, err := json.Marshal(State{Heartbeat: heartbeat.Format(time.RFC3339Nano), DBThrough: through, Home: home})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return through
}

func TestBackend(t *testing.T) {
	home := t.TempDir()
	through := loaderState(t, home, time.Now())
	var comment string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		comment = r.URL.Query().Get("log_comment")
		io.WriteString(w, "1\n")
	}))
	defer s.Close()
	t.Setenv("TL_CLICKHOUSE_URL", s.URL)
	var warn bytes.Buffer

	t.Setenv("TL_BACKEND", "")
	if c, _ := Backend("tool1", home, &warn); c != nil || warn.Len() > 0 {
		t.Fatalf("files by default: %v %q", c, warn.String())
	}
	t.Setenv("TL_BACKEND", "dbx")
	if c, _ := Backend("tool1", home, &warn); c != nil || warn.String() != "tool1: TL_BACKEND=\"dbx\" is not files or db; reading files\n" {
		t.Fatalf("unknown backend: %v %q", c, warn.String())
	}
	warn.Reset()
	t.Setenv("TL_BACKEND", "db")
	c, st := Backend("tool1", home, &warn)
	if c == nil || warn.Len() > 0 || !st.DBThrough.Equal(through) {
		t.Fatalf("healthy loader: %v %+v %q", c, st, warn.String())
	}
	if _, err := c.Select(t.Context(), "probe", "SELECT 1", nil); err != nil || comment != "tool1:probe" {
		t.Fatalf("query of the tool: %v %q", err, comment)
	}
	t.Setenv("TL_CLICKHOUSE_URL", "not a url")
	if c, _ := Backend("tool1", home, &warn); c != nil || !strings.HasPrefix(warn.String(), "tool1: TL_BACKEND=db: bad TL_CLICKHOUSE_URL") ||
		!strings.HasSuffix(warn.String(), "; reading files instead\n") {
		t.Fatalf("bad address: %v %q", c, warn.String())
	}
	warn.Reset()
	t.Setenv("TL_CLICKHOUSE_URL", s.URL)
	loaderState(t, home, time.Now().Add(-time.Hour))
	if c, _ := Backend("tool1", home, &warn); c != nil || warn.String() != "tool1: TL_BACKEND=db: tlfeed heartbeat is stale or invalid; reading files instead\n" {
		t.Fatalf("stale loader: %v %q", c, warn.String())
	}
	// Connect does not read TL_BACKEND: the caller has chosen the database.
	t.Setenv("TL_BACKEND", "files")
	loaderState(t, home, time.Now())
	if c, _, err := Connect("tool2", home); err != nil || c == nil || c.tool != "tool2" {
		t.Fatalf("Connect: %v %v", c, err)
	}
	if _, _, err := Connect("tool2", t.TempDir()); err == nil {
		t.Fatal("Connect accepted the loader of another home")
	}
}
