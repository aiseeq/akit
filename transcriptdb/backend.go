package transcriptdb

import (
	"fmt"
	"io"
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

// SourceFromEnv is TL_BACKEND, the switch tl reads: files (also when unset) or db; another value is an error.
func SourceFromEnv() (Source, error) {
	switch b := os.Getenv("TL_BACKEND"); b {
	case "", string(Files):
		return Files, nil
	case string(DB):
		return DB, nil
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

// Backend is the start of a tool that reads the database when TL_BACKEND=db: the client and the loader state, or a
// nil client when the tool reads the files. Why the database is not used goes to warn as one line of the tool:
// an unknown TL_BACKEND ("…; reading files"), an unhealthy loader or a bad address ("…; reading files instead").
// TL_BACKEND unset or files is not a warning.
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
		fmt.Fprintf(warn, "%s: TL_BACKEND=db: %v; reading files instead\n", tool, err)
		return nil, State{}
	}
	return c, state
}
