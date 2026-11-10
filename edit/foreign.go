package edit

import (
	"fmt"
	"strings"
)

// parseForeignSpec accepts the small set of unambiguous edit notations seen by
// callers. Inputs with any of their control lines in text are refused.
func parseForeignSpec(raw []byte, fixed string) ([]Edit, error) {
	lines := strings.Split(string(raw), "\n")
	if len(lines) > 1 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 1 && strings.TrimSpace(lines[0]) == "" {
		return nil, noForeignForm(raw, fixed)
	}
	trim := func(i int) string { return strings.TrimSpace(lines[i]) }
	var edits []Edit
	form := ""
	var err error
	switch {
	case strings.HasPrefix(trim(0), "FILE "):
		form = "FILE OLD/NEW"
		if len(lines) > 1 && trim(1) == "SEARCH" {
			form = "FILE SEARCH/REPLACE"
		}
		edits, err = parseFileOldNew(lines, fixed)
	case strings.HasPrefix(trim(0), "=== "):
		form = "=== path / old / --- / new"
		if len(lines) > 1 && trim(1) == "--- old" {
			form = "=== path / --- old / --- new"
		}
		edits, err = parseEqualsBlocks(lines, fixed)
	default:
		return nil, noForeignForm(raw, fixed)
	}
	if err != nil {
		return nil, ambiguousForeign(raw, fixed, err.Error())
	}
	for i := range edits {
		edits[i].Origin = "foreign:" + form
	}
	return edits, nil
}

func parseFileOldNew(lines []string, fixed string) ([]Edit, error) {
	path := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[0]), "FILE "))
	if path == "" {
		return nil, fmt.Errorf("missing FILE path")
	}
	if fixed != "" && !samePath(path, fixed) {
		return nil, fmt.Errorf("path %q differs from the FILE argument %q", path, fixed)
	}
	if len(lines) < 4 {
		return nil, fmt.Errorf("expected OLD/NEW blocks after FILE path")
	}
	oldSeparator, newSeparator := "OLD", "NEW"
	if strings.TrimSpace(lines[1]) == "SEARCH" {
		oldSeparator, newSeparator = "SEARCH", "REPLACE"
	}
	var edits []Edit
	for i := 1; i < len(lines); {
		if strings.TrimSpace(lines[i]) != oldSeparator {
			return nil, fmt.Errorf("expected %s block", oldSeparator)
		}
		oldStart := i + 1
		newMark := oldStart
		for newMark < len(lines) && strings.TrimSpace(lines[newMark]) != newSeparator {
			if isForeignSignal(strings.TrimSpace(lines[newMark])) {
				return nil, fmt.Errorf("control line %q occurs in OLD text", strings.TrimSpace(lines[newMark]))
			}
			newMark++
		}
		if newMark == len(lines) || newMark == oldStart {
			return nil, fmt.Errorf("missing %s or empty OLD text", newSeparator)
		}
		newStart := newMark + 1
		next := newStart
		for next < len(lines) && strings.TrimSpace(lines[next]) != oldSeparator {
			if strings.TrimSpace(lines[next]) == newSeparator || isForeignSignal(strings.TrimSpace(lines[next])) {
				return nil, fmt.Errorf("control line %q occurs in NEW text", strings.TrimSpace(lines[next]))
			}
			next++
		}
		old := strings.Join(lines[oldStart:newMark], "\n")
		if old == "" {
			return nil, fmt.Errorf("empty OLD text")
		}
		edits = append(edits, Edit{Files: []string{path}, Old: old, New: strings.Join(lines[newStart:next], "\n"), Want: 1})
		i = next
	}
	if len(edits) == 0 {
		return nil, fmt.Errorf("no OLD/NEW blocks")
	}
	return edits, nil
}

func parseEqualsBlocks(lines []string, fixed string) ([]Edit, error) {
	var edits []Edit
	for start := 0; start < len(lines); {
		header := strings.TrimSpace(lines[start])
		if !strings.HasPrefix(header, "=== ") {
			return nil, fmt.Errorf("expected === path")
		}
		path := strings.TrimSpace(strings.TrimPrefix(header, "==="))
		if path == "" {
			return nil, fmt.Errorf("missing === path")
		}
		if fixed != "" && !samePath(path, fixed) {
			return nil, fmt.Errorf("path %q differs from the FILE argument %q", path, fixed)
		}
		end := start + 1
		for end < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[end]), "=== ") {
			end++
		}
		edit, err := parseEqualsBlock(path, lines[start+1:end])
		if err != nil {
			return nil, err
		}
		edits = append(edits, edit)
		start = end
	}
	return edits, nil
}

func parseEqualsBlock(path string, body []string) (Edit, error) {
	plain, oldMark, newMark, err := findEqualsSeparators(body)
	if err != nil {
		return Edit{}, err
	}
	var old, replacement []string
	if oldMark >= 0 {
		if oldMark != 0 || newMark <= oldMark {
			return Edit{}, fmt.Errorf("expected --- old followed by --- new")
		}
		old, replacement = body[oldMark+1:newMark], body[newMark+1:]
	} else if plain >= 0 {
		old, replacement = body[:plain], body[plain+1:]
	} else {
		return Edit{}, fmt.Errorf("missing --- separator")
	}
	if err := checkForeignText(old, replacement); err != nil {
		return Edit{}, err
	}
	oldText := strings.Join(old, "\n")
	if oldText == "" {
		return Edit{}, fmt.Errorf("empty old text")
	}
	return Edit{Files: []string{path}, Old: oldText, New: strings.Join(replacement, "\n"), Want: 1}, nil
}

