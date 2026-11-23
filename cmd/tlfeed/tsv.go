package main

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/akit/transcript"
)

// escape writes s as one TabSeparated field: the bytes that end a field or a row, and the escape itself, are escaped;
// anything else, invalid UTF-8 too, goes as is, so a raw line comes back byte for byte.
func escape(b *bytes.Buffer, s []byte) {
	for _, c := range s {
		switch c {
		case '\\':
			b.WriteString(`\\`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case 0:
			b.WriteString(`\0`)
		default:
			b.WriteByte(c)
		}
	}
}

func escapeString(b *bytes.Buffer, s string) {
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\', '\t', '\n', '\r', 0:
			escape(b, []byte{c})
		default:
			b.WriteByte(c)
		}
	}
}

// unescape reads a TabSeparated field of a server answer.
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' || i+1 == len(s) {
			b.WriteByte(c)
			continue
		}
		i++
		switch s[i] {
		case 't':
			b.WriteByte('\t')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case '0':
			b.WriteByte(0)
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// row writes fields as one TabSeparated row; strings are escaped, numbers and times are written as they are.
type row struct{ b *bytes.Buffer }

func (r row) sep()                { r.b.WriteByte('\t') }
func (r row) end()                { r.b.WriteByte('\n') }
func (r row) str(s string)        { escapeString(r.b, s); r.sep() }
func (r row) num(n uint64)        { r.b.WriteString(strconv.FormatUint(n, 10)); r.sep() }
func (r row) signed(n int64)      { r.b.WriteString(strconv.FormatInt(max(n, 0), 10)); r.sep() }
func (r row) flag(v bool)         { r.b.WriteString(strconv.FormatBool(v)); r.sep() }
func (r row) last(s string)       { escapeString(r.b, s); r.end() }
func (r row) ms(t time.Time)      { r.b.WriteString(msTime(t)); r.sep() }
func (r row) seconds(t time.Time) { r.b.WriteString(secTime(t)); r.sep() }

func secTime(t time.Time) string {
	if t.IsZero() || t.Before(time.Unix(0, 0)) {
		return "1970-01-01 00:00:00"
	}
	return t.UTC().Format(time.DateTime)
}

func msTime(t time.Time) string {
	if t.IsZero() || t.Before(time.Unix(0, 0)) {
		return "1970-01-01 00:00:00.000"
	}
	return t.UTC().Format("2006-01-02 15:04:05.000")
}

const (
	rawColumns   = "file_id, line, off, raw"
	eventColumns = "ts, file_id, session, kind, project, line, role, who, text, images, error, subtype, model, reply_id, " +
		"request_id, in_tok, out_tok, cache_read, cache_write, cwd"
	fileColumns = "file_id, path, kind, project, session, parent, name, description, size, ingested, lines, ev_lines, mtime, inode, " +
		"first_ts, last_ts, bad, tail, gen, dirty, ver"
	// usageColumns — the columns of usage as usage_mv fills them from events
	usageColumns = "file_id, ts, session, project, model, reply_id, request_id, in_tok, out_tok, cache_read, cache_write"
)

// rawRow — one line of a file, without its '\n'.
func rawRow(b *bytes.Buffer, id uint64, line int, off int64, raw []byte) {
	r := row{b}
	r.num(id)
	r.num(uint64(line))
	r.num(uint64(off))
	escape(b, raw)
	r.end()
}

// eventRow — one event as tl shows it: text is Shown(), the reply's usage goes to its token columns.
func eventRow(b *bytes.Buffer, f *file, e *transcript.Event) {
	r := row{b}
	r.ms(e.Time)
	r.num(f.id)
	r.str(f.src.ID)
	r.str(f.kind)
	r.str(f.project)
	r.num(uint64(e.Line))
	r.str(string(e.Role))
	r.str(e.Who)
	r.str(e.Shown())
	r.num(uint64(min(max(e.Images, 0), 65535)))
	r.flag(e.Error)
	r.str(e.Subtype)
	var model, id, req string
	var in, out, cr, cw int64
	if e.Reply != nil {
		model, id, req = e.Reply.Model, e.Reply.ID, e.Reply.RequestID
		if u := e.Reply.Usage; u != nil {
			in, out, cr, cw = u.Input, u.Output, u.CacheRead, u.CacheWrite
		}
	}
	r.str(model)
	r.str(id)
	r.str(req)
	r.signed(in)
	r.signed(out)
	r.signed(cr)
	r.signed(cw)
	r.last(e.Cwd)
}

// fileRow — the state of a file after this pass. A dirty row is written before rows past its lines and ev_lines
// are sent: if the loader stops before the clean row, the next start deletes them and loads them again. Its size
// is 0, so that the next start looks at the file whatever its size.
func fileRow(b *bytes.Buffer, f *file, ver uint64, dirty bool) {
	size, flag := uint64(max(f.size, 0)), uint64(0)
	if dirty {
		size, flag = 0, 1
	}
	r := row{b}
	r.num(f.id)
	r.str(f.src.Path)
	r.str(f.kind)
	r.str(f.project)
	r.str(f.src.ID)
	r.str(f.src.ParentID)
	r.str(f.name)
	r.str(f.src.Description)
	r.num(size)
	r.num(uint64(f.ingested))
	r.num(uint64(f.lines))
	r.num(uint64(f.evLines))
	r.seconds(f.mtime)
	r.num(f.inode)
	r.ms(f.first)
	r.ms(f.last)
	r.num(uint64(max(f.bad, 0)))
	r.num(f.tail)
	r.num(f.gen)
	r.num(flag)
	r.b.WriteString(strconv.FormatUint(ver, 10))
	r.end()
}

// fields reads typed fields of one answer row; the first field that does not parse is kept in err, and the
// caller checks err once after reading them all.
type fields struct {
	row []string
	err error
}

func (p *fields) uint(i int) uint64 {
	n, err := strconv.ParseUint(p.field(i), 10, 64)
	p.fail(i, err)
	return n
}

func (p *fields) int(i int) int {
	n, err := strconv.Atoi(p.field(i))
	p.fail(i, err)
	return n
}

// time reads a DateTime or DateTime64 field; the epoch (a time never set) is the zero time.
func (p *fields) time(i int) time.Time {
	at, err := time.ParseInLocation("2006-01-02 15:04:05.999", p.field(i), time.UTC)
	p.fail(i, err)
	if at.Unix() <= 0 {
		return time.Time{}
	}
	return at
}

func (p *fields) field(i int) string {
	if i >= len(p.row) {
		p.fail(i, errors.New("no such field"))
		return "0"
	}
	return p.row[i]
}

func (p *fields) fail(i int, err error) {
	if err != nil && p.err == nil {
		p.err = fmt.Errorf("field %d of %q: %w", i+1, strings.Join(p.row, "\t"), err)
	}
}
