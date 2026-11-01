package edit

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestParseSpec(t *testing.T) {
	spec := `a.go
<<<<<<< SEARCH
old one
  indented
=======
new one
>>>>>>> REPLACE

<<<<<<< SEARCH 3
x
=======
>>>>>>> REPLACE
b.go
<<<<<<< SEARCH
y
=======
z
>>>>>>> REPLACE
`
	edits, err := ParseSpec(strings.NewReader(spec))
	if err != nil {
		t.Fatal(err)
	}
	if len(edits) != 3 {
		t.Fatalf("got %d edits", len(edits))
	}
	if e := edits[0]; e.Files[0] != "a.go" || e.Old != "old one\n  indented" || e.New != "new one" || e.Want != 1 {
		t.Errorf("edit 1: %+v", e)
	}
	if e := edits[1]; e.Files[0] != "a.go" || e.Old != "x" || e.New != "" || e.Want != 3 {
		t.Errorf("edit 2: %+v", e)
	}
	if e := edits[2]; e.Files[0] != "b.go" || e.Origin != "block 3 (line 14)" {
		t.Errorf("edit 3: %+v", e)
	}
}

func TestParseSpecIndentedMarkers(t *testing.T) {
	spec := "  a.go\n  <<<<<<< SEARCH\n\told\n  =======\n\tnew\n  >>>>>>> REPLACE\n"
	edits, err := ParseSpec(strings.NewReader(spec))
	if err != nil {
		t.Fatal(err)
	}
	if len(edits) != 1 || edits[0].Files[0] != "a.go" || edits[0].Old != "\told" || edits[0].New != "\tnew" {
		t.Errorf("edits: %+v", edits)
	}
}

