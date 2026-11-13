package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// missingSkill — скилл из списка не нашёлся в источнике: выгрузка была бы неполной.
type missingSkill struct{ name string }

func (m missingSkill) Error() string { return "missing skill: " + m.name }

type missingManifestEntry struct{ file, line string }

func (m missingManifestEntry) Error() string {
	return fmt.Sprintf("manifest %s: no match for %q", m.file, m.line)
}

type rewriteMatches struct {
	replace []bool
	drop    []bool
}

func export(opts options, stdout io.Writer) (err error) {
	// сверяются, читаются и очищаются одни и те же пути — разрешённые так, как их поймёт система
	out, err := physical(opts.out)
	if err != nil {
		return fmt.Errorf("OUT_DIR: %w", err)
	}
	src, err := physical(opts.srcHome)
	if err != nil {
		return fmt.Errorf("--src-home: %w", err)
	}
	rules, err := physical(opts.rules)
	if err != nil {
		return fmt.Errorf("--rules: %w", err)
	}
	m, err := readManifest(rules)
	if err != nil {
		return err
	}
	if err := checkOut(out, src, rules, m); err != nil {
		return err
	}
	// выгрузка собирается и проверяется в своей папке рядом со списком: ошибка сборки не трогает OUT_DIR, а сборку
	// оборванного прогона следующий убирает
	stage, err := stageDir(out)
	if err != nil {
		return err
	}
	defer func() {
		if rerr := os.RemoveAll(stage); rerr != nil && err == nil {
			err = fmt.Errorf("remove stage: %w", rerr)
		}
	}()
	if err := assemble(stage, src, rules, m); err != nil {
		return err
	}
	files, err := textFiles(stage)
	if err != nil {
		return err
	}
	readme := filepath.Join(stage, "README.md")
	matches, err := rewriteFiles(files, readme, m)
	if err != nil {
		return err
	}
	hits, err := findPII(stage, files, m.pii)
	if err != nil {
		return err
	}
	if err := replaceExport(out, stage); err != nil {
		return err
	}
	printReplaceWarnings(stdout, m, matches)
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

func rewriteFiles(files []string, readme string, m manifest) (rewriteMatches, error) {
	matches := rewriteMatches{replace: make([]bool, len(m.replace)), drop: make([]bool, len(m.drop))}
	for _, f := range files {
		if f == readme {
			continue // шаблон README — как есть, проверка на личные данные его видит
		}
		if err := rewrite(f, m, &matches); err != nil {
			return matches, err
		}
	}
	for i, found := range matches.drop {
		if !found {
			return matches, missingManifestEntry{file: "drop-lines.txt", line: m.drop[i]}
		}
	}
	return matches, nil
}

func printReplaceWarnings(stdout io.Writer, m manifest, matches rewriteMatches) {
	for i, found := range matches.replace {
		if !found {
			fmt.Fprintf(stdout, "claude-rules-export: warning: replace.tsv: no match for %q\n", m.replace[i].from)
		}
	}
}

// assemble собирает выгрузку из источника: скиллы, правила рабочего пространства, разделы общего CLAUDE.md, README.
func assemble(out, src, rules string, m manifest) error {
	for _, s := range m.skills {
		if strings.Contains(s, "/") || s == "." || s == ".." || strings.EqualFold(s, ".git") {
			return fmt.Errorf("invalid skill name in manifest: %s", s)
		}
		dir := filepath.Join(src, ".claude", "skills", s)
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			return missingSkill{s}
		}
		// папка скилла может быть ссылкой: в выгрузку — её содержимое, а не ссылка, висящая вне home
		real, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return fmt.Errorf("skill %s: %w", s, err)
		}
		if err := copyTree(real, filepath.Join(out, "skills", s)); err != nil {
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
	selected, found := sections(string(global), m.sections)
	names := slices.Sorted(maps.Keys(m.sections))
	for _, name := range names {
		if !found[name] {
			return missingManifestEntry{file: "global-sections.txt", line: name}
		}
	}
	if err := writeFile(filepath.Join(out, "global", "CLAUDE.md"), []byte(selected), 0o644); err != nil {
		return err
	}
	return copyFile(filepath.Join(rules, "README.template.md"), filepath.Join(out, "README.md"))
}

// sections — разделы CLAUDE.md из списка. Заголовок раздела — непустая строка не с «-» и не с пробела; раздел
// идёт до следующего заголовка.
func sections(text string, wanted map[string]bool) (string, map[string]bool) {
	var b strings.Builder
	found := map[string]bool{}
	active := false
	for l := range strings.Lines(text) {
		line := strings.TrimSuffix(l, "\n")
		if strings.TrimSpace(line) != "" && line[0] != '-' && line[0] != ' ' {
			active = wanted[line]
			if active {
				found[line] = true
			}
		}
		if active {
			b.WriteString(line + "\n")
		}
	}
	return b.String(), found
}

// rewrite делает замены в каждой строке файла по порядку, затем вычёркивает строки с подстрокой из списка.
func rewrite(path string, m manifest, matches *rewriteMatches) error {
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
		for i, r := range m.replace {
			changed := strings.ReplaceAll(line, r.from, r.to)
			if changed != line {
				matches.replace[i] = true
			}
			line = changed
		}
		removed := false
		for i, d := range m.drop {
			if strings.Contains(line, d) {
				matches.drop[i] = true
				removed = true
			}
		}
		if !removed {
			b.WriteString(line + "\n")
		}
	}
	// копия файла источника без права записи (0444) пишется с ним и получает прежние права обратно
	if err := os.Chmod(path, st.Mode().Perm()|0o200); err != nil {
		return fmt.Errorf("rewrite: %w", err)
	}
	if err := putFile(path, []byte(b.String()), st.Mode().Perm(), os.O_TRUNC); err != nil {
		return err
	}
	if err := os.Chmod(path, st.Mode()&modeBits); err != nil {
		return fmt.Errorf("rewrite: %w", err)
	}
	return nil
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

// under — путь p относительно dir, если p совпадает с dir или лежит внутри него, по тексту.
func under(p, dir string) (string, bool) {
	r, err := filepath.Rel(dir, p)
	return r, err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator))
}

