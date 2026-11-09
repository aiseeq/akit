// mut — проверить тесты одним мутантом, затем вернуть файл к исходному виду.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/aiseeq/akit/edit"
)

const usage = `mut — run Go tests against one or more temporary source mutations.

Usage:
  mut OLD NEW FILE -- <go test arguments>
  mut [-F SPEC] -- <go test arguments>

Each SEARCH/REPLACE block is an independent mutant based on the original file.
Output: killed (the mutant fails a test), survived (tests pass), or error.
Exit: 0 all killed, 1 at least one survived, 2 an error; 130/143/129 on
SIGINT/SIGTERM/SIGHUP. Build errors are included in the verdict, up to 5 lines.
The mutated file is restored with its original bytes, mode, and modification time.
If it changed during the run, mut reverts its change when that is unambiguous.
Vet errors are reported as build failed (exit 2); a mutant outside the test packages survives.

  -F SPEC    read blocks from SPEC; "-" or no -F reads stdin when it is not a terminal
  -h         show this help

` + edit.SpecHelp

const (
	exitInterrupt = 130
	exitTerminate = 143
	exitHangup    = 129
)

type opts struct {
	spec       string
	positional []string
	testArgs   []string
}
type event struct{ Action, Package, ImportPath, Test, Output, FailedBuild string }
type verdict struct {
	text     string
	code     int
	killed   bool
	memory   bool
	failures []failure
}
type failure struct {
	packageName string
	testName    string
	output      string
}

func verdictOf(text string, code int, killed bool) verdict {
	return verdict{text: text, code: code, killed: killed}
}

func main() {
	o, err := parseArgs(os.Args[1:])
	if errors.Is(err, errHelp) {
		fmt.Print(usage)
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "mut: %v\n(mut -h for usage)\n", err)
		os.Exit(2)
	}
	os.Exit(run(o, os.Stdin, os.Stdout))
}

var errHelp = errors.New("help")
var errFileChanged = errors.New("file content differs")

func parseArgs(args []string) (opts, error) {
	var o opts
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			o.testArgs = args[i+1:]
			break
		}
		switch a {
		case "-h", "--help":
			return o, errHelp
		case "-F":
			if i+1 == len(args) {
				return o, errors.New("-F needs a value")
			}
			i++
			o.spec = args[i]
		default:
			if after, ok := strings.CutPrefix(a, "-F="); ok {
				o.spec = after
			} else if strings.HasPrefix(a, "-") {
				return o, fmt.Errorf("unknown flag %s", a)
			} else {
				o.positional = append(o.positional, a)
			}
		}
	}
	if len(o.testArgs) == 0 {
		return o, errors.New("missing -- <go test arguments>")
	}
	if o.spec != "" && len(o.positional) != 0 {
		return o, errors.New("-F and OLD NEW FILE together; pick one")
	}
	if o.spec == "" && len(o.positional) != 0 && len(o.positional) != 3 {
		return o, errors.New("need OLD NEW FILE")
	}
	return o, nil
}

// loadEdits — мутанты из OLD NEW FILE или из спеки (файл, "-" или stdin не с терминала).
func loadEdits(o opts, stdin io.Reader) ([]edit.Edit, error) {
	if len(o.positional) == 3 {
		return []edit.Edit{{Old: o.positional[0], New: o.positional[1], Files: []string{o.positional[2]}, Want: 1, Origin: "OLD"}}, nil
	}
	r := stdin
	if o.spec != "" && o.spec != "-" {
		f, err := os.Open(o.spec)
		if err != nil {
			return nil, fmt.Errorf("read spec: %w", err)
		}
		defer f.Close()
		r = f
	}
	if o.spec == "" {
		if f, ok := stdin.(*os.File); ok {
			if st, err := f.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 {
				return nil, errors.New("no spec on stdin")
			}
		}
	}
	return edit.ParseSpec(r)
}

