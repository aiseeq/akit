package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// missingSkill — скилл из списка не нашёлся в источнике: выгрузка была бы неполной.
type missingSkill struct{ name string }

func (m missingSkill) Error() string { return "missing skill: " + m.name }

func export(opts options, stdout io.Writer) error {
	out, err := filepath.Abs(opts.out)
	if err != nil {
		return fmt.Errorf("OUT_DIR: %w", err)
	}
	src, err := filepath.Abs(opts.srcHome)
	if err != nil {
		return fmt.Errorf("--src-home: %w", err)
	}
	if out == "/" || out == src {
		return errors.New("OUT_DIR must not be / or the source home")
	}
	m, err := readManifest(opts.rules)
	if err != nil {
		return err
	}
	if err := assemble(out, src, opts.rules, m); err != nil {
		return err
	}
	files, err := textFiles(out)
	if err != nil {
		return err
	}
	readme := filepath.Join(out, "README.md")
	for _, f := range files {
		if f == readme {
			continue // шаблон README — как есть, проверка на личные данные его видит
		}
		if err := rewrite(f, m); err != nil {
			return err
		}
	}
	hits, err := findPII(out, files, m.pii)
	if err != nil {
		return err
	}
	for _, h := range hits {
		fmt.Fprintln(stdout, h)
	}
	if len(hits) > 0 {
		fmt.Fprintf(stdout, "claude-rules-export: %d private data hit(s) in %s\n", len(hits), out)
		return errDirty
	}
	fmt.Fprintf(stdout, "claude-rules-export: %d file(s) in %s, no private data\n", len(files), out)
	return nil
}

// assemble собирает выгрузку из источника: скиллы, правила рабочего пространства, разделы общего CLAUDE.md, README.
func assemble(out, src, rules string, m manifest) error {
	if err := clearDir(out); err != nil {
		return err
	}
	for _, s := range m.skills {
		if strings.Contains(s, "/") || s == "." || s == ".." {
			return fmt.Errorf("invalid skill name in manifest: %s", s)
		}
		dir := filepath.Join(src, ".claude", "skills", s)
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			return missingSkill{s}
		}
		if err := copyTree(dir, filepath.Join(out, "skills", s)); err != nil {
			return err
		}
	}
	if err := copyFile(filepath.Join(src, "work", "CLAUDE.md"), filepath.Join(out, "work", "CLAUDE.md")); err != nil {
		return err
	}
	global, err := os.ReadFile(filepath.Join(src, ".claude", "CLAUDE.md"))
	if err != nil {
		return fmt.Errorf("global rules: %w", err)
	}
	if err := writeFile(filepath.Join(out, "global", "CLAUDE.md"), []byte(sections(string(global), m.sections)), 0o644); err != nil {
		return err
	}
	return copyFile(filepath.Join(rules, "README.template.md"), filepath.Join(out, "README.md"))
}

// sections — разделы CLAUDE.md из списка. Заголовок раздела — непустая строка не с «-» и не с пробела; раздел
// идёт до следующего заголовка.
func sections(text string, wanted map[string]bool) string {
	var b strings.Builder
	active := false
	for l := range strings.Lines(text) {
		line := strings.TrimSuffix(l, "\n")
		if strings.TrimSpace(line) != "" && line[0] != '-' && line[0] != ' ' {
			active = wanted[line]
		}
		if active {
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}

// rewrite делает замены в каждой строке файла по порядку, затем вычёркивает строки с подстрокой из списка.
func rewrite(path string, m manifest) error {
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("rewrite: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("rewrite: %w", err)
	}
	var b strings.Builder
	for l := range strings.Lines(string(data)) {
		line := strings.TrimSuffix(l, "\n")
		for _, r := range m.replace {
			line = strings.ReplaceAll(line, r.from, r.to)
		}
		if !dropped(line, m.drop) {
			b.WriteString(line + "\n")
		}
	}
	return writeFile(path, []byte(b.String()), st.Mode().Perm())
}

func dropped(line string, drop []string) bool {
	for _, d := range drop {
		if strings.Contains(line, d) {
			return true
		}
	}
	return false
}

// findPII — совпадения шаблонов личных данных, кроме разрешённых значений: «файл:строка: вид».
func findPII(root string, files []string, pii []piiRule) ([]string, error) {
	var hits []string
	for _, r := range pii {
		for _, f := range files {
			data, err := os.ReadFile(f)
			if err != nil {
				return nil, fmt.Errorf("private data check: %w", err)
			}
			rel := strings.TrimPrefix(f, root+"/")
			n := 0
			for l := range strings.Lines(string(data)) {
				n++
				for range r.forbidden(l) {
					hits = append(hits, fmt.Sprintf("%s:%d: %s", rel, n, r.kind))
				}
			}
		}
	}
	return hits, nil
}
