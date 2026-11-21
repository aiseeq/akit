package main

import (
	"os"
	"regexp"
	"testing"

	"github.com/aiseeq/akit/internal/cachecode"
)

const waitCacheCodeFile = "wait_cache_code.go"

// Хеш в waitCacheCode совпадает с исходниками: правка кода, от которого зависит запись, без нового хеша отдала
// бы старые записи как свои. UPDATE_WAIT_CACHE_CODE=1 переписывает константу.
func TestWaitCacheCodeMatchesSources(t *testing.T) {
	want := cachecode.SourcesHash(t, waitCacheCodeFile)
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
			t.Fatalf("%s: no waitCacheCode constant", waitCacheCodeFile)
		}
		if err := os.WriteFile(waitCacheCodeFile, re.ReplaceAll(data, []byte(`const waitCacheCode = "`+want+`"`)), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("waitCacheCode updated to %s", want)
		return
	}
	t.Fatalf("waitCacheCode %q is stale, sources hash to %q: run UPDATE_WAIT_CACHE_CODE=1 go test ./cmd/tl -run TestWaitCacheCodeMatchesSources", waitCacheCode, want)
}
