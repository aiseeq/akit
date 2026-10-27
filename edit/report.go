package edit

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Summary — короткий итог удачной правки: сколько замен и где, с номерами строк в новом тексте.
func (p *Plan) Summary() string {
	perFile := map[string][]int{}
	var order []string
	total := 0
	for i := range p.Results {
		r := &p.Results[i]
		total += r.Found
		for _, h := range r.Hits {
			if _, seen := perFile[h.File]; !seen {
				order = append(order, h.File)
			}
			perFile[h.File] = append(perFile[h.File], h.Lines...)
		}
	}
	parts := make([]string, 0, len(order))
	for _, name := range order {
		slices.Sort(perFile[name])
		parts = append(parts, fmt.Sprintf("%s %d (%s)", name, len(perFile[name]), lineList("line", perFile[name])))
	}
	return fmt.Sprintf("%s in %s: %s", plural(total, "replacement"), plural(len(order), "file"), strings.Join(parts, ", "))
}

// Mismatches — по строке на правку, у которой число совпадений не сошлось.
func (p *Plan) Mismatches() []string {
	var out []string
	for i := range p.Results {
		r := &p.Results[i]
		if r.OK() {
			continue
		}
		msg := fmt.Sprintf("%s: expected %d, found %d", r.Edit.Origin, r.Edit.Want, r.Found)
		switch {
		case r.Found > 0:
			msg += " at " + hitList(r.Hits)
			if len(r.Hints) > 0 {
				msg += "; near-misses ignoring indentation/whitespace at " + hitList(r.Hints)
			}
		case len(r.Hints) > 0:
			msg += " in " + strings.Join(r.Edit.Files, ", ") + "; matches ignoring indentation/whitespace at " + hitList(r.Hints)
		default:
			msg += " in " + strings.Join(r.Edit.Files, ", ") + "; first line " + strconv.Quote(firstLine(r.Edit.Old)) + " " + firstLineWhere(p, r.Edit)
		}
		out = append(out, msg)
	}
	return out
}

// firstLineWhere подсказывает, встречается ли хотя бы первая строка искомого: помогает понять, где текст разошёлся.
func firstLineWhere(p *Plan, e Edit) string {
	first := strings.TrimSpace(firstLine(e.Old))
	var hits []Hit
	for _, name := range e.Files {
		var lines []int
		for i, l := range strings.Split(p.files[name].cur, "\n") {
			if first != "" && strings.Contains(l, first) {
				lines = append(lines, i+1)
			}
		}
		if len(lines) > 0 {
			hits = append(hits, Hit{File: name, Lines: lines})
		}
	}
	if len(hits) == 0 {
		return "not found either"
	}
	return "found at " + hitList(hits)
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

func hitList(hits []Hit) string {
	parts := make([]string, 0, len(hits))
	for _, h := range hits {
		parts = append(parts, h.File+":"+lineList("", h.Lines))
	}
	return strings.Join(parts, " ")
}

// lineList — «line 3» или «lines 3,10,…»; без слова — только номера.
func lineList(word string, lines []int) string {
	nums := make([]string, 0, maxLines+1)
	for i, n := range lines {
		if i == maxLines {
			nums = append(nums, fmt.Sprintf("…+%d", len(lines)-maxLines))
			break
		}
		nums = append(nums, strconv.Itoa(n))
	}
	list := strings.Join(nums, ",")
	switch {
	case word == "":
		return list
	case len(lines) == 1:
		return word + " " + list
	default:
		return word + "s " + list
	}
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}
