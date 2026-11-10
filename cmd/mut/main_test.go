package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

var mutBinary string

func TestLoadForeignSpec(t *testing.T) {
	o := opts{testArgs: []string{"go", "test"}}
	edits, err := loadEdits(o, strings.NewReader("FILE x.go\nOLD\nold\nNEW\nnew\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(edits) != 1 || edits[0].Files[0] != "x.go" || edits[0].Old != "old" || edits[0].New != "new" {
		t.Fatalf("edits = %#v", edits)
	}
}

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "mut-test-bin-")
	if err != nil {
		panic(err)
	}
	mutBinary = filepath.Join(dir, "mut")
	build := exec.Command("go", "build", "-o", mutBinary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintln(os.Stderr, string(out))
		os.Exit(2)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

type fixture struct {
	dir, src, path string
	original       []byte
	mtime          time.Time
	bin            string
}

func newFixture(t *testing.T, test string) *fixture {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module fixture\n\ngo 1.26.0\n"), 0644); err != nil {
		t.Fatal(err)
	}
	src := "package calc\nfunc Add(a, b int) int { return a + b }\n"
	path := filepath.Join(dir, "calc.go")
	if err := os.WriteFile(path, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "calc_test.go"), []byte(test), 0644); err != nil {
		t.Fatal(err)
	}
	graftDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(graftDir, 0755); err != nil {
		t.Fatal(err)
	}
	graft := filepath.Join(graftDir, "graft")
	// Как настоящий graft --heavy: справочная строка о потолке на каждом прогоне — не упор в него.
	script := "#!/bin/sh\necho 'graft: exec: moved to graft-heavy-1.scope (memory ceiling 10 GiB: 34% of RAM, the default)' >&2\nwhile [ \"$1\" != -- ]; do shift; done\nshift\nexec \"$@\"\n"
	if err := os.WriteFile(graft, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	f := &fixture{dir: dir, src: src, path: path, original: []byte(src), bin: graftDir}
	f.stamp(t)
	return f
}
func (f *fixture) stamp(t *testing.T) {
	t.Helper()
	now := time.Unix(1700000000, 123456789)
	if err := os.Chtimes(f.path, now, now); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.mtime = st.ModTime()
}
func (f *fixture) run(t *testing.T, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(mutBinary, args...)
	cmd.Dir = f.dir
	cmd.Env = mutEnv(t, f.bin)
	var out, er bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &er
	err := cmd.Run()
	code := 0
	if err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			code = e.ExitCode()
		} else {
			t.Fatal(err)
		}
	}
	if er.Len() > 0 {
		t.Logf("stderr: %s", er.String())
	}
	return strings.TrimSpace(out.String()), code
}

func mutEnv(t *testing.T, bin string) []string {
	t.Helper()
	var env []string
	for _, value := range os.Environ() {
		if strings.HasPrefix(value, "PATH=") || strings.HasPrefix(value, "TMPDIR=") {
			continue
		}
		env = append(env, value)
	}
	return append(env, "PATH="+bin+":"+os.Getenv("PATH"), "TMPDIR="+t.TempDir())
}
func (f *fixture) unchanged(t *testing.T) {
	t.Helper()
	got, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, f.original) {
		t.Fatalf("source changed: %q", got)
	}
	st, err := os.Stat(f.path)
	if err != nil {
		t.Fatal(err)
	}
	if !st.ModTime().Equal(f.mtime) {
		t.Fatalf("mtime changed: got %v want %v", st.ModTime(), f.mtime)
	}
}

const goodTest = "package calc\nimport \"testing\"\nfunc TestAdd(t *testing.T){if Add(2,3)!=5{t.Fatalf(\"got %d\",Add(2,3))}}\n"

