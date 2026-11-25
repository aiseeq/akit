package main

import (
	"os"
	"regexp"
	"testing"

	"github.com/aiseeq/akit/internal/codehash"
)

const waitCacheCodeFile = "wait_cache_code.go"

// The hash covers source used to write tl --wait cache entries. The shared
// implementation excludes tests and this generated constant file.
func TestWaitCacheCodeMatchesSources(t *testing.T) {
	want, err := codehash.Hash(".", waitCacheCodeFile)
	if err != nil {
		t.Fatal(err)
	}
	if waitCacheCode == want {
		return
	}
	if os.Getenv("UPDATE_WAIT_CACHE_CODE") == "1" {
		data, err := os.ReadFile(waitCacheCodeFile)
		if err != nil {
			t.Fatal(err)
		}
		re := regexp.MustCompile(`const waitCacheCode = "[^"]*"`)
		if !re.Match(data) {
			t.Fatalf("%s: no waitCacheCode", waitCacheCodeFile)
		}
		if err := os.WriteFile(waitCacheCodeFile, re.ReplaceAll(data, []byte(`const waitCacheCode = "`+want+`"`)), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("waitCacheCode updated to %s", want)
		return
	}
	t.Fatalf("waitCacheCode %q is stale, sources hash to %q: run UPDATE_WAIT_CACHE_CODE=1 go test ./cmd/tl -run TestWaitCacheCodeMatchesSources", waitCacheCode, want)
}
