package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aiseeq/akit/proc"
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

// A parent whose subtests were all skipped passes; whether it checked anything
// itself is unknown, so a survivor says that only parents passed.
func TestAllSubtestsSkipped(t *testing.T) {
	f := newFixture(t, "package calc\nimport \"testing\"\nfunc TestAdd(t *testing.T){for _, n := range []string{\"one\", \"two\"} {t.Run(n, func(t *testing.T){t.SkipNow()})}}\n")
	out, code := f.run(t, "a + b", "a - b", "calc.go", "--", "./...")
	if code != 1 || !strings.Contains(out, "(only parent tests passed, 2 skipped: TestAdd/one TestAdd/two)") {
		t.Fatalf("got %d %q", code, out)
	}
	f.unchanged(t)
}

// A parent that checks the code itself kills the mutant though its subtests
// are skipped.
func TestParentKillsBesideSkippedSubtests(t *testing.T) {
	f := newFixture(t, "package calc\nimport \"testing\"\nfunc TestAdd(t *testing.T){if Add(1,2)!=3 {t.Fatal(\"sum\")}; t.Run(\"one\", func(t *testing.T){t.Skip(\"need SAMPLE_VAR\")})}\n")
	out, code := f.run(t, "a + b", "a - b", "calc.go", "--", "./...")
	if code != 0 || !strings.Contains(out, "killed: FAIL TestAdd (") {
		t.Fatalf("got %d %q", code, out)
	}
	f.unchanged(t)
}