func TestKilled(t *testing.T) {
	f := newFixture(t, goodTest)
	out, code := f.run(t, "a + b", "a - b", "calc.go", "--", "./...")
	if code != 0 || !strings.Contains(out, "killed: FAIL TestAdd (") {
		t.Fatalf("got %d %q", code, out)
	}
	f.unchanged(t)
}
func TestSurvivedNoop(t *testing.T) {
	f := newFixture(t, goodTest)
	out, code := f.run(t, "a + b", "b + a", "calc.go", "--", "./...")
	if code != 1 || !strings.Contains(out, "survived: ok ") {
		t.Fatalf("got %d %q", code, out)
	}
	f.unchanged(t)
}
func TestBuildFailed(t *testing.T) {
	f := newFixture(t, goodTest)
	out, code := f.run(t, "a + b", "a +", "calc.go", "--", "./...")
	if code != 2 || !strings.Contains(out, "build failed") {
		t.Fatalf("got %d %q", code, out)
	}
	f.unchanged(t)
}
func TestNoMatch(t *testing.T) {
	f := newFixture(t, goodTest)
	out, code := f.run(t, "missing", "x", "calc.go", "--", "./...")
	if code != 2 || !strings.Contains(out, "error:") {
		t.Fatalf("got %d %q", code, out)
	}
	f.unchanged(t)
}
func TestNoTestRan(t *testing.T) {
	f := newFixture(t, goodTest)
	out, code := f.run(t, "a + b", "a - b", "calc.go", "--", "-run", "NoSuchTest", "./...")
	if code != 2 || !strings.Contains(out, "no test ran") {
		t.Fatalf("got %d %q", code, out)
	}
	f.unchanged(t)
}

// A run whose tests were all skipped tested nothing: the mutant did not survive
// them, and the answer names the skip reason.
func TestAllSkipped(t *testing.T) {
	f := newFixture(t, "package calc\nimport \"testing\"\nfunc TestAdd(t *testing.T){t.Skip(\"SAMPLE_VAR empty\")}\n")
	out, code := f.run(t, "a + b", "a - b", "calc.go", "--", "./...")
	if code != 2 || !strings.Contains(out, "every test skipped") || !strings.Contains(out, "SAMPLE_VAR empty") {
		t.Fatalf("got %d %q", code, out)
	}
	f.unchanged(t)
}

// A survivor beside skipped tests names them: the skipped one may be the test
// that would have caught the mutant.
func TestSurvivedBesideSkipped(t *testing.T) {
	f := newFixture(t, goodTest)
	skip := "package calc\nimport \"testing\"\nfunc TestSub(t *testing.T){t.Skip(\"no sample service\")}\n"
	if err := os.WriteFile(filepath.Join(f.dir, "sub_test.go"), []byte(skip), 0644); err != nil {
		t.Fatal(err)
	}
	out, code := f.run(t, "a + b", "b + a", "calc.go", "--", "./...")
	if code != 1 || !strings.Contains(out, "survived: ok ") || !strings.Contains(out, "1 skipped: TestSub") {
		t.Fatalf("got %d %q", code, out)
	}
	f.unchanged(t)
}

