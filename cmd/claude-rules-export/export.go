package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// missingSkill — скилл из списка не нашёлся в источнике: выгрузка была бы неполной.
type missingSkill struct{ name string }

func (m missingSkill) Error() string { return "missing skill: " + m.name }

func export(opts options, stdout io.Writer) error {
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
	if err := assemble(out, src, rules, m); err != nil {
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

// within — путь p совпадает с dir или лежит внутри него. Кроме текста разрешённых путей сравниваются сами папки:
// bind mount показывает ту же папку под другим путём, и по тексту её не узнать.
func within(p, dir string) (bool, error) {
	if _, ok := under(p, dir); ok {
		return true, nil
	}
	d, err := os.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat %s: %w", dir, err)
	}
	for q := p; ; q = filepath.Dir(q) {
		s, err := os.Stat(q)
		if err == nil && os.SameFile(s, d) {
			return true, nil
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return false, fmt.Errorf("stat %s: %w", q, err)
		}
		if q == filepath.Dir(q) {
			return false, nil
		}
	}
}

// checkOut — OUT_DIR очищается: папка, где лежит читаемое, или папка внутри читаемого стёрла бы источник. Пути
// сравниваются после разрешения симлинков: ссылка на читаемое ведёт туда же.
func checkOut(out, src, rules string, m manifest) error {
	reads, err := readPaths(src, rules, m)
	if err != nil {
		return err
	}
	o, err := resolved(out)
	if err != nil {
		return fmt.Errorf("OUT_DIR: %w", err)
	}
	mounts, err := readMounts()
	if err != nil {
		return err
	}
	outs := aliases(o, mounts)
	for _, read := range reads {
		r, err := resolved(read)
		if err != nil {
			return err
		}
		for _, ra := range aliases(r, mounts) {
			for _, oa := range outs {
				bad, err := overlaps(ra, oa, read != src)
				if err != nil {
					return err
				}
				if bad {
					return fmt.Errorf("OUT_DIR must not contain or be inside what it reads: %s", read)
				}
			}
		}
	}
	// сверка путей называет, что задето; содержимое — рубеж для путей, которых сверка не узнаёт
	return checkOutContent(o, mounts)
}

// exportEntries — всё, что выгрузка пишет в OUT_DIR верхним уровнем; .git — история выгрузки, её не трогают.
var exportEntries = map[string]bool{".git": true, "global": true, "README.md": true, "skills": true, "work": true}

// checkOutContent — очищается только то, что выгрузка пишет сама: в OUT_DIR нет ничего другого и ни одной точки
// монтирования внутри. Так источник не стереть ни под каким другим путём — ссылкой, bind mount, overlay, /proc:
// в нём лежит своё, и выгрузка откажет, не тронув ни файла.
func checkOutContent(out string, mounts []mount) error {
	entries, err := os.ReadDir(out)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("list OUT_DIR: %w", err)
	}
	for _, e := range entries {
		if !exportEntries[e.Name()] {
			return fmt.Errorf("OUT_DIR holds %s, which the export does not write: give an empty folder or a previous export", e.Name())
		}
	}
	for _, m := range mounts {
		if r, ok := under(m.point, out); ok && r != "." {
			return fmt.Errorf("OUT_DIR has a mount point inside: %s", m.point)
		}
	}
	return nil
}

// overlaps — OUT_DIR o содержит читаемое r или (если inside) лежит внутри него.
func overlaps(r, o string, inside bool) (bool, error) {
	if contains, err := within(r, o); err != nil || contains {
		return contains, err
	}
	if !inside {
		return false, nil
	}
	return within(o, r)
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
