package codehash

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type fixture struct {
	root      string
	pkg       string
	depA      string
	depB      string
	toolchain string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	if err := os.MkdirAll("/var/tmp/hash-foreign/run", 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("/var/tmp/hash-foreign/run", "codehash-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOTOOLCHAIN", "local")
	pkg := filepath.Join(root, "pkg")
	depA := filepath.Join(root, "dep-a")
	depB := filepath.Join(root, "dep-b")
	unused := filepath.Join(root, "unused")
	for _, dir := range []string{pkg, depA, depB, unused} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/root\n\ngo 1.26.0\n\nrequire example.com/dep v0.0.0\n\nreplace (\n\texample.com/dep => ./dep-a\n\texample.com/unused => ./unused\n)\n")
	writeFile(t, filepath.Join(pkg, "pkg.go"), "package pkg\n\nimport \"example.com/dep\"\n\nvar Value = dep.Value\n")
	writeFile(t, filepath.Join(pkg, "const.go"), "package pkg\n\nconst code = \"one\"\n")
	writeFile(t, filepath.Join(pkg, "pkg_test.go"), "package pkg\n\nvar _ = 1\n")
	for _, dir := range []string{depA, depB} {
		writeFile(t, filepath.Join(dir, "go.mod"), "module example.com/dep\n\ngo 1.26.0\n")
		writeFile(t, filepath.Join(dir, "dep.go"), "package dep\n\nconst Value = 1\n")
	}
	writeFile(t, filepath.Join(unused, "go.mod"), "module example.com/unused\n\ngo 1.26.0\n")
	out, err := exec.Command("go", "env", "GOVERSION").Output()
	if err != nil {
		t.Fatal(err)
	}
	toolchain := strings.SplitN(strings.TrimSpace(string(out)), "-", 2)[0]
	return fixture{root: root, pkg: pkg, depA: depA, depB: depB, toolchain: toolchain}
}

func (f fixture) hash(t *testing.T) string {
	t.Helper()
	h, err := Hash(f.pkg, "const.go")
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHashIgnoresUnrelatedRequire(t *testing.T) {
	f := newFixture(t)
	mod := filepath.Join(f.root, "go.mod")
	data, err := os.ReadFile(mod)
	if err != nil {
		t.Fatal(err)
	}
	want := f.hash(t)
	withRequire := string(data) + "\nrequire example.com/unused v1.2.3\n"
	writeFile(t, mod, withRequire)
	if got := f.hash(t); got != want {
		t.Fatalf("adding an unrelated require changed hash: got %s, want %s", got, want)
	}
	writeFile(t, mod, strings.Replace(withRequire, "v1.2.3", "v1.2.4", 1))
	if got := f.hash(t); got != want {
		t.Fatalf("changing an unrelated require version changed hash: got %s, want %s", got, want)
	}
}

func TestHashIncludesBuildDirectives(t *testing.T) {
	tests := []struct {
		name string
		edit func(*testing.T, fixture)
	}{
		{"go", func(t *testing.T, f fixture) { runModEdit(t, f.root, "-go=1.26.1") }},
		{"toolchain", func(t *testing.T, f fixture) { runModEdit(t, f.root, "-toolchain="+f.toolchain) }},
		{"godebug", func(t *testing.T, f fixture) { runModEdit(t, f.root, "-godebug=asynctimerchan=0") }},
		{"replace", func(t *testing.T, f fixture) { runModEdit(t, f.root, "-replace=example.com/dep=./dep-b") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			before := f.hash(t)
			tt.edit(t, f)
			if after := f.hash(t); after == before {
				t.Fatalf("changing %s did not change hash %s", tt.name, after)
			}
		})
	}
}

func runModEdit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("go", append([]string{"mod", "edit"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go mod edit: %v: %s", err, out)
	}
}

func TestHashIncludesPackageAndDependencySources(t *testing.T) {
	for _, path := range []string{"pkg/pkg.go", "dep-a/dep.go"} {
		t.Run(path, func(t *testing.T) {
			f := newFixture(t)
			before := f.hash(t)
			file := filepath.Join(f.root, filepath.FromSlash(path))
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, file, string(data)+"\n// changed\n")
			if after := f.hash(t); after == before {
				t.Fatalf("changing %s did not change hash %s", path, after)
			}
		})
	}
}

func TestHashIgnoresTestsAndExcludedFile(t *testing.T) {
	f := newFixture(t)
	want := f.hash(t)
	writeFile(t, filepath.Join(f.pkg, "pkg_test.go"), "package pkg\n\nvar _ = 2\n")
	writeFile(t, filepath.Join(f.pkg, "const.go"), "package pkg\n\nconst code = \"two\"\n")
	if got := f.hash(t); got != want {
		t.Fatalf("test or excluded file changed hash: got %s, want %s", got, want)
	}
}
