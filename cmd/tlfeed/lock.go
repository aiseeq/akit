package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// errLocked — another loader holds the lock of the database: this one ends at once.
type errLocked struct{ msg string }

func (e *errLocked) Error() string { return e.msg }

// lockDir — where the lock files are: beside the state file, or, without one, in the runtime folder of the user.
func lockDir(state string) string {
	if state != "" {
		return filepath.Dir(state)
	}
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "akit", "tlfeed")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("akit-tlfeed-%d", os.Getuid()))
}

// serverID — the UUID of the server, the same through any address or proxy in front of it.
func serverID(ctx context.Context, srv *server) (string, error) {
	out, err := srv.query(ctx, "SELECT serverUUID()", time.Minute)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return "", fmt.Errorf("server answered no serverUUID()")
	}
	return id, nil
}

// lockPath — the lock file of a database on a server.
func lockPath(dir, server, db string) string {
	return filepath.Join(dir, "tlfeed-"+db+"-"+server+".lock")
}

// takeLock holds the lock of one database on one server for the life of the process: two loaders on the same
// database would undo each other's generations. Taken, the file holds this pid; held by another, the error names
// its pid.
func takeLock(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("lock folder: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("lock: %w", err)
	}
	busy, err := tryLock(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	if busy {
		pid, err := os.ReadFile(path)
		f.Close()
		holder := strings.TrimSpace(string(pid))
		switch {
		case err != nil:
			holder = "unknown (" + err.Error() + ")"
		case holder == "":
			holder = "unknown"
		}
		return nil, &errLocked{fmt.Sprintf("another loader works on this database: pid %s holds %s", holder, path)}
	}
	if err := writePid(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return f, nil
}

func writePid(f *os.File) error {
	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("truncate: %w", err)
	}
	if _, err := f.WriteAt(fmt.Appendf(nil, "%d\n", os.Getpid()), 0); err != nil {
		return fmt.Errorf("write pid: %w", err)
	}
	return nil
}
