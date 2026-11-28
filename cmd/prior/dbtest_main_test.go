package main

import (
	"fmt"
	"os"
	"testing"

	"github.com/aiseeq/akit/internal/dbtest"
)

// testCacheEnv marks a cache directory made by TestMain, so that a child test process (help checks re-run the
// test binary) uses the parent's directory instead of creating one it would leave behind on os.Exit.
const testCacheEnv = "AKIT_PRIOR_TEST_CACHE_DIR"

// TestMain keeps tests that do not choose a cache directory away from the user's prior cache: a synthetic
// report index written there would make the next real call rebuild the index of every report.
func TestMain(m *testing.M) {
	if dir := os.Getenv(testCacheEnv); dir != "" && os.Getenv("AKIT_PRIOR_CACHE_DIR") == dir {
		dbtest.Main(m)
		return
	}
	dir, err := os.MkdirTemp("", "prior-test-cache-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	for _, name := range []string{"AKIT_PRIOR_CACHE_DIR", testCacheEnv} {
		if err := os.Setenv(name, dir); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
	dbtest.Main(cleanupAfter{m, dir})
}

type cleanupAfter struct {
	m   *testing.M
	dir string
}

func (c cleanupAfter) Run() int {
	code := c.m.Run()
	if err := os.RemoveAll(c.dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	return code
}