// checkOut — ранний понятный отказ: OUT_DIR равен читаемому или содержит его. Пути сравниваются после разрешения
// симлинков. Лежать внутри читаемой папки можно (обычный OUT_DIR — ~/work/<папка>). Безопасность на этом не
// держится: под путём, которого сверка не узнаёт (bind mount, overlay, /proc), выгрузка удаляет только записи
// своего списка (outdir.go).
func checkOut(out, src, rules string, m manifest) error {
	reads, err := readPaths(src, rules, m)
	if err != nil {
		return err
	}
	for _, read := range reads {
		r, err := resolved(read)
		if err != nil {
			return err
		}
		if _, contains := under(r, out); contains {
			return fmt.Errorf("OUT_DIR must not contain what it reads: %s", read)
		}
	}
	return nil
}

// physical — абсолютный путь, как его поймёт система. filepath.Abs чистит текст до разрешения ссылок и убирает
// «..» вместе со ссылкой перед ним, а система идёт от цели ссылки: здесь путь не чистится до EvalSymlinks.
func physical(p string) (string, error) {
	if !filepath.IsAbs(p) {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("current folder: %w", err)
		}
		p = wd + string(filepath.Separator) + p
	}
	return resolved(p)
}

// resolved — путь с разрешёнными симлинками; у несуществующего — по ближайшему существующему предку. Предок
// берётся срезом текста, не filepath.Dir: тот чистит «..» по тексту.
func resolved(p string) (string, error) {
	r, err := filepath.EvalSymlinks(p)
	if err == nil {
		return r, nil
	}
	sep := string(filepath.Separator)
	trimmed := strings.TrimRight(p, sep)
	i := strings.LastIndex(trimmed, sep)
	base := trimmed[i+1:]
	if !errors.Is(err, fs.ErrNotExist) || i < 0 || base == "." || base == ".." {
		return "", err
	}
	parent := trimmed[:i]
	if parent == "" {
		parent = sep
	}
	if r, err = resolved(parent); err != nil {
		return "", err
	}
	return filepath.Join(r, base), nil
}

// readPaths — всё, что выгрузка читает или до чего дотянется из источника: home, его .claude и work, оба
// CLAUDE.md, папка правил, её манифест и шаблон, папка каждого скилла и цели ссылок внутри скиллов. Ссылкой
// может быть любой из них: сравнивается разрешённый путь.
func readPaths(src, rules string, m manifest) ([]string, error) {
	reads := []string{src, filepath.Join(src, ".claude"), filepath.Join(src, "work"), rules,
		filepath.Join(src, ".claude", "CLAUDE.md"), filepath.Join(src, "work", "CLAUDE.md"),
		filepath.Join(rules, "manifest"), filepath.Join(rules, "README.template.md")}
	manifestDir, err := resolved(filepath.Join(rules, "manifest"))
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(manifestDir)
	if err != nil {
		return nil, fmt.Errorf("--rules manifest: %w", err)
	}
	for _, e := range entries {
		reads = append(reads, filepath.Join(rules, "manifest", e.Name()))
	}
	for _, s := range m.skills {
		dir := filepath.Join(src, ".claude", "skills", s)
		reads = append(reads, dir)
		links, err := skillLinks(dir)
		if err != nil {
			return nil, err
		}
		reads = append(reads, links...)
	}
	return reads, nil
}

// skillLinks — ссылки внутри папки скилла (сама папка может быть ссылкой); нет папки — пусто, её отсутствие
// назовёт сборка.
func skillLinks(dir string) ([]string, error) {
	root, err := resolved(dir)
	if err != nil {
		return nil, err
	}
	var links []string
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) && p == root {
			return filepath.SkipAll
		}
		if err != nil {
			return fmt.Errorf("skill %s: %w", dir, err)
		}
		if d.Type()&fs.ModeSymlink != 0 {
			links = append(links, p)
		}
		return nil
	})
	return links, err
}
