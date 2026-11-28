//go:build linux

package main

import (
	"encoding/binary"
	"os"
	"syscall"
)

type dirEntry struct {
	name string
	dir  bool
}

// pathError is the error of a raw system call on path, worded as the os package words it.
func pathError(op, path string, err error) error {
	return &os.PathError{Op: op, Path: path, Err: err}
}

// openRaw opens path read-only with a raw descriptor, retrying an interrupted call. The error is the bare errno.
func openRaw(path string, flags int) (int, error) {
	for {
		fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|flags, 0)
		if err != syscall.EINTR {
			return fd, err
		}
	}
}

// readDirEntries lists a directory with raw system calls: the report tree has thousands of directories, and
// os.File adds five system calls per directory for the runtime poller. Errors read as those of os.File.ReadDir.
func readDirEntries(dir string) ([]dirEntry, []error) {
	fd, err := openRaw(dir, syscall.O_DIRECTORY)
	if err == syscall.ENOTDIR {
		// os.Open succeeds on a file, and reading it as a directory fails.
		return nil, []error{pathError("readdirent", dir, err)}
	}
	if err != nil {
		return nil, []error{pathError("open", dir, err)}
	}
	var entries []dirEntry
	var errs []error
	buf := make([]byte, 32<<10)
	for {
		n, err := syscall.ReadDirent(fd, buf)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			errs = append(errs, pathError("readdirent", dir, err))
			break
		}
		if n <= 0 {
			break
		}
		var parseErrs []error
		entries, parseErrs = appendDirents(entries, dir, buf[:n])
		errs = append(errs, parseErrs...)
	}
	if err := syscall.Close(fd); err != nil {
		errs = append(errs, pathError("close", dir, err))
	}
	return entries, errs
}

// appendDirents parses linux_dirent64 records: inode (8), offset (8), record length (2), type (1), name. An entry
// of unknown type is classified by lstat; as in os.ReadDir, one that is gone by then is skipped, and another
// lstat error is returned with the entry left out.
func appendDirents(entries []dirEntry, dir string, buf []byte) ([]dirEntry, []error) {
	var errs []error
	for len(buf) >= 19 {
		reclen := int(binary.NativeEndian.Uint16(buf[16:18]))
		if reclen < 19 || reclen > len(buf) {
			break
		}
		inode := binary.NativeEndian.Uint64(buf[:8])
		typ := buf[18]
		name := buf[19:reclen]
		for i, c := range name {
			if c == 0 {
				name = name[:i]
				break
			}
		}
		buf = buf[reclen:]
		if inode == 0 || string(name) == "." || string(name) == ".." {
			continue // a removed entry, or the directory itself and its parent
		}
		entry := dirEntry{name: string(name), dir: typ == syscall.DT_DIR}
		if typ == syscall.DT_UNKNOWN {
			var st syscall.Stat_t
			err := syscall.Lstat(dir+"/"+entry.name, &st)
			for err == syscall.EINTR {
				err = syscall.Lstat(dir+"/"+entry.name, &st)
			}
			if err == syscall.ENOENT {
				continue
			}
			if err != nil {
				errs = append(errs, pathError("lstat", dir+"/"+entry.name, err))
				continue
			}
			entry.dir = st.Mode&syscall.S_IFMT == syscall.S_IFDIR
		}
		entries = append(entries, entry)
	}
	return entries, errs
}

// readFileRaw is os.ReadFile without the runtime poller (see readDirEntries). An open or read failure reads as in
// os.ReadFile; a failed fstat of the open file is returned too, where os.ReadFile would read without a size hint.
func readFileRaw(path string) ([]byte, error) {
	fd, err := openRaw(path, 0)
	if err != nil {
		return nil, pathError("open", path, err)
	}
	defer syscall.Close(fd)
	source := &runSource{fd: fd, path: path}
	if err := source.fstat(&source.stat); err != nil {
		return nil, err
	}
	return source.readAll(nil, -1)
}
