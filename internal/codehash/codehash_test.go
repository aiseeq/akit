package codehash

import (
	"archive/zip"
	"fmt"
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
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOTOOLCHAIN", "local")
	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "-modcacherw")
	toolchain := goVersion(t)
	goVersion := previousGoVersion(t, toolchain)
	root := t.TempDir()
	pkg := filepath.Join(root, "pkg")
	depA := filepath.Join(root, "dep-a")
	depB := filepath.Join(root, "dep-b")
	unused := filepath.Join(root, "unused")
	for _, dir := range []string{pkg, depA, depB, unused} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(root, "go.mod"), fmt.Sprintf("module example.com/root\n\ngo %s\n\nrequire example.com/dep v0.0.0\n\nreplace (\n\texample.com/dep => ./dep-a\n\texample.com/unused => ./unused\n)\n", goVersion))
	writeFile(t, filepath.Join(pkg, "pkg.go"), "package pkg\n\nimport (\n\t_ \"embed\"\n\t\"example.com/dep\"\n)\n\n//go:embed data.txt\nvar data string\n\nvar Value = dep.Value\n")
	writeFile(t, filepath.Join(pkg, "const.go"), "package pkg\n\nconst code = \"one\"\n")
	writeFile(t, filepath.Join(pkg, "tagged.go"), "//go:build codehash_review\n\npackage pkg\n\nconst Tagged = 1\n")
	writeFile(t, filepath.Join(pkg, "tagged_test.go"), "//go:build codehash_review_test\n\npackage pkg\n\nvar _ = 1\n")
	writeFile(t, filepath.Join(pkg, "data.txt"), "embedded data\n")
	for _, dir := range []string{depA, depB} {
		writeFile(t, filepath.Join(dir, "go.mod"), fmt.Sprintf("module example.com/dep\n\ngo %s\n", goVersion))
		writeFile(t, filepath.Join(dir, "dep.go"), "package dep\n\nconst Value = 1\n")
		writeFile(t, filepath.Join(dir, "const.go"), "package dep\n\nconst code = \"one\"\n")
	}
	writeFile(t, filepath.Join(unused, "go.mod"), fmt.Sprintf("module example.com/unused\n\ngo %s\n", goVersion))
	return fixture{root: root, pkg: pkg, depA: depA, depB: depB, toolchain: toolchain}
}

func goVersion(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "env", "GOVERSION").Output()
	if err != nil {
		t.Fatal(err)
	}
	version := strings.TrimPrefix(strings.TrimSpace(string(out)), "go")
	return strings.SplitN(version, "-", 2)[0]
}

func previousGoVersion(t *testing.T, version string) string {
	t.Helper()
	var major, minor, patch int
	if _, err := fmt.Sscanf(version, "%d.%d.%d", &major, &minor, &patch); err != nil {
		t.Fatalf("parse Go version %q: %v", version, err)
	}
	if patch > 0 {
		patch--
	} else if minor > 0 {
		minor--
		patch = 0
	} else {
		t.Fatalf("cannot choose an earlier Go version than %q", version)
	}
	return fmt.Sprintf("%d.%d.%d", major, minor, patch)
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
		{"go", func(t *testing.T, f fixture) { runModEdit(t, f.root, "-go", f.toolchain) }},
		{"toolchain", func(t *testing.T, f fixture) { runModEdit(t, f.root, "-toolchain=go"+f.toolchain) }},
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

func TestHashIncludesGodebugValue(t *testing.T) {
	f := newFixture(t)
	runModEdit(t, f.root, "-godebug=asynctimerchan=0")
	before := f.hash(t)
	runModEdit(t, f.root, "-godebug=asynctimerchan=1")
	if after := f.hash(t); after == before {
		t.Fatalf("changing godebug value did not change hash %s", after)
	}
}

func TestCommandErrorIncludesStderr(t *testing.T) {
	cmd := exec.Command("sh", "-c", "echo diagnostic >&2; exit 1")
	if _, err := cmd.Output(); err == nil {
		t.Fatal("expected command failure")
	} else if got := commandError("command", err).Error(); !strings.Contains(got, "diagnostic") {
		t.Fatalf("command error %q does not include stderr", got)
	}
}

func runModEdit(t *testing.T, dir string, args ...string) {
	t.Helper()
	runGo(t, dir, append([]string{"mod", "edit"}, args...)...)
}

func runGo(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go %s: %v: %s", strings.Join(args, " "), err, out)
	}
}

func TestHashIncludesPackageAndDependencySources(t *testing.T) {
	tests := []struct {
		name string
		file string
		edit func(string) string
	}{
		{"package source", "pkg/pkg.go", func(s string) string { return strings.Replace(s, "dep.Value", "dep.Value+1", 1) }},
		{"same length source content", "dep-a/dep.go", func(s string) string { return strings.Replace(s, "Value = 1", "Value = 2", 1) }},
		{"same basename in dependency package", "dep-a/const.go", func(s string) string { return strings.Replace(s, `"one"`, `"two"`, 1) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			before := f.hash(t)
			file := filepath.Join(f.root, filepath.FromSlash(tt.file))
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			updated := tt.edit(string(data))
			if updated == string(data) {
				t.Fatalf("test did not alter %s", tt.file)
			}
			writeFile(t, file, updated)
			if after := f.hash(t); after == before {
				t.Fatalf("changing %s did not change hash %s", tt.file, after)
			}
		})
	}
}