func run(o opts, stdin io.Reader, stdout io.Writer) int {
	edits, err := loadEdits(o, stdin)
	if err != nil {
		fmt.Fprintf(stdout, "error: %v\n", err)
		return 2
	}
	graft, err := exec.LookPath("graft")
	if err != nil {
		fmt.Fprintf(stdout, "error: graft not found: %v\n", err)
		return 2
	}
	jsonArgs := testArgs(o.testArgs)
	// Сигналы ловятся на весь вызов: пришедший между правкой и прогоном не должен убить mut до отката.
	sigc := make(chan os.Signal, 2)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigc)
	var baseline *verdict
	survived, hadError := false, false
	for _, e := range edits {
		v, line := mutant(graft, e, jsonArgs, &baseline, sigc)
		if len(edits) > 1 {
			fmt.Fprintf(stdout, "%s:%d %s\n", e.Files[0], line, v.text)
		} else {
			fmt.Fprintln(stdout, v.text)
		}
		if v.code == exitInterrupt || v.code == exitTerminate || v.code == exitHangup {
			return v.code
		}
		if strings.Contains(v.text, "changed during the run") || strings.HasPrefix(v.text, "error: restore ") {
			return 2
		}
		if v.code == 2 {
			hadError = true
		}
		if strings.HasPrefix(v.text, "survived:") {
			survived = true
		}
	}
	if hadError {
		return 2
	}
	if survived {
		return 1
	}
	return 0
}

func testArgs(args []string) []string {
	out := append([]string(nil), args...)
	hasJSON, hasCount := false, false
	for _, a := range out {
		if a == "-json" {
			hasJSON = true
		}
		if a == "-count" || strings.HasPrefix(a, "-count=") {
			hasCount = true
		}
	}
	if !hasJSON {
		out = append([]string{"-json"}, out...)
	}
	if !hasCount {
		out = append([]string{"-count=1"}, out...)
	}
	return out
}

type mutation struct {
	path              string
	original, changed []byte
	mode              os.FileMode
	mtime             time.Time
	line              int
}

func prepare(e edit.Edit) (*mutation, error) {
	plan, err := edit.Build([]edit.Edit{e})
	if err != nil {
		return nil, err
	}
	if !plan.OK() {
		return nil, errors.New(strings.Join(plan.Mismatches(), "; "))
	}
	line := 1
	if len(plan.Results) > 0 && len(plan.Results[0].Hits) > 0 && len(plan.Results[0].Hits[0].Lines) > 0 {
		line = plan.Results[0].Hits[0].Lines[0]
	}
	path, err := filepath.EvalSymlinks(e.Files[0])
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", e.Files[0], err)
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	changed := bytes.Replace(original, []byte(e.Old), []byte(e.New), e.Want)
	return &mutation{path, original, changed, st.Mode(), st.ModTime(), line}, nil
}

func mutant(graft string, e edit.Edit, args []string, baseline **verdict, sigc <-chan os.Signal) (result verdict, resultLine int) {
	m, err := prepare(e)
	if err != nil {
		return verdictOf("error: "+err.Error(), 2, false), 1
	}
	resultLine = m.line
	restored, runStarted := false, false
	defer func() {
		if !restored {
			if restoreErr := emergencyRestore(m, runStarted); restoreErr != nil {
				if errors.Is(restoreErr, errFileChanged) {
					return
				}
				result = verdictOf("error: restore "+e.Files[0]+": "+restoreErr.Error(), 2, false)
			}
		}
	}()
	if err = writeMutation(m); err != nil {
		return verdictOf("error: write "+e.Files[0]+": "+err.Error(), 2, false), resultLine
	}
	runStarted = true
	result = runTests(graft, args, sigc)
	if err = verifyMutation(m); err != nil {
		copyPath, cerr := saveOriginal(m.original)
		if cerr != nil {
			copyPath = "(could not save source copy: " + cerr.Error() + ")"
		}
		if reverted, revertErr := revertInPlace(m, e); reverted && revertErr == nil {
			return verdictOf(fmt.Sprintf("error: %s changed during the run; mutant reverted in place, other changes kept (source copy: %s)", e.Files[0], copyPath), 2, false), resultLine
		}
		return verdictOf(fmt.Sprintf("error: %s changed during the run, not restored: %s; the mutant may still be in the file", e.Files[0], copyPath), 2, false), resultLine
	}
	if err = restore(m.path, m.original, m.mode, m.mtime); err != nil {
		return verdictOf("error: restore "+e.Files[0]+": "+err.Error(), 2, false), resultLine
	}
	restored = true
	if result.code == exitInterrupt || result.code == exitTerminate || result.code == exitHangup {
		return verdictOf(fmt.Sprintf("error: interrupted, %s restored", e.Files[0]), result.code, false), resultLine
	}
	return baselineCheck(result, graft, args, baseline, sigc), resultLine
}

