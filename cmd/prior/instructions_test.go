package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func searchInstructions(t *testing.T, home string, words ...string) string {
	t.Helper()
	q, err := newQuery(words)
	if err != nil {
		t.Fatal(err)
	}
	roots, notes := instructionRoots(home)
	var out strings.Builder
	instructions(&out, q, roots, home, notes)
	return out.String()
}

func TestInstructionFileKindsAndPaths(t *testing.T) {
	home := t.TempDir()
	for path, text := range map[string]string{
		".claude/CLAUDE.md":               "- rule insalpha1 here\n",
		".claude/skills/one/SKILL.md":     "- rule insalpha2 here\n",
		".claude/skills/one/extra.md":     "- rule insalpha3 here\n",
		"CLAUDE.md":                       "- rule insalpha4 here\n",
		"AGENTS.md":                       "- rule insalpha12 here\n",
		"work/CLAUDE.md":                  "- rule insalpha5 here\n",
		"work/project/CLAUDE.md":          "- rule insalpha6 here\n",
		"work/project/AGENTS.md":          "- rule insalpha7 here\n",
		"git/repo/CLAUDE.md":              "- rule insalpha8 here\n",
		"git/repo/.claude/rules/style.md": "- rule insalpha9 here\n",
		"work/.claude/rules/work.txt":     "- rule insalpha10 here\n",
		".claude/rules/local.yaml":        "- rule insalpha11 here\n",
		"other/CLAUDE.md":                 "- rule insalpha13 out of scope\n",
	} {
		write(t, filepath.Join(home, path), text)
	}
	out := searchInstructions(t, home, "insalpha")
	for _, want := range []string{
		".claude/CLAUDE.md:1", ".claude/skills/one/SKILL.md:1", ".claude/skills/one/extra.md:1",
		"CLAUDE.md:1", "AGENTS.md:1", "work/CLAUDE.md:1", "work/project/CLAUDE.md:1", "work/project/AGENTS.md:1",
		"git/repo/CLAUDE.md:1", "git/repo/.claude/rules/style.md:1", "work/.claude/rules/work.txt:1",
		".claude/rules/local.yaml:1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "insalpha13") {
		t.Fatal("the home directory was incorrectly searched as a project")
	}
	for _, want := range []string{"\nCLAUDE.md:1:", "\nAGENTS.md:1:", "\nwork/CLAUDE.md:1:"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing exact root path %q:\n%s", want, out)
		}
	}
	roots, _ := instructionRoots(home)
	files, _ := instructionFiles(roots)
	if !slices.ContainsFunc(files, func(file instructionFile) bool {
		return file.path == filepath.Join(home, "work", ".claude", "rules", "work.txt")
	}) {
		t.Fatal("non-Markdown rule file was not enumerated")
	}
}

func TestInstructionRankingLimitAndPathOrder(t *testing.T) {
	home := t.TempDir()
	for i := range 60 {
		write(t, filepath.Join(home, ".claude", "skills", fmt.Sprintf("a%02d", i), "SKILL.md"), "- rule rankonly\n")
	}
	write(t, filepath.Join(home, "work", "zz", ".claude", "rules", "rule.md"), "- rule rankone ranktwo\n")
	out := searchInstructions(t, home, "rankone", "ranktwo", "rankonly")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if !strings.Contains(lines[0], "work/zz/.claude/rules/rule.md:1") {
		t.Fatalf("highest word count did not rank first:\n%s", out)
	}
	if strings.Contains(out, "a59/SKILL.md") || !strings.Contains(out, "a00/SKILL.md") {
		t.Fatal("equal-ranked files are not selected in path order")
	}
	if !strings.Contains(out, "лимит 40") || strings.Count(out, ":1:- rule rank") != maxInstructionLines {
		t.Fatalf("section does not honor its hit line cap: %d hits\n%s", strings.Count(out, ":1:- rule rank"), out)
	}

	tieHome := t.TempDir()
	write(t, filepath.Join(tieHome, ".claude", "skills", "z", "SKILL.md"), "- rule tieword\n")
	write(t, filepath.Join(tieHome, ".claude", "skills", "a", "SKILL.md"), "- rule tieword\n")
	tie := searchInstructions(t, tieHome, "tieword")
	if strings.Index(tie, ".claude/skills/a/SKILL.md") > strings.Index(tie, ".claude/skills/z/SKILL.md") {
		t.Fatalf("equal scores are not ordered by path:\n%s", tie)
	}
}

