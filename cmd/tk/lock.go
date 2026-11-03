package main

import (
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"syscall"
)

// lockTasks takes the exclusive lock of a task folder and returns its release. Every read-modify-write
// of a ticket holds it from the read to the rename, so two tk processes never write from the same stale
// copy. The lock sits on the folder itself: no lock file appears among the tickets.
func lockTasks(dir string) (func(), error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, fmt.Errorf("lock task folder: %w", err)
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		if cerr := f.Close(); cerr != nil {
			err = errors.Join(err, cerr)
		}
		return nil, fmt.Errorf("lock task folder %s: %w", dir, err)
	}
	// Closing the folder drops the lock; a failed close leaves nothing to undo.
	return func() { _ = f.Close() }, nil
}

// writeTask replaces a ticket in one rename: a reader sees the old or the new text, never a mix. A new
// ticket gets fileMode less the umask, as a plain create would; an existing one keeps its mode. A symlinked
// ticket is written at its target, so the link stays; a ticket with several hard links is refused, since a
// rename would leave the other names on the old text.
func writeTask(path, body string) error {
	target, keep, err := taskTarget(path)
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	tmp, err := createTemp(filepath.Dir(target))
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err = writeTemp(tmp, body, keep); err == nil {
		err = os.Rename(tmp.Name(), target)
	}
	if err != nil {
		if rerr := os.Remove(tmp.Name()); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			err = errors.Join(err, rerr)
		}
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// taskTarget resolves the file a write replaces and the mode to keep: zero for a new ticket, whose mode the
// create already set.
func taskTarget(path string) (string, fs.FileMode, error) {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return path, 0, nil
	}
	if err != nil {
		return "", 0, fmt.Errorf("stat ticket: %w", err)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
		return "", 0, fmt.Errorf("the ticket has %d hard links; a rename would split them", st.Nlink)
	}
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", 0, fmt.Errorf("resolve ticket link: %w", err)
	}
	return target, info.Mode().Perm(), nil
}

// createTemp makes the temporary file with fileMode, so the umask applies as for a plain create. The leading
// dot keeps it out of fileRE: a concurrent ls never lists it.
func createTemp(dir string) (*os.File, error) {
	for range 10 {
		name := filepath.Join(dir, fmt.Sprintf(".tk-%d-%d.tmp", os.Getpid(), rand.Uint64()))
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
		if !errors.Is(err, fs.ErrExist) {
			return f, err
		}
	}
	return nil, errors.New("no free temporary name after 10 tries")
}

func writeTemp(tmp *os.File, body string, keep fs.FileMode) error {
	_, err := tmp.WriteString(body)
	if err == nil && keep != 0 {
		err = tmp.Chmod(keep)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	return err
}

// lockForCommand locks the task folder for a command that rewrites one ticket. show and check only read,
// and done runs its long checks first and locks for the final compare and write itself (checkDone). A
// missing folder has no tickets to guard: find then reports the ticket as not found.
func lockForCommand(dir, cmd string) (func(), error) {
	switch cmd {
	case "show", "check", "done":
		return func() {}, nil
	}
	release, err := lockTasks(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return func() {}, nil
	}
	return release, err
}
