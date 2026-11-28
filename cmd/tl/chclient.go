package main

import "github.com/aiseeq/akit/transcriptdb"

// Compatibility names keep tl's call sites and tests stable; the implementation lives in transcriptdb.
type chClient = transcriptdb.Client
type chQuery = transcriptdb.Query
type chServerError = transcriptdb.ServerError

var errUnavailable = transcriptdb.ErrUnavailable

// newCHClientFromEnv and newCHClient tag the queries of tl with its name in log_comment.
func newCHClientFromEnv() (*chClient, error)         { return tlNamed(transcriptdb.NewFromEnv()) }
func newCHClient(base, db string) (*chClient, error) { return tlNamed(transcriptdb.New(base, db)) }

func tlNamed(c *chClient, err error) (*chClient, error) {
	if err != nil {
		return nil, err
	}
	return c.WithTool("tl"), nil
}

var (
	chString    = transcriptdb.String
	chStrings   = transcriptdb.Strings
	chUints     = transcriptdb.Uints
	chTime      = transcriptdb.Time
	tsvEscape   = transcriptdb.TSVEscape
	tsvUnescape = transcriptdb.TSVUnescape
)