func TestInstructionRankingCombinesRuleAndWhyWords(t *testing.T) {
	home := t.TempDir()
	for i := range 40 {
		write(t, filepath.Join(home, ".claude", "skills", fmt.Sprintf("a%02d", i), "SKILL.md"), "- rule splitone only\n")
	}
	write(t, filepath.Join(home, ".claude", "skills", "zz", "SKILL.md"), "- rule splitone\n  Зачем: splittwo\n")
	out := searchInstructions(t, home, "splitone", "splittwo")
	if !strings.HasPrefix(out, ".claude/skills/zz/SKILL.md:1:- rule splitone") || !strings.Contains(out, ".claude/skills/zz/SKILL.md:2:  Зачем: splittwo") {
		t.Fatalf("rule and explanation words were not combined ahead of the limit:\n%s", out)
	}
}

func TestInstructionPathTieBreakUsesPath(t *testing.T) {
	q, err := newQuery([]string{"tiesort"})
	if err != nil {
		t.Fatal(err)
	}
	docs := []instructionDoc{
		{path: "z-last/CLAUDE.md", lines: []instructionLine{{number: 1, text: "- rule tiesort"}}},
		{path: "a-first/CLAUDE.md", lines: []instructionLine{{number: 1, text: "- rule tiesort"}}},
	}
	hits := instructionMatches(q, docs, nil)
	if len(hits) != 2 || hits[0].path != "a-first/CLAUDE.md" || hits[1].path != "z-last/CLAUDE.md" {
		t.Fatalf("equal scores were not ordered by path: %#v", hits)
	}
}

func TestInstructionFragmentFilteringAndDeduplication(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, ".claude", "skills", "gamma", "SKILL.md"),
		"# gamma\n- rule  zqn3 spaced\n- rule zqh9 body here\n  Зачем: zqh9 why\n- rule beta line\n  Зачем: zqw4 only here\n- rule twinsame built\n")
	write(t, filepath.Join(home, ".claude", "skills", "joined", "SKILL.md"), "- rule joinrule\n  Зачем: joinwhy\n")
	write(t, filepath.Join(home, ".claude", "skills", "other", "SKILL.md"), "- rule twinsame built\n")
	write(t, filepath.Join(home, ".claude", "skills", "case", "SKILL.md"), "- rule CaseDedupe built\n")
	write(t, filepath.Join(home, ".claude", "codex", "frag", "skill", "zqh.md"),
		"---\nid: zqh9 header-only\nцель: zqh9 quoted text\nоснование: zqh9 citation\n---\n- rule zqh9 body here\n  Зачем: zqh9 why\n")
	write(t, filepath.Join(home, ".claude", "codex", "frag", "skill", "zqn.md"),
		"---\nid: zqn3 header-only\n---\n- rule   `zqn3`   spaced\n")
	write(t, filepath.Join(home, ".claude", "codex", "frag", "skill", "case.md"),
		"---\nid: casededupe header\n---\n- rule casededupe BUILT\n")
	write(t, filepath.Join(home, ".claude", "codex", "frag", "plain", "separator.md"),
		"- rule before separatorbody\n---\n- rule after separatorbody\n")
	write(t, filepath.Join(home, ".claude", "codex", "frag", ".claude-decisions.md", "decision.md"),
		"---\nid: zqd5 header\n---\n- rule zqd5 already in decisions\n")
	write(t, filepath.Join(home, ".claude", "codex", "frag", ".claude-CLAUDE.md", "record.md"),
		"---\nid: zqe5 header\nзапись: true\n---\n- rule zqe5 record only\n")
	write(t, filepath.Join(home, ".claude", "codex", "frag", ".claude-CLAUDE.md", "unique.md"),
		"---\nid: unique header must not match\n---\n- rule fragunique body\n")
	for query, expected := range map[string][2]string{
		"zqh9":   {".claude/skills/gamma/SKILL.md", "codex/frag"},
		"zqn3":   {".claude/skills/gamma/SKILL.md:2", "codex/frag"},
		"zqd5":   {"", "codex/frag"},
		"zqe5":   {"", "codex/frag"},
		"unique": {"codex/frag/", "header must not match"},
	} {
		want, unwanted := expected[0], expected[1]
		out := searchInstructions(t, home, query)
		if want != "" && !strings.Contains(out, want) {
			t.Errorf("%q: missing %q:\n%s", query, want, out)
		}
		if unwanted != "" && strings.Contains(out, unwanted) {
			t.Errorf("%q: unexpected %q:\n%s", query, unwanted, out)
		}
	}
	if got := strings.Count(searchInstructions(t, home, "twinsame"), "twinsame built"); got != 2 {
		t.Errorf("same rule in two built files lost a path: got %d rows", got)
	}
	if got := strings.Count(searchInstructions(t, home, "zqn3"), "zqn3"); got != 1 {
		t.Errorf("normalized fragment duplicate printed %d times, want 1", got)
	}
	caseOut := searchInstructions(t, home, "casededupe")
	if !strings.Contains(caseOut, ".claude/skills/case/SKILL.md") || strings.Contains(caseOut, "codex/frag/skill/case.md") {
		t.Errorf("case-only duplicate was not normalized in favor of the built file:\n%s", caseOut)
	}
	separatorOut := searchInstructions(t, home, "before")
	if !strings.Contains(separatorOut, "codex/frag/plain/separator.md:1:- rule before separatorbody") {
		t.Errorf("body text before an internal separator was lost:\n%s", separatorOut)
	}
	joinedOut := searchInstructions(t, home, "joinrule", "joinwhy")
	if got := strings.Count(joinedOut, "- rule joinrule"); got != 1 {
		t.Errorf("rule row matching with its explanation printed %d times, want 1:\n%s", got, joinedOut)
	}
}

