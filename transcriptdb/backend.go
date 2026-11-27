package transcriptdb

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
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
		if !NoLoader(err) {
			fmt.Fprintf(warn, "%s: TL_BACKEND=db: %v; reading files instead\n", tool, err)
		}
		return nil, State{}
	}
	return c, state
}

// NoLoader tells that the database was not asked for by name (TL_BACKEND unset) and this home has no loader at
// all (no loader state file): the tool reads the files without a warning, as on a machine without the database.
// With TL_BACKEND=db the same is a warning.
func NoLoader(err error) bool {
	return os.Getenv("TL_BACKEND") == "" && errors.Is(err, fs.ErrNotExist)
}
