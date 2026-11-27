package main

import (
	"fmt"
	"os"
	"testing"

	"github.com/aiseeq/akit/internal/dbtest"
)

func TestMain(m *testing.M) {
	cleanup, err := dbtest.Isolate()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	code := m.Run()
	cleanup()
	os.Exit(code)
}
