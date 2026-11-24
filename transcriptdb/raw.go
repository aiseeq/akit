package transcriptdb

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
)

// maxRawLine bounds the length the stream may announce for one line, so a damaged answer does not allocate it.
const maxRawLine = 1 << 30

// Open gives the lines of the transcript: from raw, and for a file the database holds only in part
// (NeedsDiskRead) from disk, as without the database. A file that is neither complete in the database nor on disk
// is a SourceError wrapping the error of the disk.
func (c *Client) Open(ctx context.Context, f File) (io.ReadCloser, error) {
	if f.NeedsDiskRead() {
		src, err := os.Open(f.Path)
		if err != nil {
			return nil, sourceErr("transcript %s is loaded in part and does not open on disk: %w", f.Path, err)
		}
		return src, nil
	}
	return c.OpenRaw(ctx, f)
}

// OpenRaw is the stream of all lines of a file loaded to its end, from raw: lines 1 to f.Lines, each with its '\n',
// byte for byte as on disk. A file the database holds in part is a SourceError at once; the stream checks
// completeness (see OpenLoaded).
func (c *Client) OpenRaw(ctx context.Context, f File) (io.ReadCloser, error) {
	if f.NeedsDiskRead() {
		return nil, sourceErr("transcript %s is loaded in part (dirty=%d, ingested=%d, size=%d)", f.Path, f.Dirty, f.Ingested, f.Size)
	}
	return c.OpenLoaded(ctx, f)
}

// OpenLoaded is the stream of the lines the database has loaded, 1 to f.Lines, also when the file has grown past
// them or a send is in progress: the first f.Ingested bytes of the file. The stream checks completeness: the lines
// come in a row from 1 and end exactly at f.Lines, nothing follows the last one; otherwise Read returns a
// SourceError, not EOF.
func (c *Client) OpenLoaded(ctx context.Context, f File) (io.ReadCloser, error) {
	if f.Lines == 0 {
		return io.NopCloser(strings.NewReader("")), nil
	}
	q := Query{Name: "raw", SQL: "SELECT line, raw FROM " + c.db + ".raw WHERE file_id = {file_id:UInt64} AND line BETWEEN 1 AND {lines:UInt32} ORDER BY line FORMAT RowBinary",
		Params: map[string]string{"file_id": strconv.FormatUint(f.ID, 10), "lines": strconv.FormatUint(uint64(f.Lines), 10)}}
	resp, err := c.post(ctx, q)
	if err != nil {
		return nil, sourceErr("transcript %s: raw: %w", f.Path, err)
	}
	return &rawReader{body: resp.Body, r: bufio.NewReaderSize(resp.Body, 1<<16), file: f}, nil
}

// rawReader turns the RowBinary answer (line UInt32, raw String) into the bytes of the file.
type rawReader struct {
	body io.ReadCloser
	r    *bufio.Reader
	file File
	last uint32 // the number of the last line given
	buf  []byte // the current line with its '\n'
	off  int
	err  error // the error, or io.EOF once the end is checked
}

func (r *rawReader) Read(p []byte) (int, error) {
	for r.off == len(r.buf) {
		if r.err != nil {
			return 0, r.err
		}
		r.buf, r.err = r.next()
		r.off = 0
	}
	n := copy(p, r.buf[r.off:])
	r.off += n
	return n, nil
}

func (r *rawReader) next() ([]byte, error) {
	if r.last == r.file.Lines {
		_, err := r.r.ReadByte()
		switch {
		case err == nil:
			return nil, sourceErr("transcript %s: data in the raw answer after line %d", r.file.Path, r.last)
		case !errors.Is(err, io.EOF):
			return nil, sourceErr("transcript %s: reading the end of the raw answer: %w", r.file.Path, err)
		}
		return nil, io.EOF
	}
	var head [4]byte
	if _, err := io.ReadFull(r.r, head[:]); err != nil {
		return nil, r.truncated(err)
	}
	line := binary.LittleEndian.Uint32(head[:])
	if line != r.last+1 {
		return nil, sourceErr("transcript %s: raw gives line %d after line %d", r.file.Path, line, r.last)
	}
	n, err := binary.ReadUvarint(r.r)
	if err != nil {
		return nil, r.truncated(err)
	}
	if n > maxRawLine {
		return nil, sourceErr("transcript %s: line %d of %d bytes", r.file.Path, line, n)
	}
	buf := make([]byte, n+1)
	if _, err := io.ReadFull(r.r, buf[:n]); err != nil {
		return nil, r.truncated(err)
	}
	buf[n] = '\n'
	r.last = line
	return buf, nil
}

func (r *rawReader) truncated(err error) error {
	if errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	return sourceErr("transcript %s: the raw answer is cut after line %d of %d: %w", r.file.Path, r.last, r.file.Lines, err)
}

func (r *rawReader) Close() error { return r.body.Close() }
