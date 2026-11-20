package main

import (
	"os"
	"regexp"
	"testing"

	"github.com/aiseeq/akit/internal/cachecode"
)

func TestBriefCacheCodeMatchesSources(t *testing.T) {
	want := cachecode.SourcesHash(t, "brief_cache_code.go")
	if briefCacheCode == want {
		return
	}
	if os.Getenv("UPDATE_BRIEF_CACHE_CODE") == "1" {
		data, err := os.ReadFile("brief_cache_code.go")
		if err != nil {
			t.Fatal(err)
		}
		re := regexp.MustCompile(`const briefCacheCode = "[^"]*"`)
		if !re.Match(data) {
			t.Fatal("no briefCacheCode")
		}
		if err := os.WriteFile("brief_cache_code.go", re.ReplaceAll(data, []byte(`const briefCacheCode = "`+want+`"`)), 0644); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatalf("briefCacheCode %q is stale, want %q; run UPDATE_BRIEF_CACHE_CODE=1 go test ./cmd/user-word -run TestBriefCacheCodeMatchesSources", briefCacheCode, want)
}
