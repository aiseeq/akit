// Package dbtest keeps the tests of the tools that read the transcript database away from the database of the
// machine that runs them: tools read it by default (TL_BACKEND unset), so a test that does not name its own
// database would otherwise depend on the loader state and server of whoever runs it.
package dbtest

import (
	"fmt"
	"net"
	"os"
)

// Isolate points the loader state at an empty folder and the database address at a port nothing listens at, for
// the whole test binary; a test that needs a database sets its own. The returned function removes the folder.
func Isolate() (func(), error) {
	dir, err := os.MkdirTemp("", "dbtest-state-")
	if err != nil {
		return nil, fmt.Errorf("dbtest: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("dbtest: %w", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		cleanup()
		return nil, fmt.Errorf("dbtest: %w", err)
	}
	for k, v := range map[string]string{"XDG_STATE_HOME": dir, "TL_CLICKHOUSE_URL": "http://" + addr} {
		if err := os.Setenv(k, v); err != nil {
			cleanup()
			return nil, fmt.Errorf("dbtest: %w", err)
		}
	}
	return cleanup, nil
}
