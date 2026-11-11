package replycache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const listFormat = `{{if .Module}}{{.Module.Path}}\t{{.Module.Version}}\t{{.Module.Dir}}\t` +
	`{{with .Module.Replace}}{{.Path}}\t{{.Version}}\t{{.Dir}}{{else}}\t\t{{end}}\t{{.Dir}}\t` +
	`{{join .GoFiles ","}},{{join .CgoFiles ","}},{{join .IgnoredGoFiles ","}},{{join .EmbedFiles ","}},` +
	`{{join .CFiles ","}},{{join .HFiles ","}},{{join .SFiles ","}},{{join .SysoFiles ","}}{{end}}`

func sourcesHash(t *testing.T) string {
	t.Helper()
	format := strings.ReplaceAll(listFormat, `\t`, "\t")
	out, err := exec.Command("go", "list", "-deps", "-f", format, ".").Output()
	if err != nil {
		t.Fatal(err)
	}
	gomod, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(strings.TrimSpace(string(gomod)))
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	fmt.Fprintf(hash, "go.mod %d\n", len(data))
	hash.Write(data)
	for line := range strings.SplitSeq(strings.TrimRight(string(out), "\n"), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 8 {
			t.Fatalf("go list: %q", line)
		}
		mod, version, root, replacement, replacementVersion, replacementDir, dir, files := fields[0], fields[1], fields[2], fields[3], fields[4], fields[5], fields[6], fields[7]
		if replacement != "" && replacementVersion != "" || replacement == "" && version != "" {
			fmt.Fprintf(hash, "module %s@%s => %s@%s\n", mod, version, replacement, replacementVersion)
			continue
		}
		if replacement != "" {
			root = replacementDir
		}
		rel, err := filepath.Rel(root, dir)
		if err != nil {
			t.Fatal(err)
		}
		for name := range strings.SplitSeq(files, ",") {
			if name == "" || strings.HasSuffix(name, "_test.go") || mod == "github.com/aiseeq/akit" && rel == filepath.Join("transcript", "replycache") && name == "code.go" {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(hash, "file %s/%s %d\n", mod, filepath.ToSlash(filepath.Join(rel, name)), len(data))
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