func TestFailsWithoutMutant(t *testing.T) {
	f := newFixture(t, "package calc\nimport \"testing\"\nfunc TestAdd(t *testing.T){t.Fatal(\"baseline\")}\n")
	out, code := f.run(t, "a + b", "b + a", "calc.go", "--", "./...")
	if code != 2 || !strings.Contains(out, "fails without the mutant") {
		t.Fatalf("got %d %q", code, out)
	}
	f.unchanged(t)
}
func TestKilledBesideRedTest(t *testing.T) {
	f := newFixture(t, goodTest)
	red := "package calc\nimport \"testing\"\nfunc TestAARed(t *testing.T) { t.Fatal(\"baseline red\") }\n"
	if err := os.WriteFile(filepath.Join(f.dir, "a_test.go"), []byte(red), 0644); err != nil {
		t.Fatal(err)
	}
	out, code := f.run(t, "a + b", "a - b", "calc.go", "--", "./...")
	if code != 0 || !strings.Contains(out, "killed: FAIL TestAdd (") {
		t.Fatalf("got %d %q", code, out)
	}
	f.unchanged(t)
}
func TestInterruptRestores(t *testing.T) {
	test := `package calc
import("os";"time";"testing")
func TestAdd(t *testing.T){os.WriteFile("started",[]byte("yes"),0600);for i:=0;i<300;i++{time.Sleep(100*time.Millisecond)}}
`
	f := newFixture(t, test)
	cmd := exec.Command(mutBinary, "a + b", "a - b", "calc.go", "--", "./...")
	cmd.Dir = f.dir
	cmd.Env = mutEnv(t, f.bin)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(f.dir, "started")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatal("test did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
	err := cmd.Wait()
	ee, ok := err.(*exec.ExitError)
	if !ok || ee.ExitCode() != 130 {
		t.Fatalf("exit %v output %q", err, out.String())
	}
	if !strings.Contains(out.String(), "interrupted") {
		t.Fatalf("output %q", out.String())
	}
	f.unchanged(t)
}
func TestSpecMultiple(t *testing.T) {
	f := newFixture(t, goodTest)
	spec := filepath.Join(f.dir, "spec")
	body := "calc.go\n<<<<<<< SEARCH\na + b\n=======\na - b\n>>>>>>> REPLACE\n<<<<<<< SEARCH\na + b\n=======\nb + a\n>>>>>>> REPLACE\n"
	if err := os.WriteFile(spec, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	out, code := f.run(t, "-F", spec, "--", "./...")
	lines := strings.Split(out, "\n")
	if code != 1 || len(lines) != 2 || !strings.HasPrefix(lines[0], "calc.go:2 killed:") || !strings.HasPrefix(lines[1], "calc.go:2 survived:") {
		t.Fatalf("got %d %q", code, out)
	}
	f.unchanged(t)
}
func TestChangedDuringRun(t *testing.T) {
	test := `package calc
import("os";"testing")
func TestAdd(t *testing.T){os.WriteFile("calc.go",[]byte("package calc\nfunc Add(a, b int) int { return a - b }\n// foreign edit\n"),0644)}
`
	f := newFixture(t, test)
	out, code := f.run(t, "a + b", "a - b", "calc.go", "--", "./...")
	if code != 2 || !strings.Contains(out, "mutant reverted in place, other changes kept") {
		t.Fatalf("got %d %q", code, out)
	}
	gotSource, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	wantSource := append(bytes.TrimSuffix(f.original, []byte("\n")), []byte("\n// foreign edit\n")...)
	if !bytes.Equal(gotSource, wantSource) {
		t.Fatalf("source after revert: %q, want %q", gotSource, wantSource)
	}
	copyPath := strings.TrimSpace(strings.SplitN(out, "source copy: ", 2)[1])
	copyPath = strings.TrimSuffix(copyPath, ")")
	got, err := os.ReadFile(copyPath)
	if err != nil {
		t.Fatalf("copy %q: %v", copyPath, err)
	}
	if !bytes.Equal(got, f.original) {
		t.Fatalf("copy differs: %q", got)
	}
}
func TestChangedDuringRunMutantGone(t *testing.T) {
	test := `package calc
import("os";"testing")
func TestAdd(t *testing.T){os.WriteFile("calc.go",[]byte("foreign edit"),0644)}
`
	f := newFixture(t, test)
	out, code := f.run(t, "a + b", "a - b", "calc.go", "--", "./...")
	if code != 2 || !strings.Contains(out, "the mutant may still be in the file") {
		t.Fatalf("got %d %q", code, out)
	}
	gotSource, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotSource) != "foreign edit" {
		t.Fatalf("source changed to %q", gotSource)
	}
	copyPath := strings.TrimSpace(strings.SplitN(strings.SplitN(out, "not restored: ", 2)[1], ";", 2)[0])
	got, err := os.ReadFile(copyPath)
	if err != nil {
		t.Fatalf("copy %q: %v", copyPath, err)
	}
	if !bytes.Equal(got, f.original) {
		t.Fatalf("copy differs: %q", got)
	}
}
func TestMemoryCeiling(t *testing.T) {
	f := newFixture(t, goodTest)
	script := `#!/bin/sh
case "$(cat calc.go)" in *"a - b"*) echo "graft: heavy scope x hit its memory ceiling 1G (default): the kernel killed 1 process(es) during this step" >&2; exit 1;; esac
while [ "$1" != -- ]; do shift; done
shift
exec "$@"
`
	if err := os.WriteFile(filepath.Join(f.bin, "graft"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	out, code := f.run(t, "a + b", "a - b", "calc.go", "--", "./...")
	if code != 0 || !strings.Contains(out, "killed: memory ceiling (") {
		t.Fatalf("got %d %q", code, out)
	}
	f.unchanged(t)
}

// The help shows only spec rules mut follows: no "sub FILE" form, and blocks do
// not build on each other.
func TestUsageHasNoSubOnlyRules(t *testing.T) {
	for _, s := range []string{"sub FILE", "previous blocks", "FILE..."} {
		if strings.Contains(usage, s) {
			t.Errorf("mut usage mentions %q", s)
		}
	}
}
