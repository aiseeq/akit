package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const blockedPrefix = "Заблокирован: "
const blocksPrefix = "Блокирует: "

func headerLinks(body, prefix string) []string {
	var out []string
	for line := range strings.SplitSeq(body, "\n") {
		if v, ok := strings.CutPrefix(line, prefix); ok && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

func addHeaderLink(body, prefix, value string) string {
	if slices.Contains(headerLinks(body, prefix), value) {
		return body
	}
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "Статус: ") {
			lines = append(lines[:i+1], append([]string{prefix + value}, lines[i+1:]...)...)
			return strings.Join(lines, "\n")
		}
	}
	return body
}

func removeHeaderLink(body, prefix, value string) string {
	lines := strings.Split(body, "\n")
	kept := lines[:0]
	for _, line := range lines {
		if line != prefix+value {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

type blockTarget struct{ root, dir, id, ref string }

func currentProject(o options) string {
	if o.mainRoot != "" {
		return o.mainRoot
	}
	if o.baseRoot != "" {
		return o.baseRoot
	}
	return ""
}

func projectRef(from, to string) string {
	fa, _ := filepath.Abs(from)
	ta, _ := filepath.Abs(to)
	if fa == ta {
		return ""
	}
	home, _ := os.UserHomeDir()
	for _, root := range []string{"git", "work"} {
		base := filepath.Join(home, root)
		if rel, err := filepath.Rel(base, ta); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return root + "/" + filepath.ToSlash(rel)
		}
	}
	return ""
}

func resolveBlockTarget(o options, raw string) (blockTarget, error) {
	parts := strings.Split(raw, "/")
	project, id := currentProject(o), raw
	if len(parts) == 3 && (parts[0] == "git" || parts[0] == "work") {
		name := parts[1]
		home, err := os.UserHomeDir()
		if err != nil {
			return blockTarget{}, err
		}
		project = filepath.Join(home, parts[0], name)
		id = parts[2]
	} else if len(parts) == 2 {
		name := parts[0]
		id = parts[1]
		home, err := os.UserHomeDir()
		if err != nil {
			return blockTarget{}, err
		}
		gp, wp := filepath.Join(home, "git", name), filepath.Join(home, "work", name)
		gi, ge := os.Stat(gp)
		wi, we := os.Stat(wp)
		if ge != nil && !os.IsNotExist(ge) {
			return blockTarget{}, ge
		}
		if we != nil && !os.IsNotExist(we) {
			return blockTarget{}, we
		}
		gok, wok := ge == nil && gi.IsDir(), we == nil && wi.IsDir()
		if gok && wok {
			return blockTarget{}, fmt.Errorf("project %q exists in both %s and %s; use git/ or work/", name, gp, wp)
		}
		if !gok && !wok {
			return blockTarget{}, fmt.Errorf("project %q not found", name)
		}
		if gok {
			project = gp
		} else {
			project = wp
		}
	} else if len(parts) != 1 {
		return blockTarget{}, fmt.Errorf("invalid ticket reference %q", raw)
	}
	if !slugRE.MatchString(id) {
		return blockTarget{}, fmt.Errorf("invalid ticket slug %q", id)
	}
	if project == "" {
		return blockTarget{}, fmt.Errorf("cannot resolve current project")
	}
	dir := filepath.Join(project, "docs/tasks")
	t, err := find(dir, id)
	if err != nil {
		return blockTarget{}, err
	}
	ref := t.slug
	if pref := projectRef(currentProject(o), project); pref != "" {
		ref = pref + "/" + t.slug
	}
	return blockTarget{project, dir, t.slug, ref}, nil
}

func linkedPath(project, ref string) (string, string, error) {
	parts := strings.Split(ref, "/")
	dir := filepath.Join(project, "docs/tasks")
	id := ref
	if len(parts) == 2 && (parts[0] == "git" || parts[0] == "work") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "", err
		}
		dir = filepath.Join(home, parts[0], parts[1], "docs/tasks")
		id = parts[1] /* overridden below for 3-part canonical references */
	}
	if len(parts) == 3 && (parts[0] == "git" || parts[0] == "work") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "", err
		}
		dir = filepath.Join(home, parts[0], parts[1], "docs/tasks")
		id = parts[2]
	}
	if len(parts) == 2 && parts[0] != "git" && parts[0] != "work" {
		return dir, "", fmt.Errorf("unresolved project reference %s", ref)
	}
	t, err := find(dir, id)
	if err != nil {
		return dir, "", err
	}
	return dir, t.path, nil
}

