package replycache

import (
	"os"
	"regexp"
	"testing"

	"github.com/aiseeq/akit/internal/cachecode"
)

func TestCacheCodeMatchesSources(t *testing.T) {
	want := cachecode.SourcesHash(t, "code.go")
	if cacheCode == want {
		return
	}
	if os.Getenv("UPDATE_REPLY_CACHE_CODE") == "1" {
		data, err := os.ReadFile("code.go")
		if err != nil {
			t.Fatal(err)
		}
		re := regexp.MustCompile(`const cacheCode = "[^"]*"`)
		if !re.Match(data) {
			t.Fatal("no cacheCode")
		}
		if err := os.WriteFile("code.go", re.ReplaceAll(data, []byte(`const cacheCode = "`+want+`"`)), 0644); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatalf("cacheCode %q is stale, want %q; run UPDATE_REPLY_CACHE_CODE=1 go test ./transcript/replycache -run TestCacheCodeMatchesSources", cacheCode, want)
}