func revertInPlace(m *mutation, e edit.Edit) (bool, error) {
	if e.New == "" {
		return false, nil
	}
	current, err := os.ReadFile(m.path)
	if err != nil {
		return false, fmt.Errorf("read changed file: %w", err)
	}
	if bytes.Count(current, []byte(e.New)) != e.Want {
		return false, nil
	}
	st, err := os.Stat(m.path)
	if err != nil {
		return false, fmt.Errorf("stat changed file: %w", err)
	}
	reverted := bytes.Replace(current, []byte(e.New), []byte(e.Old), e.Want)
	if err := restore(m.path, reverted, st.Mode(), st.ModTime()); err != nil {
		return false, err
	}
	return true, nil
}

func writeMutation(m *mutation) error {
	if err := os.WriteFile(m.path, m.changed, m.mode.Perm()); err != nil {
		return fmt.Errorf("write mutated file: %w", err)
	}
	if err := os.Chmod(m.path, m.mode); err != nil {
		return fmt.Errorf("set mutated file mode: %w", err)
	}
	return nil
}
func verifyMutation(m *mutation) error {
	current, err := os.ReadFile(m.path)
	if err != nil {
		return fmt.Errorf("read current file: %w", err)
	}
	if !bytes.Equal(current, m.changed) {
		return errFileChanged
	}
	return nil
}
func emergencyRestore(m *mutation, runStarted bool) error {
	if runStarted {
		if err := verifyMutation(m); err != nil {
			return err
		}
	}
	return restore(m.path, m.original, m.mode, m.mtime)
}
func baselineCheck(result verdict, graft string, args []string, baseline **verdict, sigc <-chan os.Signal) verdict {
	if !result.killed {
		return result
	}
	if *baseline == nil {
		b := runTests(graft, args, sigc)
		*baseline = &b
	}
	b := **baseline
	if b.code == exitInterrupt || b.code == exitTerminate || b.code == exitHangup {
		return verdictOf(fmt.Sprintf("error: interrupted, source restored (%s)", signalName(b.code)), b.code, false)
	}
	if b.code == 2 || b.memory || result.memory && b.killed {
		return verdictOf("error: fails without the mutant: "+b.text, 2, false)
	}
	if result.memory {
		if b.code == 1 {
			return result
		}
		return verdictOf("error: fails without the mutant: "+b.text, 2, false)
	}
	var novel []failure
	for _, candidate := range result.failures {
		if !hasFailure(b.failures, candidate) {
			novel = append(novel, candidate)
		}
	}
	if len(novel) > 0 {
		first := novel[0]
		var text string
		if first.testName != "" {
			text = fmt.Sprintf("killed: FAIL %s (%s)", first.testName, first.packageName)
		} else {
			message := first.output
			if message == "" {
				message = "test process failed"
			}
			text = fmt.Sprintf("killed: FAIL %s: %s", first.packageName, message)
		}
		if len(novel) > 1 {
			text += fmt.Sprintf(" +%d more", len(novel)-1)
		}
		return verdict{text: text, killed: true, failures: novel}
	}
	return verdictOf("error: fails without the mutant: "+b.text, 2, false)
}

func hasFailure(failures []failure, want failure) bool {
	for _, have := range failures {
		if have.packageName == want.packageName && have.testName == want.testName {
			return true
		}
	}
	return false
}