func TestHashIncludesIgnoredGoAndEmbedFiles(t *testing.T) {
	tests := []struct {
		name string
		file string
	}{
		{"ignored build-tagged Go file", "tagged.go"},
		{"embed file", "data.txt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			before := f.hash(t)
			path := filepath.Join(f.pkg, tt.file)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, path, string(data)+"x")
			if after := f.hash(t); after == before {
				t.Fatalf("changing %s did not change hash %s", tt.file, after)
			}
		})
	}
}

func TestHashIgnoresTaggedTestAndExcludedFile(t *testing.T) {
	f := newFixture(t)
	want := f.hash(t)
	writeFile(t, filepath.Join(f.pkg, "tagged_test.go"), "//go:build codehash_review_test\n\npackage pkg\n\nvar _ = 2\n")
	writeFile(t, filepath.Join(f.pkg, "const.go"), "package pkg\n\nconst code = \"two\"\n")
	if got := f.hash(t); got != want {
		t.Fatalf("tagged test or excluded file changed hash: got %s, want %s", got, want)
	}
}

func TestHashIncludesDependencyModuleVersion(t *testing.T) {
	f := newFixture(t)
	proxy := newModuleProxy(t, f.root, "example.com/dep", "example.com/fork")
	useProxy(t, f.root, proxy)
	runModEdit(t, f.root, "-dropreplace=example.com/dep", "-require=example.com/dep@v1.0.0")
	runGo(t, f.root, "mod", "download", "example.com/dep@v1.0.0")
	before := f.hash(t)
	runModEdit(t, f.root, "-require=example.com/dep@v1.1.0")
	runGo(t, f.root, "mod", "download", "example.com/dep@v1.1.0")
	if after := f.hash(t); after == before {
		t.Fatalf("changing dependency module version did not change hash %s", after)
	}
}

func TestHashIncludesVersionedReplaceVersions(t *testing.T) {
	tests := []struct {
		name  string
		setup []string
		edit  []string
	}{
		{
			name:  "replacement version",
			setup: []string{"-replace=example.com/unused=example.com/fork@v1.0.0"},
			edit:  []string{"-replace=example.com/unused=example.com/fork@v1.1.0"},
		},
		{
			name:  "old version",
			setup: []string{"-replace=example.com/unused@v1.1.0=example.com/fork@v1.0.0"},
			edit: []string{
				"-dropreplace=example.com/unused@v1.1.0",
				"-replace=example.com/unused@v1.2.0=example.com/fork@v1.0.0",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			runModEdit(t, f.root, tt.setup...)
			before := f.hash(t)
			runModEdit(t, f.root, tt.edit...)
			if after := f.hash(t); after == before {
				t.Fatalf("changing %s did not change hash %s", tt.name, after)
			}
		})
	}
}

func newModuleProxy(t *testing.T, root string, modules ...string) string {
	t.Helper()
	proxy := filepath.Join(root, "proxy")
	for _, module := range modules {
		for _, version := range []string{"v1.0.0", "v1.1.0"} {
			versionDir := filepath.Join(proxy, filepath.FromSlash(module), "@v")
			if err := os.MkdirAll(versionDir, 0o755); err != nil {
				t.Fatal(err)
			}
			moduleFile := fmt.Sprintf("module %s\n\ngo %s\n", module, previousGoVersion(t, goVersion(t)))
			writeFile(t, filepath.Join(versionDir, version+".mod"), moduleFile)
			writeFile(t, filepath.Join(versionDir, version+".info"), fmt.Sprintf(`{"Version":%q}`, version))
			writeFile(t, filepath.Join(versionDir, "list"), "v1.0.0\nv1.1.0\n")
			zipPath := filepath.Join(versionDir, version+".zip")
			archive, err := os.Create(zipPath)
			if err != nil {
				t.Fatal(err)
			}
			zw := zip.NewWriter(archive)
			for name, contents := range map[string]string{
				module + "@" + version + "/go.mod": moduleFile,
				module + "@" + version + "/dep.go": "package dep\n\nconst Value = 1\n",
			} {
				file, err := zw.Create(name)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := file.Write([]byte(contents)); err != nil {
					t.Fatal(err)
				}
			}
			if err := zw.Close(); err != nil {
				t.Fatal(err)
			}
			if err := archive.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
	return proxy
}

func useProxy(t *testing.T, root, proxy string) {
	t.Helper()
	t.Setenv("GOPROXY", "file://"+filepath.ToSlash(proxy))
	t.Setenv("GOSUMDB", "off")
	t.Setenv("GONOSUMDB", "*")
	t.Setenv("GOMODCACHE", filepath.Join(root, "modcache"))
}