func parseTask(path, body string) (task, error) {
	name, ok := parseTaskName(filepath.Base(path))
	if !ok {
		return task{}, fmt.Errorf("invalid task name %s", path)
	}
	st, _ := status(body)
	return task{path: path, slug: name.slug, status: st, prio: name.prio, diff: name.diff, title: title(body), body: body}, nil
}

func attachBlocks(t task, o options, out, errout io.Writer, targets []string) int {
	var roots []string
	for _, raw := range targets {
		target, err := resolveBlockTarget(o, raw)
		if err != nil {
			return fail(errout, err)
		}
		if target.dir == filepath.Dir(t.path) && target.id == t.slug {
			return fail(errout, fmt.Errorf("ticket cannot block itself"))
		}
		targetTask, err := find(target.dir, target.id)
		if err != nil {
			return fail(errout, err)
		} else if targetTask.status == "готово" || targetTask.status == "отклонён" {
			return fail(errout, fmt.Errorf("target %s is closed", raw))
		}
		roots = append(roots, targetTask.path)
		ref := target.ref
		back := projectRef(target.root, currentProject(o))
		if back != "" {
			back += "/"
		}
		back += t.slug
		if slices.Contains(headerLinks(targetTask.body, blockedPrefix), back) && slices.Contains(headerLinks(t.body, blocksPrefix), ref) {
			continue
		}
		// The reverse chain from X must not reach T.
		chain := []string{t.slug}
		seen := map[string]bool{}
		cur := blockTarget{root: currentProject(o), dir: filepath.Dir(t.path), id: t.slug}
		for {
			if cur.dir == target.dir && cur.id == target.id {
				return fail(errout, fmt.Errorf("blocking cycle: %s", strings.Join(append(chain, target.id), " → ")))
			}
			key := cur.dir + "/" + cur.id
			if seen[key] {
				break
			}
			seen[key] = true
			tt, e := find(cur.dir, cur.id)
			if e != nil {
				break
			}
			links := headerLinks(tt.body, blockedPrefix)
			if len(links) == 0 {
				break
			}
			d, p, e := linkedPath(cur.root, links[0])
			if e != nil {
				break
			}
			name, _ := parseTaskName(filepath.Base(p))
			chain = append(chain, name.slug)
			cur = blockTarget{root: filepath.Dir(filepath.Dir(filepath.Dir(p))), dir: d, id: name.slug}
		}
		if !slices.Contains(headerLinks(targetTask.body, blockedPrefix), back) {
			if err := updateTicket(targetTask.path, func(body string) string {
				return journal(addHeaderLink(body, blockedPrefix, back), fmt.Sprintf("- %s note — %s: блокирует %s", timeStamp(), o.by, back))
			}); err != nil {
				return fail(errout, err)
			}
		}
		if !slices.Contains(headerLinks(t.body, blocksPrefix), ref) {
			if err := updateTicket(t.path, func(body string) string {
				return journal(addHeaderLink(body, blocksPrefix, ref), fmt.Sprintf("- %s note — %s: заблокирован %s", timeStamp(), o.by, ref))
			}); err != nil {
				return fail(errout, err)
			}
		}
	}
	code := 0
	for _, path := range roots {
		if c := propagatePriority(path, o, out, errout); c != 0 {
			code = c
		}
	}
	if len(targets) > 0 {
		return code
	}
	return 0
}

func timeStamp() string { return time.Now().Format("2006-01-02 15:04") }

func updateTicket(path string, edit func(string) string) error {
	dir := filepath.Dir(path)
	release, err := lockTasks(dir)
	if err != nil {
		return err
	}
	defer release()
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	next := edit(string(b))
	if next == string(b) {
		return nil
	}
	return writeTask(path, next)
}

func blockPrioRank(p string) int {
	switch normalizePrio(p) {
	case "CRIT":
		return 0
	case "HIGH":
		return 1
	case "MED":
		return 2
	case "LOW":
		return 3
	}
	return 4
}