func restore(path string, data []byte, mode os.FileMode, modTime time.Time) error {
	if err := os.WriteFile(path, data, mode.Perm()); err != nil {
		return fmt.Errorf("write original bytes: %w", err)
	}
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("restore mode: %w", err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		return fmt.Errorf("restore modification time: %w", err)
	}
	return nil
}

func saveOriginal(data []byte) (string, error) {
	f, err := os.CreateTemp(os.TempDir(), "mut-source-*")
	if err != nil {
		return "", fmt.Errorf("create source copy: %w", err)
	}
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("write source copy: %w", err)
	}
	if err = f.Close(); err != nil {
		return "", fmt.Errorf("close source copy: %w", err)
	}
	return f.Name(), nil
}
func runTests(graft string, args []string, sigc <-chan os.Signal) verdict {
	select {
	case sig := <-sigc:
		return verdictOf(fmt.Sprintf("error: interrupted (%v)", sig), signalStatus(sig), false)
	default:
	}
	argv := append([]string{"exec", "--heavy", "--", "go", "test"}, args...)
	cmd := exec.Command(graft, argv...)
	cmd.Stdin = nil
	out, err := cmd.StdoutPipe()
	if err != nil {
		return verdictOf("error: "+err.Error(), 2, false)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err = cmd.Start(); err != nil {
		return verdictOf("error: start graft: "+err.Error(), 2, false)
	}
	var events []event
	done := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 4096), 16*1024*1024)
		for sc.Scan() {
			var ev event
			if json.Unmarshal(sc.Bytes(), &ev) == nil {
				events = append(events, ev)
			}
		}
		scanErr := sc.Err()
		waitErr := cmd.Wait()
		if scanErr != nil {
			done <- scanErr
		} else {
			done <- waitErr
		}
	}()
	// Wait is performed by the reader goroutine after stdout closes; signals are relayed and restoration follows it.
	select {
	case err = <-done:
	case sig := <-sigc:
		var signalErr error
		if sig != os.Interrupt {
			signalErr = cmd.Process.Signal(sig)
		}
		err = <-done
		text := fmt.Sprintf("error: interrupted (%v)", sig)
		if signalErr != nil && !errors.Is(signalErr, os.ErrProcessDone) {
			text += ": child signal: " + signalErr.Error()
		}
		return verdictOf(text, signalStatus(sig), false)
	}
	_ = err
	return classify(events, stderr.String(), cmd.ProcessState.ExitCode())
}

func signalStatus(sig os.Signal) int {
	switch sig {
	case os.Interrupt:
		return exitInterrupt
	case syscall.SIGTERM:
		return exitTerminate
	case syscall.SIGHUP:
		return exitHangup
	default:
		return 2
	}
}

func signalName(code int) string {
	switch code {
	case exitInterrupt:
		return "SIGINT"
	case exitTerminate:
		return "SIGTERM"
	case exitHangup:
		return "SIGHUP"
	default:
		return "signal"
	}
}

type testSummary struct {
	failedTests, packageFails []event
	packages, runs            []string
	outputs, build            map[string][]string
	buildOrder                []string
}

func summarize(events []event) testSummary {
	s := testSummary{outputs: map[string][]string{}, build: map[string][]string{}}
	for _, e := range events {
		pkg := e.Package
		if pkg == "" {
			pkg = e.ImportPath
		}
		if e.Action == "run" && e.Test != "" {
			s.runs = append(s.runs, e.Test)
			if !slices.Contains(s.packages, pkg) {
				s.packages = append(s.packages, pkg)
			}
		}
		if e.Action == "output" {
			s.addOutput(pkg, e.Output)
		}
		if e.FailedBuild != "" || e.Action == "build-fail" || e.Action == "build-output" {
			s.addBuild(pkg, e.Output)
		}
		if e.Action == "fail" {
			if e.Test == "" {
				s.packageFails = append(s.packageFails, e)
			} else {
				s.failedTests = append(s.failedTests, e)
			}
		}
	}
	return s
}

