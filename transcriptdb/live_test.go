package transcriptdb

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLiveLinesEqualFiles checks the package against a live database: for every path listed in the file named by
// TRANSCRIPTDB_LIVE_FILES (one per line), the lines read through the package equal the bytes of the file on disk
// over what the database has loaded (the first Ingested bytes: a file may grow during the check). A path missing
// on disk or in the database fails its subtest. Runs only with TRANSCRIPTDB_LIVE=1; the address comes from
// TL_CLICKHOUSE_URL and TL_CLICKHOUSE_DB.
func TestLiveLinesEqualFiles(t *testing.T) {
	if os.Getenv("TRANSCRIPTDB_LIVE") != "1" {
		t.Skip("set TRANSCRIPTDB_LIVE=1 and TRANSCRIPTDB_LIVE_FILES to compare with a live database")
	}
	list := os.Getenv("TRANSCRIPTDB_LIVE_FILES")
	if list == "" {
		t.Fatal("TRANSCRIPTDB_LIVE_FILES is not set")
	}
	data, err := os.ReadFile(list)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for l := range strings.Lines(string(data)) {
		if p := strings.TrimSpace(l); p != "" {
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		t.Fatalf("%s lists no paths", list)
	}
	c, err := NewFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	c = c.WithTool("transcriptdb-live")
	for i, path := range paths {
		t.Run(fmt.Sprintf("%02d_%s", i+1, filepath.Base(path)), func(t *testing.T) {
			liveCompare(t, c, path)
		})
	}
}

func liveCompare(t *testing.T, c *Client, path string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	disk, err := os.Open(path)
	if err != nil {
		t.Fatalf("%s: not on disk: %v", path, err)
	}
	defer disk.Close()
	files, err := c.FilesByPath(ctx, path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if len(files) != 1 {
		t.Fatalf("%s: %d rows in files, want 1", path, len(files))
	}
	f := files[0]
	how := "OpenRaw"
	var src io.ReadCloser
	if f.NeedsDiskRead() {
		how = "OpenLoaded" // loaded in part: the lines the database has
		src, err = c.OpenLoaded(ctx, f)
	} else {
		src, err = c.OpenRaw(ctx, f)
	}
	if err != nil {
		t.Fatalf("%s: %s: %v", path, how, err)
	}
	defer src.Close()
	n, err := equalStreams(src, io.LimitReader(disk, int64(f.Ingested)))
	if err != nil {
		t.Fatalf("%s (%s, %d lines, ingested %d of %d bytes, dirty %d): %v", path, how, f.Lines, f.Ingested, f.Size, f.Dirty, err)
	}
	if n != int64(f.Ingested) {
		t.Fatalf("%s: %d bytes compared, the database has loaded %d", path, n, f.Ingested)
	}
	t.Logf("%s: %s %d lines, %d bytes equal (file size %d)", path, how, f.Lines, n, f.Size)
}

// equalStreams compares db with disk byte for byte to their ends and returns the length of the equal part. The
// error names the offset of the first difference or which stream ended first.
func equalStreams(db, disk io.Reader) (int64, error) {
	a, b := bufio.NewReaderSize(db, 1<<20), bufio.NewReaderSize(disk, 1<<20)
	bufA, bufB := make([]byte, 1<<16), make([]byte, 1<<16)
	var off int64
	for {
		na, errA := io.ReadFull(a, bufA)
		nb, errB := io.ReadFull(b, bufB)
		if errA != nil && !errors.Is(errA, io.EOF) && !errors.Is(errA, io.ErrUnexpectedEOF) {
			return off, fmt.Errorf("database stream: %w", errA)
		}
		if errB != nil && !errors.Is(errB, io.EOF) && !errors.Is(errB, io.ErrUnexpectedEOF) {
			return off, fmt.Errorf("disk: %w", errB)
		}
		m := min(na, nb)
		if i := firstDiff(bufA[:m], bufB[:m]); i >= 0 {
			return off + int64(i), fmt.Errorf("differs at byte %d", off+int64(i))
		}
		off += int64(m)
		if na != nb {
			if na < nb {
				return off, fmt.Errorf("the database stream ends at byte %d, the file goes on", off)
			}
			return off, fmt.Errorf("the file ends at byte %d before the loaded length", off)
		}
		if errA != nil || errB != nil {
			return off, nil
		}
	}
}

func firstDiff(a, b []byte) int {
	if bytes.Equal(a, b) {
		return -1
	}
	for i := range a {
		if a[i] != b[i] {
			return i
		}
	}
	return -1
}

func TestEqualStreams(t *testing.T) {
	big := bytes.Repeat([]byte("0123456789"), 20000)
	changed := bytes.Clone(big)
	changed[123457] = 'x'
	for _, tc := range []struct {
		db, disk []byte
		n        int64
		ok       bool
	}{
		{big, big, int64(len(big)), true},
		{nil, nil, 0, true},
		{big, changed, 123457, false},
		{big[:100], big, 100, false},
		{big, big[:70000], 70000, false},
	} {
		n, err := equalStreams(bytes.NewReader(tc.db), bytes.NewReader(tc.disk))
		if n != tc.n || (err == nil) != tc.ok {
			t.Errorf("db %d disk %d bytes: %d %v, want %d ok %v", len(tc.db), len(tc.disk), n, err, tc.n, tc.ok)
		}
	}
}
