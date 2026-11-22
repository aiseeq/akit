package transcript

import (
	"bytes"
	"errors"
	"io"
	"os"
)

const recordReadWindow = (64 << 10) + (4 << 10)

// RecordAt returns the start offset, up to 4 KiB of the JSONL record prefix, and the end offset
// for the record containing offset. It reads around the requested offset instead of scanning
// from the beginning of the file.
func RecordAt(f *os.File, offset, size int64) (int64, []byte, int64, error) {
	if offset >= size {
		return size, nil, size, nil
	}
	var chunk [recordReadWindow]byte
	pos := offset
	var lineData []byte
	for {
		start := max(int64(0), pos-(64<<10))
		readSize := min(int64(len(chunk)), size-start)
		n, err := f.ReadAt(chunk[:readSize], start)
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, nil, 0, err
		}
		boundary := min(int64(n), pos-start)
		if i := bytes.LastIndexByte(chunk[:boundary], '\n'); i >= 0 {
			lineData = chunk[i+1 : n]
			pos = start + int64(i+1)
			break
		}
		if start == 0 {
			lineData = chunk[:n]
			pos = 0
			break
		}
		if n == 0 {
			return size, nil, size, nil
		}
		pos = start
	}
	// Clamp to this record before copying: the read window may include following records.
	lineEnd := bytes.IndexByte(lineData, '\n')
	if lineEnd >= 0 {
		lineData = lineData[:lineEnd]
	}
	prefix := bytes.Clone(lineData[:min(len(lineData), 4096)])
	if lineEnd >= 0 {
		return pos, prefix, pos + int64(lineEnd+1), nil
	}
	end := pos + int64(len(lineData))
	for end < size {
		n, err := f.ReadAt(chunk[:], end)
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, nil, 0, err
		}
		if i := bytes.IndexByte(chunk[:n], '\n'); i >= 0 {
			return pos, prefix, end + int64(i+1), nil
		}
		if n == 0 {
			break
		}
		end += int64(n)
	}
	return pos, prefix, size, nil
}