func (s *testSummary) addOutput(pkg, output string) {
	if strings.Contains(output, "[build failed]") || strings.Contains(output, "[setup failed]") {
		s.addBuild(pkg, output)
		return
	}
	if strings.TrimSpace(output) != "" {
		s.outputs[pkg] = append(s.outputs[pkg], strings.TrimSpace(output))
	}
}
func (s *testSummary) addBuild(pkg, output string) {
	if _, exists := s.build[pkg]; !exists {
		s.buildOrder = append(s.buildOrder, pkg)
		s.build[pkg] = nil
	}
	// Заголовок «# пакет» и пустые строки go печатает перед ошибками: в причину идут сами ошибки.
	if line := strings.TrimSpace(output); line != "" && !strings.HasPrefix(line, "# ") {
		s.build[pkg] = append(s.build[pkg], line)
	}
}

func classify(events []event, stderr string, exit int) verdict {
	for line := range strings.SplitSeq(stderr, "\n") {
		clean := strings.TrimPrefix(line, "graft: ")
		// Только сообщения graft об убитых процессах: строку о размере потолка он пишет на каждом прогоне.
		if strings.Contains(line, "hit its memory ceiling") || strings.Contains(line, "the kernel killed") {
			return verdict{text: "killed: memory ceiling (" + strings.TrimSpace(clean) + ")", killed: true, memory: true}
		}
	}
	s := summarize(events)
	if len(s.buildOrder) > 0 {
		p := s.buildOrder[0]
		lines := s.build[p]
		if len(lines) == 0 {
			lines = s.outputs[p]
		}
		return verdictOf(fmt.Sprintf("error: build failed (%s): %s", p, strings.Join(firstN(lines, 5), " | ")), 2, false)
	}
	if len(s.failedTests) > 0 || len(s.packageFails) > 0 {
		failures := collectFailures(s)
		first := failures[0]
		var text string
		if first.testName != "" {
			text = fmt.Sprintf("killed: FAIL %s (%s)", first.testName, first.packageName)
		} else {
			message := first.output
			if message == "" {
				message = "test process failed"
			}
			text = fmt.Sprintf("killed: FAIL %s: %s", first.packageName, message)
		}
		if len(failures) > 1 {
			text += fmt.Sprintf(" +%d more", len(failures)-1)
		}
		return verdict{text: text, killed: true, failures: failures}
	}
	if exit == 125 || exit == 126 || exit == 127 {
		last := ""
		if lines := strings.Split(strings.TrimSpace(stderr), "\n"); len(lines) > 0 {
			last = lines[len(lines)-1]
		}
		return verdictOf(fmt.Sprintf("error: graft exit %d: %s", exit, last), 2, false)
	}
	if len(s.runs) == 0 {
		return verdictOf("error: no test ran (check -run and packages)", 2, false)
	}
	if exit != 0 {
		return verdictOf(fmt.Sprintf("error: go test exit %d: %s", exit, strings.TrimSpace(stderr)), 2, false)
	}
	return verdictOf("survived: ok "+strings.Join(s.packages, " "), 1, false)
}

func collectFailures(s testSummary) []failure {
	var failures []failure
	seen := map[string]bool{}
	for _, e := range s.failedTests {
		p := e.Package
		if p == "" {
			p = e.ImportPath
		}
		name := strings.SplitN(e.Test, "/", 2)[0]
		key := p + "\x00" + name
		if !seen[key] {
			seen[key] = true
			failures = append(failures, failure{packageName: p, testName: name})
		}
	}
	for _, e := range s.packageFails {
		p := e.Package
		if p == "" {
			p = e.ImportPath
		}
		key := p + "\x00"
		if seen[key] {
			continue
		}
		seen[key] = true
		message := ""
		if lines := s.outputs[p]; len(lines) > 0 {
			message = lines[0]
		}
		failures = append(failures, failure{packageName: p, output: message})
	}
	return failures
}
func firstN(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