// A skip that prints nothing is named as such.
func TestSkipWithoutReason(t *testing.T) {
	f := newFixture(t, "package calc\nimport \"testing\"\nfunc TestAdd(t *testing.T){t.SkipNow()}\n")
	out, code := f.run(t, "a + b", "a - b", "calc.go", "--", "./...")
	if code != 2 || !strings.Contains(out, "every test skipped (TestAdd: no reason printed)") {
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
			_, _ = proc.StopChild(cmd.Process.Pid, 0, nil)
			t.Fatal("test did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, _ = proc.StopChildGroup(cmd, 10*time.Second, syscall.SIGINT)
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

func TestSignalWaitsForGraftButNotItsDetachedChild(t *testing.T) {
	if os.Getenv("MUT_SIGNAL_HELPER") == "1" {
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGTERM)
		defer signal.Stop(signals)
		verdict := runTests([]string{os.Getenv("MUT_FAKE_GRAFT")}, []string{"./..."}, signals)
		os.Exit(verdict.code)
	}
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	pidFile := filepath.Join(dir, "sleep.pid")
	fakeGraft := filepath.Join(dir, "graft")
	script := "#!/bin/sh\n" +
		"sleep 30 </dev/null >/dev/null 2>&1 &\n" +
		"echo $! > \"$MUT_SLEEP_PID\"\n" +
		"echo ready > \"$MUT_READY\"\n" +
		"trap 'exit 143' TERM\n" +
		"while :; do wait; done\n"
	if err := os.WriteFile(fakeGraft, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestSignalWaitsForGraftButNotItsDetachedChild$")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(os.Environ(), "MUT_SIGNAL_HELPER=1", "MUT_FAKE_GRAFT="+fakeGraft, "MUT_READY="+ready, "MUT_SLEEP_PID="+pidFile)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := proc.StopChildGroup(cmd, 0, syscall.SIGKILL); err != nil {
			t.Errorf("clean helper process group: %v", err)
		}
	})
	waitForMarker(t, ready)
	start := time.Now()
	if err := proc.SignalChild(cmd.Process.Pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 143 {
		t.Fatalf("mut exit: %v, want 143", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("mut waited for the detached child: %v", elapsed)
	}
	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatal(err)
	}
	stat, err := proc.ReadStat("/proc", pid)
	if err != nil || stat.State == "Z" || stat.State == "X" {
		t.Fatalf("detached child was not left for group cleanup: stat=%+v err=%v", stat, err)
	}
}

func waitForMarker(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("marker did not appear: %s", path)
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

// Tests on the project database get its DSN only through graft exec --test-db.
func TestTestDBRunsThroughGraftTestDB(t *testing.T) {
	f := newFixture(t, "package calc\nimport (\"os\"; \"testing\")\nfunc TestAdd(t *testing.T){if os.Getenv(\"TEST_DB_DSN\")==\"\"{t.Fatal(\"TEST_DB_DSN is not set\")}; if Add(2,3)!=5{t.Fatal(Add(2,3))}}\n")
	script := `#!/bin/sh
while [ "$1" != -- ]; do [ "$1" = --test-db ] && export TEST_DB_DSN=postgres://test; shift; done
shift
exec "$@"
`
	if err := os.WriteFile(filepath.Join(f.bin, "graft"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	if out, code := f.run(t, "--test-db", "a + b", "a - b", "calc.go", "--", "./..."); code != 0 || !strings.HasPrefix(out, "killed") {
		t.Fatalf("with --test-db: %d %q", code, out)
	}
	if out, code := f.run(t, "--test-db", "a + b", "a + b", "calc.go", "--", "./..."); code != 1 || !strings.HasPrefix(out, "survived") {
		t.Fatalf("empty edit with --test-db: %d %q", code, out)
	}
	if out, code := f.run(t, "a + b", "a - b", "calc.go", "--", "./..."); code != 2 || !strings.Contains(out, "fails without the mutant") {
		t.Fatalf("without --test-db: %d %q", code, out)
	}
	f.unchanged(t)
}

// A heavy run moved to another machine sees only the variables named with
// graft's --env; mut passes each --env NAME on, and only those.
func TestEnvPassesNamedVariablesToGraft(t *testing.T) {
	f := newFixture(t, "package calc\nimport (\"os\"; \"testing\")\nfunc TestAdd(t *testing.T){if os.Getenv(\"SAMPLE_VAR\")!=\"1\"{t.Fatal(\"SAMPLE_VAR is not passed\")}; if os.Getenv(\"OTHER_VAR\")!=\"\"{t.Fatal(\"OTHER_VAR leaked\")}; if Add(2,3)!=5{t.Fatal(Add(2,3))}}\n")
	// Как graft на чужой машине: переменные вызывающего доходят, только если названы через --env.
	script := `#!/bin/sh
keep=" "
while [ "$1" != -- ]; do [ "$1" = --env ] && { shift; keep="$keep$1 "; }; shift; done
shift
case "$keep" in *" SAMPLE_VAR "*) ;; *) unset SAMPLE_VAR;; esac
case "$keep" in *" OTHER_VAR "*) ;; *) unset OTHER_VAR;; esac
exec "$@"
`
	if err := os.WriteFile(filepath.Join(f.bin, "graft"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SAMPLE_VAR", "1")
	t.Setenv("OTHER_VAR", "1")
	if out, code := f.run(t, "--env", "SAMPLE_VAR", "a + b", "a - b", "calc.go", "--", "./..."); code != 0 || !strings.HasPrefix(out, "killed") {
		t.Fatalf("with --env SAMPLE_VAR: %d %q", code, out)
	}
	if out, code := f.run(t, "--env=SAMPLE_VAR", "a + b", "a + b", "calc.go", "--", "./..."); code != 1 || !strings.HasPrefix(out, "survived") {
		t.Fatalf("empty edit with --env=SAMPLE_VAR: %d %q", code, out)
	}
	if out, code := f.run(t, "a + b", "a - b", "calc.go", "--", "./..."); code != 2 || !strings.Contains(out, "fails without the mutant") {
		t.Fatalf("without --env: %d %q", code, out)
	}
	if out, code := f.run(t, "--env", "SAMPLE_VAR", "--env", "OTHER_VAR", "a + b", "a - b", "calc.go", "--", "./..."); code != 2 || !strings.Contains(out, "fails without the mutant") {
		t.Fatalf("both variables passed: %d %q", code, out)
	}
	f.unchanged(t)
}

func TestEnvRejectsAssignmentsAndBadNames(t *testing.T) {
	for _, args := range [][]string{{"--env"}, {"--env", "FOO=1"}, {"--env="}, {"--env", "1FOO"}, {"--env", "FOO-BAR"}} {
		if _, err := parseArgs(append(args, "--", "./...")); err == nil {
			t.Errorf("%q: no error", args)
		}
	}
	o, err := parseArgs([]string{"--env", "A_1", "--env=b", "--", "./..."})
	if err != nil || strings.Join(o.env, ",") != "A_1,b" {
		t.Fatalf("valid names: %v %q", err, o.env)
	}
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
