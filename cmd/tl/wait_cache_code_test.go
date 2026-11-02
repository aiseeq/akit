package main

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

const waitCacheCodeFile = "wait_cache_code.go"

// waitCacheSourcesHash — хеш кода, который пишет запись кэша: файлы пакетов модуля (путь от корня модуля и
// содержимое) и path@version внешних модулей. Сам файл хеша в него не входит.
func waitCacheSourcesHash(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "list", "-deps", "-f",
		`{{if .Module}}{{.Module.Path}}|{{.Module.Version}}|{{.Module.Dir}}|{{.Dir}}|{{join .GoFiles ","}}{{end}}`, ".").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	hash := sha256.New()
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "|")
		if len(f) != 5 {
			t.Fatalf("go list line %q", line)
		}
		if f[1] != "" {
			fmt.Fprintf(hash, "module %s@%s\n", f[0], f[1])
			continue
		}
		rel, err := filepath.Rel(f[2], f[3])
		if err != nil {
			t.Fatal(err)
		}
		for name := range strings.SplitSeq(f[4], ",") {
			if rel == filepath.Join("cmd", "tl") && name == waitCacheCodeFile {
				continue
			}
			data, err := os.ReadFile(filepath.Join(f[3], name))
			if err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(hash, "file %s %d\n", filepath.ToSlash(filepath.Join(rel, name)), len(data))
			hash.Write(data)
		}
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// Хеш в waitCacheCode совпадает с исходниками: правка кода, от которого зависит запись, без нового хеша отдала
// бы старые записи как свои. UPDATE_WAIT_CACHE_CODE=1 переписывает константу.
func TestWaitCacheCodeMatchesSources(t *testing.T) {
	want := waitCacheSourcesHash(t)
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