func propagatePriority(start string, o options, out, errout io.Writer) int {
	queue := []string{start}
	seen := map[string]bool{}
	code := 0
	for len(queue) > 0 {
		path := queue[0]
		queue = queue[1:]
		abs, _ := filepath.Abs(path)
		if seen[abs] {
			continue
		}
		seen[abs] = true
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		source, err := parseTask(path, string(b))
		if err != nil {
			continue
		}
		for _, ref := range headerLinks(source.body, blockedPrefix) {
			dir, neighborPath, err := linkedPath(filepath.Dir(filepath.Dir(filepath.Dir(path))), ref)
			if err != nil {
				rows, _ := readTransferLedger(dir)
				stem := strings.TrimSuffix(filepath.Base(ref), ".md")
				transferred := false
				for _, row := range rows {
					if row.Slug == stem || row.Stem == stem {
						note := fmt.Sprintf("блокирующий %s вне tk: подними в трекере", row.Key)
						_ = updateTicket(path, func(s string) string { return journal(s, "- "+timeStamp()+" note — "+o.by+": "+note) })
						fmt.Fprintln(out, note)
						transferred = true
						break
					}
				}
				if transferred {
					continue
				}
				fmt.Fprintf(errout, "tk: missing blocking link %s from %s\n", ref, source.slug)
				code = exitUsage
				continue
			}
			neighborBytes, err := os.ReadFile(neighborPath)
			if err != nil {
				fmt.Fprintf(errout, "tk: missing blocking link %s from %s\n", ref, source.slug)
				code = exitUsage
				continue
			}
			neighbor, err := parseTask(neighborPath, string(neighborBytes))
			if err != nil {
				continue
			}
			if neighbor.status == "готово" || neighbor.status == "отклонён" {
				continue
			}
			queue = append(queue, neighborPath)
			if blockPrioRank(neighbor.prio) <= blockPrioRank(source.prio) {
				queue = append(queue, neighborPath)
				continue
			}
			name, err := retaggedName(filepath.Base(neighborPath), options{prio: normalizePrio(source.prio)})
			if err != nil {
				fmt.Fprintf(errout, "tk: raise %s: %v\n", ref, err)
				code = exitUsage
				continue
			}
			newPath := filepath.Join(filepath.Dir(neighborPath), name)
			why := ""
			if blockPrioRank(source.prio) <= 1 && lineValue(neighbor.body, "Причина: ") == "" {
				ref := projectRef(filepath.Dir(filepath.Dir(filepath.Dir(neighborPath))), filepath.Dir(filepath.Dir(filepath.Dir(path))))
				if ref != "" {
					ref += "/"
				}
				why = "блокирует " + ref + source.slug
			}
			updated := neighbor.body
			if why != "" {
				updated, _ = setHeaderValue(updated, "Причина: ", why)
			}
			updated = journal(updated, fmt.Sprintf("- %s note — %s: приоритет %s → %s: блокирует %s", timeStamp(), o.by, normalizePrio(neighbor.prio), normalizePrio(source.prio), source.slug))
			if newPath != neighborPath {
				updated = journal(updated, renameJournal(o.by, neighborPath, newPath))
			}
			release, e := lockTasks(filepath.Dir(neighborPath))
			if e == nil {
				if newPath != neighborPath {
					e = renameTaskFile(neighborPath, newPath)
				}
				if e == nil {
					e = writeRenamedTask(neighborPath, newPath, updated)
				}
				release()
			}
			if e != nil {
				fmt.Fprintf(errout, "tk: raise %s: %v\n", ref, e)
				code = exitUsage
				continue
			}
			fmt.Fprintf(out, "поднят %s %s → %s: блокирует %s\n", ref, normalizePrio(neighbor.prio), normalizePrio(source.prio), source.slug)
			queue = append(queue, newPath)
		}
	}
	return code
}

