package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type instructionRoot struct {
	path     string
	fragment bool
	anyFile  bool
}

type instructionFile struct {
	path     string
	fragment bool
}

type instructionLine struct {
	number int
	text   string
}

type instructionDoc struct {
	path     string
	fragment bool
	lines    []instructionLine
}

type instructionHit struct {
	path      string
	firstLine int
	words     int
	rows      []instructionLine
}

func addInstructionFileRoots(roots *[]instructionRoot, dir string) {
	for _, name := range []string{"CLAUDE.md", "AGENTS.md"} {
		*roots = append(*roots, instructionRoot{path: filepath.Join(dir, name)})
	}
}

func addInstructionTree(roots *[]instructionRoot, path string, fragment, anyFile bool) {
	*roots = append(*roots, instructionRoot{path: path, fragment: fragment, anyFile: anyFile})
}

// instructionRoots enumerates global files and workspace files, with projects
// limited to one directory below work/ and git/.
func instructionRoots(home string) ([]instructionRoot, []string) {
	var roots []instructionRoot
	var notes []string
	addInstructionFileRoots(&roots, home)
	claude := filepath.Join(home, ".claude")
	addInstructionFileRoots(&roots, claude)
	addInstructionTree(&roots, filepath.Join(claude, "skills"), false, false)
	addInstructionTree(&roots, filepath.Join(claude, "codex", "frag"), true, false)
	addInstructionTree(&roots, filepath.Join(claude, "rules"), false, true)
	for _, base := range []string{filepath.Join(home, "work"), filepath.Join(home, "git")} {
		notes = append(notes, addWorkspaceInstructionRoots(&roots, base)...)
	}
	return roots, uniqueStrings(notes)
}

func addWorkspaceInstructionRoots(roots *[]instructionRoot, base string) []string {
	addInstructionFileRoots(roots, base)
	addInstructionTree(roots, filepath.Join(base, ".claude", "rules"), false, true)
	entries, err := os.ReadDir(base)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return []string{fmt.Sprintf("(каталог инструкций не прочитался: %s: %v)", base, err)}
	}
	var notes []string
	for _, entry := range entries {
		project, isDir, statErr := workspaceProjectDir(base, entry)
		if statErr != nil {
			notes = append(notes, fmt.Sprintf("(каталог пространства не прочитался: %s: %v)", project, statErr))
		} else if isDir {
			addProjectInstructionRoots(roots, project)
		}
	}
	return notes
}

func workspaceProjectDir(base string, entry fs.DirEntry) (string, bool, error) {
	path := filepath.Join(base, entry.Name())
	if entry.IsDir() {
		return path, true, nil
	}
	if entry.Type()&fs.ModeSymlink == 0 {
		return path, false, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return path, false, fmt.Errorf("stat workspace project %s: %w", path, err)
	}
	return path, info.IsDir(), nil
}

func addProjectInstructionRoots(roots *[]instructionRoot, project string) {
	addInstructionFileRoots(roots, project)
	addInstructionTree(roots, filepath.Join(project, ".claude", "rules"), false, true)
}

func instructions(w io.Writer, q query, roots []instructionRoot, home string, notes []string) {
	files, fileNotes := instructionFiles(roots)
	allNotes := append(slicesClone(notes), fileNotes...)
	docs := readInstructionDocs(files, home, &allNotes)
	var builtLines = make(map[string]bool)
	for _, doc := range docs {
		if !doc.fragment {
			for _, line := range doc.lines {
				builtLines[normalizeInstruction(line.text)] = true
			}
		}
	}
	hits := instructionMatches(q, docs, builtLines)
	for _, note := range uniqueStrings(allNotes) {
		fmt.Fprintln(w, note)
	}
	shown := 0
	printed := make(map[string]bool)
	for _, hit := range hits {
		var rows []instructionLine
		for _, row := range hit.rows {
			key := fmt.Sprintf("%s:%d", hit.path, row.number)
			if !printed[key] {
				rows = append(rows, row)
			}
		}
		if len(rows) == 0 {
			continue
		}
		if shown+len(rows) > maxInstructionLines {
			fmt.Fprintf(w, "… ещё совпадения (лимит %d строк)\n", maxInstructionLines)
			break
		}
		for _, row := range rows {
			key := fmt.Sprintf("%s:%d", hit.path, row.number)
			printed[key] = true
			fmt.Fprintln(w, cut(fmt.Sprintf("%s:%d:%s", hit.path, row.number, row.text)))
			shown++
		}
	}
	if shown == 0 && len(allNotes) == 0 {
		fmt.Fprintln(w, "(нет)")
	}
}

