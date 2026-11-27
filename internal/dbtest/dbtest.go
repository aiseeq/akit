// Package dbtest keeps the tests of the tools that read the transcript database away from the database of the
// machine that runs them: tools read it by default (TL_BACKEND unset), so a test that does not name its own
// database would otherwise depend on the loader state and server of whoever runs it.
package dbtest

import (
	"errors"
	"fmt"
	"net"
	"os"
)

// Isolate points the loader state at an empty folder and the database address at a port nothing listens at, for
// the whole test binary; a test that needs a database sets its own. The returned function removes the folder.
func Isolate() (func() error, error) {
	dir, err := os.MkdirTemp("", "dbtest-state-")
	if err != nil {
		return nil, fmt.Errorf("dbtest: %w", err)
	}
	cleanup := func() error { return os.RemoveAll(dir) }
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, errors.Join(fmt.Errorf("dbtest: %w", err), cleanup())
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		return nil, errors.Join(fmt.Errorf("dbtest: %w", err), cleanup())
	}
	for k, v := range map[string]string{"XDG_STATE_HOME": dir, "TL_CLICKHOUSE_URL": "http://" + addr, "TL_BACKEND": ""} {
		if err := os.Setenv(k, v); err != nil {
			return nil, errors.Join(fmt.Errorf("dbtest: %w", err), cleanup())
		}
	}
	return cleanup, nil
}

// Main runs the tests of a package under Isolate and exits with their code.
func Main(m interface{ Run() int }) {
	cleanup, err := Isolate()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	code := m.Run()
	if err := cleanup(); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(code)
}