func TestInstructionWhyContextAndSectionOrder(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, ".claude", "CLAUDE.md"), "- rule context-rule\n  Зачем: contextwhy reason\n")
	write(t, filepath.Join(home, ".claude", "said.tsv"), "time\tsession\tname\tproject\tscope\tkind\twords\tquote\tref\tsupersedes\n"+
		"2026-11-16T10:00:00+01:00\t12345678-0000\tmain\t~\tglobal\trule\tcontextwhy\tcontextwhy said\t12345678:1\t\n")
	out := runPrior(t, home, "contextwhy")
	said := strings.Index(out, "== сказанное пользователем")
	instructionsAt := strings.Index(out, "== инструкции")
	if said < 0 || instructionsAt < 0 || said >= instructionsAt {
		t.Fatalf("section order said=%d instructions=%d:\n%s", said, instructionsAt, out)
	}
	if !strings.Contains(out, "context-rule") || !strings.Contains(out, "Зачем: contextwhy") {
		t.Fatalf("missing rule context for a matching explanation:\n%s", out)
	}
	write(t, filepath.Join(home, ".claude", "CLAUDE.md"), "- rule blankcontext\n\n\n  Зачем: blankwhy\n")
	blanks := searchInstructions(t, home, "blankwhy")
	if !strings.Contains(blanks, "CLAUDE.md:1:- rule blankcontext") {
		t.Fatalf("rule context was lost across blank lines:\n%s", blanks)
	}
}

func TestInstructionSymlinkUsesTargetPath(t *testing.T) {
	home := t.TempDir()
	file := filepath.Join(home, ".claude", "CLAUDE.md")
	write(t, file, "- rule symlinkword\n")
	if err := os.Symlink("CLAUDE.md", filepath.Join(home, ".claude", "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	out := searchInstructions(t, home, "symlinkword")
	if strings.Count(out, "symlinkword") != 1 || !strings.Contains(out, ".claude/CLAUDE.md:1") || strings.Contains(out, "AGENTS.md") {
		t.Fatalf("symlink alias was not collapsed to its target:\n%s", out)
	}
}

func TestInstructionWorkspaceSingleProjectAndSymlinkProject(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, "work", "solo", "CLAUDE.md"), "- rule projectword work\n")
	write(t, filepath.Join(home, "git", "target", "CLAUDE.md"), "- rule projectword git\n")
	if err := os.Symlink("target", filepath.Join(home, "git", "linked")); err != nil {
		t.Fatal(err)
	}
	out := searchInstructions(t, home, "projectword")
	if !strings.Contains(out, "work/solo/CLAUDE.md:1") || !strings.Contains(out, "git/target/CLAUDE.md:1") {
		t.Fatalf("single project or symlink target missing:\n%s", out)
	}
	if strings.Contains(out, "git/linked/") || strings.Count(out, "projectword") != 2 {
		t.Fatalf("project symlink was duplicated or named by its alias:\n%s", out)
	}
}

