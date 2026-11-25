package transcriptdb

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"
)

// Tokens is token usage and the price metadata stored in the usage table.
type Tokens struct {
	In, Out, CacheRead, CacheWrite uint64
	CacheWrite5m, CacheWrite1h     uint64
	Fast                           bool
	Geo                            string
}

// Add adds the tokens of o.
func (t *Tokens) Add(o Tokens) {
	t.In += o.In
	t.Out += o.Out
	t.CacheRead += o.CacheRead
	t.CacheWrite += o.CacheWrite
	t.CacheWrite5m += o.CacheWrite5m
	t.CacheWrite1h += o.CacheWrite1h
}

// UsageReply is one model turn of the usage table: one row per session, reply id and request id, the record with
// the most output of the reply (a streamed reply repeats its usage in every record).
type UsageReply struct {
	At        time.Time
	FileID    uint64
	Session   string
	Project   string
	Model     string
	ReplyID   string
	RequestID string
	Tokens
}

// UsageTotal is the sum of turns of one local day, model, project, speed and route.
type UsageTotal struct {
	Day     string // YYYY-MM-DD in the location of UsageTotals
	Model   string
	Project string
	Replies uint64
	Tokens
}

// usageWindow is the condition and parameters of the replies of [from, until).
func usageWindow(from, until time.Time) (string, map[string]string) {
	return "ts >= {from:DateTime64(3, 'UTC')} AND ts < {until:DateTime64(3, 'UTC')}",
		map[string]string{"from": Time(from), "until": Time(until)}
}

// Usage gives fn the replies of [from, until), in no particular order, as they come: a month of replies is not held
// whole. Every error but the one of fn is a SourceError.
func (c *Client) Usage(ctx context.Context, from, until time.Time, fn func(UsageReply) error) error {
	where, params := usageWindow(from, until)
	var fnErr error
	err := c.EachTSV(ctx, Query{Name: "usage", Params: params,
		SQL: "SELECT toString(toUnixTimestamp64Milli(ts)), toString(file_id), session, project, model, reply_id, request_id, " +
			"toString(in_tok), toString(out_tok), toString(cache_read), toString(cache_write), toString(cache_write_5m), toString(cache_write_1h), toString(fast), geo FROM " + c.db + ".usage FINAL WHERE " + where},
		func(r []string) error {
			if len(r) != 15 {
				return sourceErr("usage: %d fields in a row, want 15", len(r))
			}
			ms, err := strconv.ParseInt(r[0], 10, 64)
			if err != nil {
				return sourceErr("usage time: %w", err)
			}
			id, err := strconv.ParseUint(r[1], 10, 64)
			if err != nil {
				return sourceErr("usage file_id: %w", err)
			}
			tok, err := parseUsageTokens(r[7:])
			if err != nil {
				return err
			}
			u := UsageReply{At: time.UnixMilli(ms).UTC(), FileID: id, Session: r[2], Project: r[3], Model: r[4], ReplyID: r[5], RequestID: r[6], Tokens: tok}
			if err := fn(u); err != nil {
				fnErr = err
				return err
			}
			return nil
		})
	return asSourceError("usage", err, fnErr)
}

// asSourceError is err of a read for the caller: nil, the error of the caller's own function as it is, or a
// SourceError.
func asSourceError(what string, err, own error) error {
	var se *SourceError
	if err == nil || own != nil && err == own || errors.As(err, &se) {
		return err
	}
	return &SourceError{fmt.Errorf("%s: %w", what, err)}
}

// UsageTotals is the token usage of [from, until) by local day in loc, model and project, sorted by them. The
// server sums by quarter hours of UTC and the days are taken here: every zone offset is a whole number of quarter
// hours, so the days match the zone's own, its daylight saving changes included. Every error is a SourceError.
func (c *Client) UsageTotals(ctx context.Context, from, until time.Time, loc *time.Location) ([]UsageTotal, error) {
	where, params := usageWindow(from, until)
	type key struct {
		day, model, project, geo string
		fast                     bool
	}
	sums := map[key]*UsageTotal{}
	err := c.EachTSV(ctx, Query{Name: "usage_totals", Params: params,
		SQL: "SELECT toString(toUnixTimestamp(toStartOfFifteenMinutes(ts))) AS q, model, project, toString(count()), " +
			"toString(sum(in_tok)), toString(sum(out_tok)), toString(sum(cache_read)), toString(sum(cache_write)), " +
			"toString(sum(cache_write_5m)), toString(sum(cache_write_1h)), toString(fast), geo FROM " + c.db + ".usage FINAL " +
			"WHERE " + where + " GROUP BY q, model, project, fast, geo"},
		func(r []string) error {
			if len(r) != 12 {
				return sourceErr("usage totals: %d fields in a row, want 12", len(r))
			}
			sec, err := strconv.ParseInt(r[0], 10, 64)
			if err != nil {
				return sourceErr("usage totals time: %w", err)
			}
			n, err := strconv.ParseUint(r[3], 10, 64)
			if err != nil {
				return sourceErr("usage totals count: %w", err)
			}
			tok, err := parseUsageTokens(r[4:])
			if err != nil {
				return err
			}
			fast := r[10] == "1" || r[10] == "true"
			k := key{day: time.Unix(sec, 0).In(loc).Format(time.DateOnly), model: r[1], project: r[2], fast: fast, geo: r[11]}
			t := sums[k]
			if t == nil {
				t = &UsageTotal{Day: k.day, Model: k.model, Project: k.project, Tokens: Tokens{Fast: k.fast, Geo: k.geo}}
				sums[k] = t
			}
			t.Replies += n
			t.Add(tok)
			return nil
		})
	if err != nil {
		return nil, asSourceError("usage totals", err, nil)
	}
	out := make([]UsageTotal, 0, len(sums))
	for _, t := range sums {
		out = append(out, *t)
	}
	slices.SortFunc(out, func(a, b UsageTotal) int {
		if c := cmp.Or(cmp.Compare(a.Day, b.Day), cmp.Compare(a.Model, b.Model), cmp.Compare(a.Project, b.Project), cmp.Compare(a.Geo, b.Geo)); c != 0 {
			return c
		}
		if a.Fast == b.Fast {
			return 0
		}
		if !a.Fast {
			return -1
		}
		return 1
	})
	return out, nil
}

func parseUsageTokens(fields []string) (Tokens, error) {
	if len(fields) != 8 {
		return Tokens{}, sourceErr("usage tokens: %d fields, want 8", len(fields))
	}
	t, err := parseTokens(fields[:4])
	if err != nil {
		return Tokens{}, err
	}
	for i, dst := range []*uint64{&t.CacheWrite5m, &t.CacheWrite1h} {
		n, err := strconv.ParseUint(fields[4+i], 10, 64)
		if err != nil {
			return Tokens{}, sourceErr("usage cache write: %w", err)
		}
		*dst = n
	}
	t.Fast = fields[6] == "1" || fields[6] == "true"
	t.Geo = fields[7]
	return t, nil
}

// parseTokens reads in_tok, out_tok, cache_read and cache_write.
func parseTokens(f []string) (Tokens, error) {
	var v [4]uint64
	for i := range v {
		n, err := strconv.ParseUint(f[i], 10, 64)
		if err != nil {
			return Tokens{}, sourceErr("usage tokens: %w", err)
		}
		v[i] = n
	}
	return Tokens{In: v[0], Out: v[1], CacheRead: v[2], CacheWrite: v[3]}, nil
}