func findEqualsSeparators(body []string) (plain, oldMark, newMark int, err error) {
	plain, oldMark, newMark = -1, -1, -1
	for i, line := range body {
		switch strings.TrimSpace(line) {
		case "---":
			if plain >= 0 {
				return 0, 0, 0, fmt.Errorf("--- separator occurs more than once")
			}
			plain = i
		case "--- old":
			if oldMark >= 0 {
				return 0, 0, 0, fmt.Errorf("--- old occurs more than once")
			}
			oldMark = i
		case "--- new":
			if newMark >= 0 {
				return 0, 0, 0, fmt.Errorf("--- new occurs more than once")
			}
			newMark = i
		}
	}
	if plain >= 0 && (oldMark >= 0 || newMark >= 0) {
		return 0, 0, 0, fmt.Errorf("mixed --- separator styles")
	}
	return plain, oldMark, newMark, nil
}

func checkForeignText(parts ...[]string) error {
	for _, part := range parts {
		for _, line := range part {
			if isForeignSignal(strings.TrimSpace(line)) {
				return fmt.Errorf("control line %q occurs in block text", strings.TrimSpace(line))
			}
		}
	}
	return nil
}

func isForeignSignal(line string) bool {
	return line == "OLD" || line == "NEW" || line == "SEARCH" || line == "REPLACE" || line == "---" || line == "--- old" || line == "--- new" || line == "+++" || line == "===" || strings.HasPrefix(line, "=== ") || strings.HasPrefix(line, "FILE ")
}

func noForeignForm(raw []byte, fixed string) error {
	if !containsForeignSignal(raw) {
		return fmt.Errorf("spec has no SEARCH/REPLACE blocks; example in SEARCH/REPLACE form:\n%s", foreignExample(raw, fixed))
	}
	return ambiguousForeign(raw, fixed, "no recognized edit form")
}

func containsForeignSignal(raw []byte) bool {
	for line := range strings.SplitSeq(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if isForeignSignal(line) || strings.HasPrefix(line, "--- ") {
			return true
		}
	}
	return false
}

func ambiguousForeign(raw []byte, fixed, reason string) error {
	sample := foreignExample(raw, fixed)
	return fmt.Errorf("foreign or ambiguous spec (%s); example in SEARCH/REPLACE form:\n%s", reason, sample)
}

func foreignExample(raw []byte, fixed string) string {
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	if len(lines) > 1 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	path := samplePath(lines, fixed)
	body := lines
	if len(body) > 0 {
		body = body[1:]
	}
	old, replacement := sampleText(body)
	return canonical(path, old, replacement)
}

func samplePath(lines []string, fixed string) string {
	path := fixed
	if len(lines) == 0 {
		return pathOrPlaceholder(path)
	}
	first := strings.TrimSpace(lines[0])
	switch {
	case strings.HasPrefix(first, "FILE "):
		path = strings.TrimSpace(strings.TrimPrefix(first, "FILE "))
	case strings.HasPrefix(first, "=== "):
		path = strings.TrimSpace(strings.TrimPrefix(first, "==="))
	case strings.HasPrefix(first, "--- "):
		path = strings.TrimSpace(strings.TrimPrefix(first, "---"))
	case path == "":
		path = first
	}
	return pathOrPlaceholder(path)
}

func pathOrPlaceholder(path string) string {
	if path == "" {
		return "path/to/file"
	}
	return path
}

func sampleText(body []string) (string, string) {
	cut := -1
	plus := -1
	minus := -1
	for i, line := range body {
		mark := strings.TrimSpace(line)
		if mark == "---" && minus < 0 {
			minus = i
		}
		if mark == "+++" && plus < 0 {
			plus = i
		}
		if cut < 0 && (mark == "---" || mark == "+++" || mark == "===" || mark == "OLD" || mark == "NEW" || mark == "SEARCH" || mark == "REPLACE") {
			cut = i
		}
	}
	var old, replacement []string
	if minus >= 0 && plus > minus {
		old, replacement = body[minus+1:plus], body[plus+1:]
	} else if cut >= 0 {
		old, replacement = body[:cut], body[cut+1:]
	} else {
		old = body
	}
	if strings.Join(old, "\n") == "" {
		old = []string{"old text"}
	}
	return strings.Join(old, "\n"), strings.Join(replacement, "\n")
}

func canonical(path, old, replacement string) string {
	return path + "\n" + markSearch + "\n" + old + "\n" + markDivider + "\n" + replacement + "\n" + markReplace
}

// ForeignNotice returns the canonical translation for an accepted foreign form.
func ForeignNotice(edits []Edit) (string, bool) {
	if len(edits) == 0 || !strings.HasPrefix(edits[0].Origin, "foreign:") {
		return "", false
	}
	form := strings.TrimPrefix(edits[0].Origin, "foreign:")
	var lines []string
	for _, e := range edits {
		lines = append(lines, canonical(e.Files[0], e.Old, e.New))
	}
	translated := strings.Join(lines, "\n")
	rows := strings.Split(translated, "\n")
	if len(rows) > 20 {
		translated = strings.Join(rows[:20], "\n") + fmt.Sprintf("\n… (+%d lines)", len(rows)-20)
	}
	return fmt.Sprintf("sub: read %s form; translated to SEARCH/REPLACE:\n%s", form, translated), true
}