func instructionFiles(roots []instructionRoot) ([]instructionFile, []string) {
	byPath := make(map[string]instructionFile)
	var notes []string
	add := func(path string, root instructionRoot) {
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				notes = append(notes, fmt.Sprintf("(путь инструкции не разрешился: %s: %v)", path, err))
			}
			return
		}
		real = filepath.Clean(real)
		if existing, ok := byPath[real]; !ok || (existing.fragment && !root.fragment) {
			byPath[real] = instructionFile{path: real, fragment: root.fragment}
		}
	}
	for _, root := range roots {
		info, err := os.Stat(root.path)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				notes = append(notes, fmt.Sprintf("(каталог инструкций не прочитался: %s: %v)", root.path, err))
			}
			continue
		}
		if !info.IsDir() {
			if filepath.Ext(root.path) == ".md" {
				add(root.path, root)
			}
			continue
		}
		walkErr := filepath.WalkDir(root.path, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				notes = append(notes, fmt.Sprintf("(каталог инструкций не прочитался: %s: %v)", path, err))
				return nil
			}
			if entry.IsDir() || (!root.anyFile && filepath.Ext(path) != ".md") {
				return nil
			}
			add(path, root)
			return nil
		})
		if walkErr != nil {
			notes = append(notes, fmt.Sprintf("(каталог инструкций не прочитался: %s: %v)", root.path, walkErr))
		}
	}
	files := make([]instructionFile, 0, len(byPath))
	for _, file := range byPath {
		files = append(files, file)
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].fragment != files[j].fragment {
			return !files[i].fragment
		}
		return files[i].path < files[j].path
	})
	return files, uniqueStrings(notes)
}

func readInstructionDocs(files []instructionFile, home string, notes *[]string) []instructionDoc {
	canonicalHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		*notes = append(*notes, fmt.Sprintf("(путь HOME не разрешился: %s: %v)", home, err))
		return nil
	}
	var docs []instructionDoc
	for _, file := range files {
		data, err := os.ReadFile(file.path)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				*notes = append(*notes, fmt.Sprintf("(инструкция не прочиталась: %s: %v)", file.path, err))
			}
			continue
		}
		rel, err := filepath.Rel(canonicalHome, file.path)
		if err != nil {
			*notes = append(*notes, fmt.Sprintf("(путь инструкции не вычислился: %s: %v)", file.path, err))
			continue
		}
		lines := splitInstructionLines(data)
		if file.fragment {
			var skip bool
			lines, skip = fragmentBody(lines, rel)
			if skip {
				continue
			}
		}
		docs = append(docs, instructionDoc{path: filepath.ToSlash(rel), fragment: file.fragment, lines: lines})
	}
	return docs
}

func splitInstructionLines(data []byte) []instructionLine {
	text := strings.TrimSuffix(string(data), "\n")
	parts := strings.Split(text, "\n")
	lines := make([]instructionLine, len(parts))
	for i, part := range parts {
		lines[i] = instructionLine{number: i + 1, text: part}
	}
	return lines
}

func fragmentBody(lines []instructionLine, path string) ([]instructionLine, bool) {
	if strings.Contains("/"+path+"/", "/.claude-decisions.md/") {
		return nil, true
	}
	if len(lines) == 0 || strings.TrimSpace(lines[0].text) != "---" {
		return lines, false
	}
	end := -1
	record := false
	for i := 1; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i].text)
		if line == "---" {
			end = i
			break
		}
		if strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(line, "запись:")), "true") {
			record = true
		}
	}
	if record {
		return nil, true
	}
	if end < 0 {
		return lines, false
	}
	return lines[end+1:], false
}

func instructionMatches(q query, docs []instructionDoc, builtLines map[string]bool) []instructionHit {
	var hits []instructionHit
	for _, doc := range docs {
		body := make([]string, len(doc.lines))
		for i, line := range doc.lines {
			body[i] = line.text
		}
		matches := matchingLines(q, []byte(strings.Join(body, "\n")), q.runs)
		for _, match := range matches {
			line := doc.lines[match.number-1]
			if doc.fragment && builtLines[normalizeInstruction(line.text)] {
				continue
			}
			rows := []instructionLine{line}
			if isWhyLine(line.text) {
				if previous := previousRuleLine(doc.lines, match.number-1); previous != nil {
					rows = append([]instructionLine{*previous}, rows...)
				}
			}
			words := distinct(q, line.text)
			if len(rows) > 1 {
				words = max(words, distinct(q, rows[0].text, rows[1].text))
			}
			hits = append(hits, instructionHit{path: doc.path, firstLine: rows[0].number, words: words, rows: rows})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].words != hits[j].words {
			return hits[i].words > hits[j].words
		}
		if hits[i].path != hits[j].path {
			return hits[i].path < hits[j].path
		}
		return hits[i].firstLine < hits[j].firstLine
	})
	return hits
}

func isWhyLine(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "Зачем:")
}

func previousRuleLine(lines []instructionLine, before int) *instructionLine {
	for i := before - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i].text) != "" {
			line := lines[i]
			return &line
		}
	}
	return nil
}

func normalizeInstruction(line string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.ReplaceAll(line, "`", "")), " "))
}

func slicesClone(values []string) []string {
	return append([]string(nil), values...)
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}
