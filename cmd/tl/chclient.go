package main

import (
	"time"

	"github.com/aiseeq/akit/internal/transcriptdb"
)

// Compatibility names keep tl's call sites and tests stable; the implementation lives in transcriptdb.
type chClient = transcriptdb.Client
type chQuery = transcriptdb.Query
type chServerError = transcriptdb.ServerError

var errUnavailable = transcriptdb.ErrUnavailable

func newCHClientFromEnv() (*chClient, error)         { return transcriptdb.NewFromEnv() }
func newCHClient(base, db string) (*chClient, error) { return transcriptdb.New(base, db) }
func chString(s string) string                       { return transcriptdb.String(s) }
func chStrings(v []string) string                    { return transcriptdb.Strings(v) }
func chUints(v []uint64) string                      { return transcriptdb.Uints(v) }
func chTime(t time.Time) string                      { return transcriptdb.Time(t) }
func tsvEscape(s string) string                      { return transcriptdb.TSVEscape(s) }
func tsvUnescape(s string) string                    { return transcriptdb.TSVUnescape(s) }
