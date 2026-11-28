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
	"unicode"
	"unicode/utf8"
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
	hits := instructionMatches(q, docs, builtInstructionLines(docs))
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
	addResolved := func(real string, root instructionRoot) {
		real = filepath.Clean(real)
		if existing, ok := byPath[real]; !ok || (existing.fragment && !root.fragment) {
			byPath[real] = instructionFile{path: real, fragment: root.fragment}
		}
	}
	add := func(path string, root instructionRoot) {
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				notes = append(notes, fmt.Sprintf("(путь инструкции не разрешился: %s: %v)", path, err))
			}
			return
		}
		addResolved(real, root)
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
		walkInstructionRoot(root, addResolved, add, func(note string) { notes = append(notes, note) })
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

// walkInstructionRoot adds the instruction files below a directory root and passes what did not read to note, in
// walk order. addResolved takes a path already free of links, add one that may still be or contain a link.
func walkInstructionRoot(root instructionRoot, addResolved, add func(string, instructionRoot), note func(string)) {
	// WalkDir does not descend into linked directories, so below the resolved root only a linked file itself
	// needs EvalSymlinks; resolving every path would lstat each of its components.
	realRoot, rootErr := filepath.EvalSymlinks(root.path)
	walkErr := filepath.WalkDir(root.path, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			note(fmt.Sprintf("(каталог инструкций не прочитался: %s: %v)", path, err))
			return nil
		}
		if entry.IsDir() || (!root.anyFile && filepath.Ext(path) != ".md") {
			return nil
		}
		if rel, relErr := filepath.Rel(root.path, path); rootErr == nil && relErr == nil && entry.Type()&fs.ModeSymlink == 0 {
			addResolved(filepath.Join(realRoot, rel), root)
			return nil
		}
		add(path, root)
		return nil
	})
	if walkErr != nil {
		note(fmt.Sprintf("(каталог инструкций не прочитался: %s: %v)", root.path, walkErr))
	}
}

func readInstructionDocs(files []instructionFile, home string, notes *[]string) []instructionDoc {
	canonicalHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		*notes = append(*notes, fmt.Sprintf("(путь HOME не разрешился: %s: %v)", home, err))
		return nil
	}
	var docs []instructionDoc
	for _, file := range files {
		data, err := readFileRaw(file.path)
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

// builtInstructionLines returns a lookup of normalized lines of non-fragment documents, built on first use:
// only a matched fragment line needs it.
func builtInstructionLines(docs []instructionDoc) func(string) bool {
	var built map[string]bool
	return func(normalized string) bool {
		if built == nil {
			built = make(map[string]bool)
			for _, doc := range docs {
				if !doc.fragment {
					for _, line := range doc.lines {
						built[normalizeInstruction(line.text)] = true
					}
				}
			}
		}
		return built[normalized]
	}
}

func instructionMatches(q query, docs []instructionDoc, builtLines func(string) bool) []instructionHit {
	var hits []instructionHit
	for _, doc := range docs {
		body := make([]string, len(doc.lines))
		for i, line := range doc.lines {
			body[i] = line.text
		}
		matches := matchingLines(q, []byte(strings.Join(body, "\n")), q.runs)
		for _, match := range matches {
			line := doc.lines[match.number-1]
			if doc.fragment && builtLines(normalizeInstruction(line.text)) {
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

// normalizeInstruction is strings.ToLower(strings.Join(strings.Fields(strings.ReplaceAll(line, "`", "")), " "))
// in one pass: fields split at Unicode spaces, runes lowered one by one, invalid bytes become U+FFFD.
func normalizeInstruction(line string) string {
	if strings.IndexByte(line, '`') >= 0 {
		line = strings.ReplaceAll(line, "`", "")
	}
	out := make([]byte, 0, len(line))
	space := false
	for i := 0; i < len(line); {
		c := line[i]
		if c < utf8.RuneSelf {
			i++
			if c == ' ' || c == '\t' || c == '\n' || c == '\v' || c == '\f' || c == '\r' {
				space = len(out) > 0
				continue
			}
			if space {
				out, space = append(out, ' '), false
			}
			if 'A' <= c && c <= 'Z' {
				c += 'a' - 'A'
			}
			out = append(out, c)
			continue
		}
		r, n := utf8.DecodeRuneInString(line[i:])
		i += n
		if unicode.IsSpace(r) {
			space = len(out) > 0
			continue
		}
		if space {
			out, space = append(out, ' '), false
		}
		out = utf8.AppendRune(out, unicode.ToLower(r))
	}
	return string(out)
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
