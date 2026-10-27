package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runSub(t *testing.T, stdin string, args ...string) (code int, out, errOut string) {
	t.Helper()
	opts, err := parseArgs(args)
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	var o, e bytes.Buffer
	code = run(opts, strings.NewReader(stdin), &o, &e)
	return code, o.String(), e.String()
}

func tempFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func content(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestArgsMode(t *testing.T) {
	f := tempFile(t, "a b a\n")
	code, out, errOut := runSub(t, "", "a", "c", f, "-n", "2")
	if code != 0 || content(t, f) != "c b c\n" {
		t.Fatalf("code %d, out %q, err %q, file %q", code, out, errOut, content(t, f))
	}
	if !strings.HasPrefix(out, "sub: 2 replacements in 1 file: ") {
		t.Errorf("out: %q", out)
	}
}

func TestArgsModeMismatch(t *testing.T) {
	f := tempFile(t, "a b a\n")
	code, _, errOut := runSub(t, "", "a", "c", f)
	if code != exitMismatch || content(t, f) != "a b a\n" {
		t.Fatalf("code %d, file %q", code, content(t, f))
	}
	if !strings.Contains(errOut, "nothing changed") || !strings.Contains(errOut, "expected 1, found 2") {
		t.Errorf("err: %q", errOut)
	}
}

func TestSpecModeDryRun(t *testing.T) {
	f := tempFile(t, "hello\n")
	spec := f + "\n<<<<<<< SEARCH\nhello\n=======\nbye\n>>>>>>> REPLACE\n"
	code, out, _ := runSub(t, spec, "--dry-run")
	if code != 0 || !strings.HasPrefix(out, "sub: dry run, would make 1 replacement") || content(t, f) != "hello\n" {
		t.Fatalf("code %d, out %q, file %q", code, out, content(t, f))
	}
	code, _, _ = runSub(t, spec)
	if code != 0 || content(t, f) != "bye\n" {
		t.Fatalf("code %d, file %q", code, content(t, f))
	}
}

func TestUsageErrors(t *testing.T) {
	if _, err := parseArgs([]string{"-x"}); err == nil {
		t.Error("unknown flag accepted")
	}
	if _, err := parseArgs([]string{"-n"}); err == nil {
		t.Error("-n without value accepted")
	}
	opts, err := parseArgs([]string{"- list item", "-1", "f"})
	if err != nil || len(opts.args) != 3 {
		t.Errorf("text starting with - taken as a flag: %+v %v", opts, err)
	}
	opts, err = parseArgs([]string{"--", "-x", "y", "f"})
	if err != nil || len(opts.args) != 3 || opts.args[0] != "-x" {
		t.Errorf("-- not honoured: %+v %v", opts, err)
	}
	if code, _, _ := runSub(t, "", "a", "b"); code != exitUsage {
		t.Errorf("two args: code %d", code)
	}
	if code, _, _ := runSub(t, "f\n<<<<<<< SEARCH\na\n", "-n", "2"); code != exitUsage {
		t.Errorf("-n with spec: code %d", code)
	}
}

// `sub FILE <<'EOF'` с блоками без строки пути — так агенты пишут сами.
func TestFileArgWithSpec(t *testing.T) {
	f := tempFile(t, "one two\n")
	spec := "<<<<<<< SEARCH\none\n=======\n1\n>>>>>>> REPLACE\n<<<<<<< SEARCH\ntwo\n=======\n2\n>>>>>>> REPLACE\n"
	code, out, errOut := runSub(t, spec, f)
	if code != 0 || content(t, f) != "1 2\n" {
		t.Fatalf("code %d, out %q, err %q, file %q", code, out, errOut, content(t, f))
	}
	code, _, errOut = runSub(t, filepath.Join(filepath.Dir(f), "other.txt")+"\n"+spec, f)
	if code != exitUsage || !strings.Contains(errOut, "differs from the FILE argument") || content(t, f) != "1 2\n" {
		t.Errorf("other path line: code %d, err %q, file %q", code, errOut, content(t, f))
	}
	if code, _, _ := runSub(t, spec, f, "-n", "2"); code != exitUsage {
		t.Errorf("-n with FILE and spec: code %d", code)
	}
}
