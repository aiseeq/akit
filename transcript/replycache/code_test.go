package replycache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func sourcesHash(t *testing.T) string {
	t.Helper()
	hash := sha256.New()
	for _, dir := range []string{"..", "."} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		sort.Strings(files)
		for _, path := range files {
			name := filepath.Base(path)
			if strings.HasSuffix(name, "_test.go") || dir == "." && name == "code.go" {
				continue
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(hash, "%s/%s %d\n", dir, name, len(data))
			hash.Write(data)
		}
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func TestCacheCodeMatchesSources(t *testing.T) {
	want := sourcesHash(t)
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
