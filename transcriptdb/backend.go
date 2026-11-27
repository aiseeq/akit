package transcriptdb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"time"
)

// Source is where a tool reads transcripts: the files on disk or the database.
type Source string

const (
	// Files reads the transcripts on disk.
	Files Source = "files"
	// DB reads the database the loader fills.
	DB Source = "db"
)

// SourceFromEnv is TL_BACKEND, the switch tl, prior and user-word read: db (also when unset) or files, which brings
// back the file path whole; another value is an error, and the tool reads the files.
func SourceFromEnv() (Source, error) {
	switch b := os.Getenv("TL_BACKEND"); b {
	case "", string(DB):
		return DB, nil
	case string(Files):
		return Files, nil
	default:
		return Files, fmt.Errorf("TL_BACKEND=%q is not files or db", b)
	}
}

// Connect opens the database for tool regardless of TL_BACKEND: the loader of home must be healthy (CheckHealth),
// the address comes from TL_CLICKHOUSE_URL and TL_CLICKHOUSE_DB, the queries carry the tool name (WithTool). The
// state gives the boundary the data is current to (FreshnessLine).
func Connect(tool, home string) (*Client, State, error) {
	state, err := CheckHealth(home)
	if err != nil {
		return nil, State{}, err
	}
	c, err := NewFromEnv()
	if err != nil {
		return nil, State{}, err
	}
	return c.WithTool(tool), state, nil
}

// Backend is the start of a tool that reads the database unless TL_BACKEND=files: the client and the loader state,
// or a nil client when the tool reads the files. Why the database is not used goes to warn as one line of the tool:
// an unknown TL_BACKEND ("…; reading files"), an unhealthy loader or a bad address ("…; reading files instead").
// TL_BACKEND=files is not a warning.
func Backend(tool, home string, warn io.Writer) (*Client, State) {
	src, err := SourceFromEnv()
	if err != nil {
		fmt.Fprintf(warn, "%s: %v; reading files\n", tool, err)
		return nil, State{}
	}
	if src != DB {
		return nil, State{}
	}
	c, state, err := Connect(tool, home)
	if err != nil {
		if !NoLoader(context.Background(), err) {
			fmt.Fprintf(warn, "%s: database: %v; reading files instead\n", tool, err)
		}
		return nil, State{}
	}
	return c, state
}

// NoLoader tells that the database was not asked for by name (TL_BACKEND unset) and this machine has none: no
// loader state file for the home and no server answering at TL_CLICKHOUSE_URL. The tool then reads the files
// without a warning. A server that answers while the state file is missing (another XDG_STATE_HOME or HOME of the
// caller) is a warning, as is anything with TL_BACKEND=db.
func NoLoader(ctx context.Context, err error) bool {
	if os.Getenv("TL_BACKEND") != "" || !errors.Is(err, fs.ErrNotExist) {
		return false
	}
	if c, cerr := NewFromEnv(); cerr == nil {
		ctx, cancel := context.WithTimeout(ctx, pingTimeout)
		defer cancel()
		return c.Ping(ctx) != nil
	}
	// a bad address is not a machine without the database: Connect reports it as a warning
	return false
}

// pingTimeout bounds the probe of NoLoader: a machine without the server refuses the connection at once.
const pingTimeout = 200 * time.Millisecond
