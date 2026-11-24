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

// Tokens is the token usage of model replies, as the usage table keeps it.
type Tokens struct {
	In         uint64
	Out        uint64
	CacheRead  uint64
	CacheWrite uint64
}

// Add adds the tokens of o.
func (t *Tokens) Add(o Tokens) {
	t.In += o.In
	t.Out += o.Out
	t.CacheRead += o.CacheRead
	t.CacheWrite += o.CacheWrite
}

// UsageReply is one model reply of the usage table: one row per session, reply id and request id, the record with
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

// UsageTotal is the sum of the replies of one local day, model and project.
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
			"toString(in_tok), toString(out_tok), toString(cache_read), toString(cache_write) FROM " + c.db + ".usage FINAL WHERE " + where},
		func(r []string) error {
			if len(r) != 11 {
				return sourceErr("usage: %d fields in a row, want 11", len(r))
			}
			ms, err := strconv.ParseInt(r[0], 10, 64)
			if err != nil {
				return sourceErr("usage time: %w", err)
			}
			id, err := strconv.ParseUint(r[1], 10, 64)
			if err != nil {
				return sourceErr("usage file_id: %w", err)
			}
			tok, err := parseTokens(r[7:])
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
	type key struct{ day, model, project string }
	sums := map[key]*UsageTotal{}
	err := c.EachTSV(ctx, Query{Name: "usage_totals", Params: params,
		SQL: "SELECT toString(toUnixTimestamp(toStartOfFifteenMinutes(ts))) AS q, model, project, toString(count()), " +
			"toString(sum(in_tok)), toString(sum(out_tok)), toString(sum(cache_read)), toString(sum(cache_write)) FROM " + c.db + ".usage FINAL " +
			"WHERE " + where + " GROUP BY q, model, project"},
		func(r []string) error {
			if len(r) != 8 {
				return sourceErr("usage totals: %d fields in a row, want 8", len(r))
			}
			sec, err := strconv.ParseInt(r[0], 10, 64)
			if err != nil {
				return sourceErr("usage totals time: %w", err)
			}
			n, err := strconv.ParseUint(r[3], 10, 64)
			if err != nil {
				return sourceErr("usage totals count: %w", err)
			}
			tok, err := parseTokens(r[4:])
			if err != nil {
				return err
			}
			k := key{time.Unix(sec, 0).In(loc).Format(time.DateOnly), r[1], r[2]}
			t := sums[k]
			if t == nil {
				t = &UsageTotal{Day: k.day, Model: k.model, Project: k.project}
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
		return cmp.Or(cmp.Compare(a.Day, b.Day), cmp.Compare(a.Model, b.Model), cmp.Compare(a.Project, b.Project))
	})
	return out, nil
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
