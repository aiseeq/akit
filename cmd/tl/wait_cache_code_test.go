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

// waitCacheListFormat — поля go list для хеша: модуль, его замена (replace, go.work) и файлы пакета под любые
// ОС и теги сборки (IgnoredGoFiles), со встроенными (embed), C-, ассемблерными и .syso-файлами.
const waitCacheListFormat = `{{if .Module}}{{.Module.Path}}	{{.Module.Version}}	{{.Module.Dir}}	` +
	`{{with .Module.Replace}}{{.Path}}	{{.Version}}	{{.Dir}}{{else}}		{{end}}	{{.Dir}}	` +
	`{{join .GoFiles ","}},{{join .CgoFiles ","}},{{join .IgnoredGoFiles ","}},{{join .EmbedFiles ","}},` +
	`{{join .CFiles ","}},{{join .HFiles ","}},{{join .SFiles ","}},{{join .SysoFiles ","}}{{end}}`

// waitCacheSourcesHash — хеш кода, который пишет запись кэша. Пакет из исходников на диске (модуль akit, замена
// на папку) входит файлами: путь от корня модуля и содержимое; пакет из версии модуля — строкой path@version
// с заменой. Сам файл хеша и тесты в него не входят.
func waitCacheSourcesHash(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "list", "-deps", "-f", waitCacheListFormat, ".").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	hash := sha256.New()
	// go.mod модуля tl: директива go и требования меняют сборку при тех же исходниках.
	gomod, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		t.Fatalf("go env GOMOD: %v", err)
	}
	data, err := os.ReadFile(strings.TrimSpace(string(gomod)))
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(hash, "go.mod %d\n", len(data))
	hash.Write(data)
	for line := range strings.SplitSeq(strings.TrimRight(string(out), "\n"), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 8 {
			t.Fatalf("go list line %q", line)
		}
		modPath, modVersion, modDir, replPath, replVersion, replDir, dir, files := f[0], f[1], f[2], f[3], f[4], f[5], f[6], f[7]
		if replPath != "" && replVersion != "" || replPath == "" && modVersion != "" {
			fmt.Fprintf(hash, "module %s@%s => %s@%s\n", modPath, modVersion, replPath, replVersion)
			continue
		}
		root := modDir
		if replPath != "" {
			root = replDir
		}
		rel, err := filepath.Rel(root, dir)
		if err != nil {
			t.Fatal(err)
		}
		for name := range strings.SplitSeq(files, ",") {
			if name == "" || strings.HasSuffix(name, "_test.go") || modPath == "github.com/aiseeq/akit" && rel == filepath.Join("cmd", "tl") && name == waitCacheCodeFile {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(hash, "file %s/%s %d\n", modPath, filepath.ToSlash(filepath.Join(rel, name)), len(data))
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