func TestParseSpecErrors(t *testing.T) {
	cases := map[string]string{
		"no file":       "<<<<<<< SEARCH\na\n=======\nb\n>>>>>>> REPLACE\n",
		"not closed":    "f\n<<<<<<< SEARCH\na\n=======\nb\n",
		"no divider":    "f\n<<<<<<< SEARCH\na\n>>>>>>> REPLACE\n",
		"bad count":     "f\n<<<<<<< SEARCH 0\na\n=======\nb\n>>>>>>> REPLACE\n",
		"empty search":  "f\n<<<<<<< SEARCH\n=======\nb\n>>>>>>> REPLACE\n",
		"stray divider": "f\n=======\n",
		"no blocks":     "f\n\n",
	}
	for name, spec := range cases {
		if _, err := ParseSpec(strings.NewReader(spec)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestBuildWriteAllOrNothing(t *testing.T) {
	dir := writeFiles(t, map[string]string{"a.txt": "foo\nbar\nfoo\n", "b.txt": "baz\n"})
	a, b := filepath.Join(dir, "a.txt"), filepath.Join(dir, "b.txt")

	plan, err := Build([]Edit{
		{Files: []string{a}, Old: "foo", New: "FOO", Want: 2, Origin: "e1"},
		{Files: []string{b}, Old: "missing", New: "x", Want: 1, Origin: "e2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.OK() {
		t.Fatal("plan should mismatch")
	}
	if _, err := plan.Write(); err == nil {
		t.Fatal("Write of a mismatched plan must fail")
	}
	if got := readFile(t, a); got != "foo\nbar\nfoo\n" {
		t.Errorf("a.txt changed: %q", got)
	}
	m := plan.Mismatches()
	if len(m) != 1 || !strings.Contains(m[0], "e2: expected 1, found 0") {
		t.Errorf("mismatches: %q", m)
	}

	plan, err = Build([]Edit{
		{Files: []string{a}, Old: "foo", New: "FOO", Want: 2, Origin: "e1"},
		{Files: []string{a, b}, Old: "ba", New: "BA", Want: 2, Origin: "e2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.OK() {
		t.Fatalf("plan should match: %q", plan.Mismatches())
	}
	if _, err := plan.Write(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, a); got != "FOO\nBAr\nFOO\n" {
		t.Errorf("a.txt: %q", got)
	}
	if got := readFile(t, b); got != "BAz\n" {
		t.Errorf("b.txt: %q", got)
	}
	if st, _ := os.Stat(a); st.Mode().Perm() != 0o640 {
		t.Errorf("mode not kept: %v", st.Mode())
	}
	if got := plan.Summary(); !strings.HasPrefix(got, "4 replacements in 2 files:") {
		t.Errorf("summary: %q", got)
	}
}

func TestWritePreservesXattr(t *testing.T) {
	dir := writeFiles(t, map[string]string{"a.txt": "old\n"})
	path := filepath.Join(dir, "a.txt")
	if err := unix.Setxattr(path, "user.test", []byte("value"), 0); err != nil {
		t.Fatal(err)
	}
	plan, err := Build([]Edit{{Files: []string{path}, Old: "old", New: "new", Want: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Write(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 100)
	n, err := unix.Getxattr(path, "user.test", buf)
	if err != nil {
		t.Fatalf("Getxattr: %v", err)
	}
	if string(buf[:n]) != "value" {
		t.Errorf("xattr = %q", buf[:n])
	}
}

func TestWritePreservesACL(t *testing.T) {
	if _, err := os.Stat("/usr/bin/setfacl"); err != nil {
		t.Skip("setfacl is unavailable")
	}
	if _, err := os.Stat("/usr/bin/getfacl"); err != nil {
		t.Skip("getfacl is unavailable")
	}
	dir, err := os.MkdirTemp(".", ".acl-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(path, []byte("old\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("setfacl", "-m", "u:12345:rw", path).CombinedOutput(); err != nil {
		t.Fatalf("setfacl: %v: %s", err, out)
	}
	plan, err := Build([]Edit{{Files: []string{path}, Old: "old", New: "new", Want: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Write(); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("getfacl", "-cp", path).CombinedOutput()
	if err != nil {
		t.Fatalf("getfacl: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "user:12345:rw-") {
		t.Errorf("ACL was not preserved:\n%s", out)
	}
}

func TestWriteHardLinkAbortsAllFiles(t *testing.T) {
	dir := writeFiles(t, map[string]string{"a.txt": "old\n", "other.txt": "old\n"})
	path, alias := filepath.Join(dir, "a.txt"), filepath.Join(dir, "alias.txt")
	if err := os.Link(path, alias); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "other.txt")
	plan, err := Build([]Edit{{Files: []string{other, path}, Old: "old", New: "new", Want: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Write(); err == nil || !strings.Contains(err.Error(), "hard link") || !strings.Contains(err.Error(), "nothing written") {
		t.Fatalf("want hard-link refusal, got %v", err)
	}
	for _, name := range []string{path, alias, other} {
		if got := readFile(t, name); got != "old\n" {
			t.Errorf("%s changed: %q", name, got)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Errorf("temporary files left: %d entries", len(entries))
	}
}

func TestSequentialEdits(t *testing.T) {
	dir := writeFiles(t, map[string]string{"a.txt": "one\n"})
	a := filepath.Join(dir, "a.txt")
	plan, err := Build([]Edit{
		{Files: []string{a}, Old: "one", New: "two", Want: 1},
		{Files: []string{a}, Old: "two", New: "three\nfour", Want: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Write(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, a); got != "three\nfour\n" {
		t.Errorf("got %q", got)
	}
}

func TestTooManyMatchesReportsLines(t *testing.T) {
	dir := writeFiles(t, map[string]string{"a.txt": "x\ny\nx\n"})
	a := filepath.Join(dir, "a.txt")
	plan, err := Build([]Edit{{Files: []string{a}, Old: "x", New: "z", Want: 1, Origin: "OLD"}})
	if err != nil {
		t.Fatal(err)
	}
	m := plan.Mismatches()
	if len(m) != 1 || !strings.HasSuffix(m[0], "expected 1, found 2 at "+a+":1,3") {
		t.Errorf("mismatches: %q", m)
	}
}

func TestLooseHint(t *testing.T) {
	dir := writeFiles(t, map[string]string{"a.go": "func f() {\n\tif x {\n\t\treturn\n\t}\n}\n"})
	a := filepath.Join(dir, "a.go")
	plan, err := Build([]Edit{{Files: []string{a}, Old: "    if x {\n        return", New: "", Want: 1, Origin: "OLD"}})
	if err != nil {
		t.Fatal(err)
	}
	m := plan.Mismatches()
	if len(m) != 1 || !strings.Contains(m[0], "matches ignoring indentation/whitespace at "+a+":2") {
		t.Errorf("mismatches: %q", m)
	}
}

func TestNearMissWhenTooFew(t *testing.T) {
	dir := writeFiles(t, map[string]string{"a.go": "const MaxAmount = 10000\n", "b.go": "const (\n\tMaxAmount  = 10000\n\tMin        = 1\n)\n"})
	a, b := filepath.Join(dir, "a.go"), filepath.Join(dir, "b.go")
	plan, err := Build([]Edit{{Files: []string{a, b}, Old: "MaxAmount = 10000", New: "MaxAmount = 20000", Want: 2, Origin: "OLD"}})
	if err != nil {
		t.Fatal(err)
	}
	m := plan.Mismatches()
	want := "expected 2, found 1 at " + a + ":1; near-misses ignoring indentation/whitespace at " + b + ":2"
	if len(m) != 1 || !strings.HasSuffix(m[0], want) {
		t.Errorf("mismatches: %q", m)
	}
}

func TestFirstLineHint(t *testing.T) {
	dir := writeFiles(t, map[string]string{"a.txt": "alpha\nbeta\n"})
	a := filepath.Join(dir, "a.txt")
	plan, err := Build([]Edit{{Files: []string{a}, Old: "alpha\ngamma", New: "", Want: 1, Origin: "OLD"}})
	if err != nil {
		t.Fatal(err)
	}
	if m := plan.Mismatches(); len(m) != 1 || !strings.Contains(m[0], `first line "alpha" found at `+a+":1") {
		t.Errorf("mismatches: %q", m)
	}
}

func TestChangedOnDiskAborts(t *testing.T) {
	dir := writeFiles(t, map[string]string{"a.txt": "foo\n", "b.txt": "foo\n"})
	a, b := filepath.Join(dir, "a.txt"), filepath.Join(dir, "b.txt")
	plan, err := Build([]Edit{{Files: []string{a, b}, Old: "foo", New: "bar", Want: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("foo changed by someone\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Write(); err == nil || !strings.Contains(err.Error(), "changed on disk") {
		t.Fatalf("want changed-on-disk error, got %v", err)
	}
	if got := readFile(t, a); got != "foo\n" {
		t.Errorf("a.txt written: %q", got)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Errorf("temp files left: %d entries", len(entries))
	}
}

func TestSymlinkSameFile(t *testing.T) {
	dir := writeFiles(t, map[string]string{"a.txt": "foo foo\n"})
	a, link := filepath.Join(dir, "a.txt"), filepath.Join(dir, "link.txt")
	if err := os.Symlink("a.txt", link); err != nil {
		t.Fatal(err)
	}
	plan, err := Build([]Edit{
		{Files: []string{a}, Old: "foo", New: "bar", Want: 2},
		{Files: []string{link}, Old: "bar bar", New: "baz", Want: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Write(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, a); got != "baz\n" {
		t.Errorf("a.txt: %q", got)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("symlink replaced by a file")
	}
}

func TestMissingFile(t *testing.T) {
	if _, err := Build([]Edit{{Files: []string{filepath.Join(t.TempDir(), "nope")}, Old: "a", Want: 1}}); err == nil {
		t.Fatal("want error for a missing file")
	}
}

func TestParseSpecFor(t *testing.T) {
	block := "<<<<<<< SEARCH\na\n=======\nb\n>>>>>>> REPLACE\n"
	edits, err := ParseSpecFor(strings.NewReader(block+"\n"+block), "x.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(edits) != 2 || edits[0].Files[0] != "x.go" || edits[1].Files[0] != "x.go" {
		t.Errorf("edits: %+v", edits)
	}
	edits, err = ParseSpecFor(strings.NewReader("./x.go\n"+block), "x.go")
	if err != nil || len(edits) != 1 || edits[0].Files[0] != "x.go" {
		t.Errorf("same path spelled ./x.go: %+v %v", edits, err)
	}
	_, err = ParseSpecFor(strings.NewReader(block+"y.go\n"+block), "x.go")
	if err == nil || !strings.Contains(err.Error(), `path "y.go" differs from the FILE argument "x.go"`) {
		t.Errorf("other path: %v", err)
	}
}

// Временный файл наследует ACL по умолчанию папки; у исходного его не было — после замены его быть не должно.
func TestWriteDropsInheritedDefaultACL(t *testing.T) {
	if _, err := os.Stat("/usr/bin/setfacl"); err != nil {
		t.Skip("setfacl is unavailable")
	}
	dir, err := os.MkdirTemp(".", ".acl-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(path, []byte("old\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("setfacl", "-d", "-m", "u:12345:rwx", dir).CombinedOutput(); err != nil {
		t.Fatalf("setfacl: %v: %s", err, out)
	}
	plan, err := Build([]Edit{{Files: []string{path}, Old: "old", New: "new", Want: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Write(); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("getfacl", "-cp", path).CombinedOutput()
	if err != nil {
		t.Fatalf("getfacl: %v: %s", err, out)
	}
	if strings.Contains(string(out), "12345") {
		t.Errorf("file got the directory's default ACL:\n%s", out)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o640 {
		t.Errorf("mode: %v %v", st.Mode(), err)
	}
}

// Особые биты режима (setuid, setgid, sticky) переживают замену, как и обычные.
func TestWriteKeepsSpecialModeBits(t *testing.T) {
	dir := writeFiles(t, map[string]string{"a.txt": "old\n"})
	path := filepath.Join(dir, "a.txt")
	want := fs.FileMode(0o755) | fs.ModeSetuid | fs.ModeSetgid
	if err := os.Chmod(path, want); err != nil {
		t.Fatal(err)
	}
	plan, err := Build([]Edit{{Files: []string{path}, Old: "old", New: "new", Want: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Write(); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky); got != want {
		t.Errorf("mode %v, want %v", got, want)
	}
}