func removeBlocks(t task, o options, out, errout io.Writer, refs []string) int {
	for _, raw := range refs {
		target, err := resolveBlockTarget(o, raw)
		if err != nil {
			return fail(errout, err)
		}
		forward := target.ref
		if !slices.Contains(headerLinks(t.body, blocksPrefix), forward) {
			continue
		}
		if err = updateTicket(t.path, func(s string) string { return removeHeaderLink(s, blocksPrefix, forward) }); err != nil {
			return fail(errout, err)
		}
		back := projectRef(target.root, currentProject(o))
		if back != "" {
			back += "/"
		}
		back += t.slug
		if err = updateTicket(func() string { tt, _ := find(target.dir, target.id); return tt.path }(), func(s string) string { return removeHeaderLink(s, blockedPrefix, back) }); err != nil {
			return fail(errout, err)
		}
	}
	return 0
}

func blockMark(t task) string {
	project := filepath.Dir(filepath.Dir(filepath.Dir(t.path)))
	for _, ref := range headerLinks(t.body, blockedPrefix) {
		dir, path, err := linkedPath(project, ref)
		if err == nil {
			if b, e := os.ReadFile(path); e == nil {
				tt, _ := parseTask(path, string(b))
				if tt.status != "готово" && tt.status != "отклонён" {
					return " заблокирован: " + ref
				}
			}
			continue
		}
		rows, _ := readTransferLedger(dir)
		stem := filepath.Base(ref)
		for _, row := range rows {
			if row.Slug == stem || row.Stem == stem {
				return " заблокирован: " + row.Key + " " + row.URL
			}
		}
	}
	return ""
}

func closeBlockRelations(t task, o options, out, errout io.Writer, code int) int {
	project := filepath.Dir(filepath.Dir(filepath.Dir(t.path)))
	for _, ref := range headerLinks(t.body, blocksPrefix) {
		dir, path, err := linkedPath(project, ref)
		if err != nil {
			fmt.Fprintf(errout, "tk: warning: cannot unblock %s: %v\n", ref, err)
			continue
		}
		name, _ := parseTaskName(filepath.Base(t.path))
		back := projectRef(dirProject(dir), project)
		if back != "" {
			back += "/"
		}
		back += name.slug
		if err = updateTicket(path, func(s string) string {
			return journal(removeHeaderLink(s, blockedPrefix, back), "- "+timeStamp()+" note — "+o.by+": блокирующий "+back+" закрыт")
		}); err != nil {
			fmt.Fprintf(errout, "tk: warning: cannot update %s: %v\n", ref, err)
			continue
		}
		fmt.Fprintln(out, "разблокирован "+ref)
	}
	for _, ref := range headerLinks(t.body, blockedPrefix) {
		dir, path, err := linkedPath(project, ref)
		if err != nil {
			continue
		}
		name, _ := parseTaskName(filepath.Base(t.path))
		back := projectRef(dirProject(dir), project)
		if back != "" {
			back += "/"
		}
		back += name.slug
		if err = updateTicket(path, func(s string) string {
			return journal(removeHeaderLink(s, blocksPrefix, back), "- "+timeStamp()+" note — "+o.by+": заблокированный "+back+" закрыт, приоритет не менялся: понизь, если не нужен")
		}); err != nil {
			fmt.Fprintf(errout, "tk: warning: cannot update %s: %v\n", ref, err)
		}
	}
	return code
}

func dirProject(dir string) string { return filepath.Dir(filepath.Dir(dir)) }

func reverseTaskRef(t task, ref string) (string, string, string, error) {
	project := filepath.Dir(filepath.Dir(filepath.Dir(t.path)))
	dir, path, err := linkedPath(project, ref)
	if err != nil {
		return "", "", "", err
	}
	name, ok := parseTaskName(filepath.Base(t.path))
	if !ok {
		return "", "", "", fmt.Errorf("invalid task path %s", t.path)
	}
	back := projectRef(dirProject(dir), project)
	if back != "" {
		back += "/"
	}
	return dir, path, back + name.slug, nil
}

func notePriorityLowered(t task, o options) {
	if o.prio == "" || blockPrioRank(o.prio) <= blockPrioRank(t.prio) {
		return
	}
	for _, ref := range headerLinks(t.body, blockedPrefix) {
		_, path, back, err := reverseTaskRef(t, ref)
		if err != nil {
			continue
		}
		_ = updateTicket(path, func(s string) string {
			return journal(s, "- "+timeStamp()+" note — "+o.by+": заблокированный "+back+" закрыт, приоритет не менялся: понизь, если не нужен")
		})
	}
}