func TestInstructionWorkspaceSymlinkOutsideGit(t *testing.T) {
	home := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "project")
	write(t, filepath.Join(target, "CLAUDE.md"), "- rule externalproject\n")
	link := filepath.Join(home, "git", "linked")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(home, filepath.Join(target, "CLAUDE.md"))
	if err != nil {
		t.Fatal(err)
	}
	out := searchInstructions(t, home, "externalproject")
	if !strings.Contains(out, filepath.ToSlash(rel)+":1:- rule externalproject") || strings.Contains(out, "git/linked/") {
		t.Fatalf("project symlink outside git did not use its target path %q:\n%s", filepath.ToSlash(rel), out)
	}
}

func TestInstructionUnreadableFileAndDirectoryAreReported(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission checks are bypassed for root")
	}
	home := t.TempDir()
	lockedFile := filepath.Join(home, ".claude", "skills", "locked", "SKILL.md")
	write(t, lockedFile, "- rule blockedfile\n")
	if err := os.Chmod(lockedFile, 0); err != nil {
		t.Fatal(err)
	}
	lockedDir := filepath.Join(home, ".claude", "skills", "closed")
	write(t, filepath.Join(lockedDir, "SKILL.md"), "- rule blockeddir\n")
	if err := os.Chmod(lockedDir, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(lockedDir, 0o755)
	out := searchInstructions(t, home, "blocked")
	if !strings.Contains(out, "инструкция не прочиталась") || !strings.Contains(out, "каталог инструкций не прочитался") {
		t.Fatalf("permission failures are not reported:\n%s", out)
	}
}

func TestNormalizeInstructionMatchesFieldsJoin(t *testing.T) {
	reference := func(line string) string {
		return strings.ToLower(strings.Join(strings.Fields(strings.ReplaceAll(line, "`", "")), " "))
	}
	for _, line := range []string{
		"", "  ", "Rule `code` here", "\tПравило С  ПРОБЕЛАМИ и\u0085всё ", "İSTANBUL", "\xd0`\xb6", "a\xffb", "ǅ ǈ Σ",
	} {
		if got, want := normalizeInstruction(line), reference(line); got != want {
			t.Errorf("normalizeInstruction(%q) = %q, want %q", line, got, want)
		}
	}
	for b := range 256 {
		for c := range 256 {
			line := string([]byte{'A', byte(b), byte(c), ' ', 0xd0})
			if got, want := normalizeInstruction(line), reference(line); got != want {
				t.Fatalf("normalizeInstruction(%q) = %q, want %q", line, got, want)
			}
		}
	}
	for r := rune(0); r <= 0x10FFFF; r++ {
		line := "x" + string(r) + "Y"
		if got, want := normalizeInstruction(line), reference(line); got != want {
			t.Fatalf("normalizeInstruction(%q) = %q, want %q", line, got, want)
		}
	}
}

// A linked file inside a walked instruction tree is named by its target, as a linked root file is.
func TestInstructionTreeLinkedFileUsesTargetPath(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, "notes", "shared.md"), "- rule treelinkword\n")
	if err := os.MkdirAll(filepath.Join(home, ".claude", "skills", "one"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "notes", "shared.md"), filepath.Join(home, ".claude", "skills", "one", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	out := searchInstructions(t, home, "treelinkword")
	if !strings.Contains(out, "notes/shared.md:1") || strings.Contains(out, "skills/one/SKILL.md") {
		t.Fatalf("linked file was not named by its target:\n%s", out)
	}
}
