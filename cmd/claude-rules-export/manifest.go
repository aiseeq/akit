package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// manifest — данные выгрузки из папки --rules.
type manifest struct {
	skills   []string
	sections map[string]bool
	replace  []replacement
	drop     []string
	pii      []piiRule
}

type replacement struct{ from, to string }

// piiRule — вид личных данных: шаблон и разрешённые значения (совпадение целиком).
type piiRule struct {
	kind      string
	re, allow *regexp.Regexp
}

// forbidden — совпадения шаблона в строке, кроме разрешённых.
func (r piiRule) forbidden(line string) []string {
	var out []string
	for _, m := range r.re.FindAllString(line, -1) {
		if r.allow == nil || !r.allow.MatchString(m) {
			out = append(out, m)
		}
	}
	return out
}

func readManifest(rules string) (manifest, error) {
	var m manifest
	dir := filepath.Join(rules, "manifest")
	var err error
	if m.skills, err = lines(filepath.Join(dir, "skills.txt"), false); err != nil {
		return m, err
	}
	secs, err := lines(filepath.Join(dir, "global-sections.txt"), false)
	if err != nil {
		return m, err
	}
	m.sections = map[string]bool{}
	for _, s := range secs {
		m.sections[s] = true
	}
	if m.drop, err = lines(filepath.Join(dir, "drop-lines.txt"), true); err != nil {
		return m, err
	}
	if m.replace, err = readReplace(filepath.Join(dir, "replace.tsv")); err != nil {
		return m, err
	}
	m.pii, err = readPII(filepath.Join(dir, "pii.tsv"))
	return m, err
}

func readReplace(path string) ([]replacement, error) {
	rows, err := lines(path, true)
	if err != nil {
		return nil, err
	}
	var out []replacement
	for _, r := range rows {
		from, to, ok := strings.Cut(r, "\t")
		if !ok {
			return nil, fmt.Errorf("invalid replacement row in %s: no tab", path)
		}
		out = append(out, replacement{from, to})
	}
	return out, nil
}

func readPII(path string) ([]piiRule, error) {
	rows, err := lines(path, true)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%s: no rules — the check would pass anything", path)
	}
	var out []piiRule
	for _, row := range rows {
		f := strings.Split(row, "\t")
		if len(f) < 2 || len(f) > 3 {
			return nil, fmt.Errorf("%s: row %q: need KIND<TAB>REGEXP[<TAB>ALLOWED]", path, row)
		}
		r := piiRule{kind: f[0]}
		if r.re, err = regexp.Compile(f[1]); err != nil {
			return nil, fmt.Errorf("%s: %s: %w", path, f[0], err)
		}
		if len(f) == 3 && f[2] != "" {
			if r.allow, err = regexp.Compile("^(?:" + f[2] + ")$"); err != nil {
				return nil, fmt.Errorf("%s: %s allowed: %w", path, f[0], err)
			}
		}
		out = append(out, r)
	}
	return out, nil
}

// lines — непустые строки файла; comments — пропускать и строки с «#» в начале.
func lines(path string, comments bool) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	var out []string
	for l := range strings.Lines(string(data)) {
		l = strings.TrimRight(l, "\r\n")
		if l == "" || (comments && strings.HasPrefix(l, "#")) {
			continue
		}
		out = append(out, l)
	}
	return out, nil
}
