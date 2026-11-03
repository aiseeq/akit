package main

import (
	"errors"
	"fmt"
	"io/fs"
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
// ticket gets fileMode, an existing one keeps its mode.
func writeTask(path, body string) error {
	mode := fs.FileMode(fileMode)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	// The leading dot keeps the temporary file out of fileRE, so a concurrent ls never lists it.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tk-*.tmp")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err = writeTemp(tmp, body, mode); err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		if rerr := os.Remove(tmp.Name()); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			err = errors.Join(err, rerr)
		}
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func writeTemp(tmp *os.File, body string, mode fs.FileMode) error {
	_, err := tmp.WriteString(body)
	if err == nil {
		err = tmp.Chmod(mode)
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
