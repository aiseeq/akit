package main

import (
	"errors"
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

func addHeaderLink(body, prefix, value string) (string, error) {
	if slices.Contains(headerLinks(body, prefix), value) {
		return body, nil
	}
	lines := strings.Split(body, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.HasPrefix(lines[i], prefix) {
			lines = append(lines[:i+1], append([]string{prefix + value}, lines[i+1:]...)...)
			return strings.Join(lines, "\n"), nil
		}
	}
	return setHeaderValue(body, prefix, value)
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

type blockTarget struct{ root, dir, id, ref, back, status, body string }

func currentProject(o options) string {
	if o.baseRoot != "" {
		return o.baseRoot
	}
	if o.mainRoot != "" {
		return o.mainRoot
	}
	return ""
}

func ticketRef(from, to, slug string) (string, error) {
	fromRoot, err := normalizedRoot(from)
	if err != nil {
		return "", fmt.Errorf("resolve source project: %w", err)
	}
	toRoot, err := normalizedRoot(to)
	if err != nil {
		return "", fmt.Errorf("resolve target project: %w", err)
	}
	if fromRoot == toRoot {
		return slug, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("determine project roots: %w", err)
	}
	for _, root := range []string{"git", "work"} {
		base, err := normalizedRoot(filepath.Join(home, root))
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return "", fmt.Errorf("resolve %s project root: %w", root, err)
			}
			continue
		}
		rel, err := filepath.Rel(base, toRoot)
		if err != nil {
			return "", fmt.Errorf("relate project paths: %w", err)
		}
		if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.Dir(rel) != "." {
			continue
		}
		return root + "/" + filepath.ToSlash(rel) + "/" + slug, nil
	}
	return "", fmt.Errorf("target project %s must be a direct child of $HOME/git or $HOME/work", toRoot)
}

func resolveBlockTarget(o options, raw string) (blockTarget, error) {
	project, dir, id, ref, err := resolveBlockLocation(o, raw)
	if err != nil {
		return blockTarget{}, err
	}
	t, err := readTaskLocked(dir, id)
	if err != nil {
		return blockTarget{}, err
	}
	ref, err = ticketRef(currentProject(o), project, t.slug)
	if err != nil {
		return blockTarget{}, err
	}
	return blockTarget{root: project, dir: dir, id: t.slug, ref: ref, status: t.status, body: t.body}, nil
}

func resolveBlockLocation(o options, raw string) (string, string, string, string, error) {
	parts := strings.Split(raw, "/")
	project, id := currentProject(o), raw
	if len(parts) == 3 && (parts[0] == "git" || parts[0] == "work") {
		name := parts[1]
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "", "", "", fmt.Errorf("determine linked project root: %w", err)
		}
		project = filepath.Join(home, parts[0], name)
		id = parts[2]
	} else if len(parts) == 2 {
		projectPath, err := destinationProject(parts[0])
		if err != nil {
			return "", "", "", "", err
		}
		id = parts[1]
		project = projectPath
	} else if len(parts) != 1 {
		return "", "", "", "", fmt.Errorf("invalid ticket reference %q", raw)
	}
	if !slugRE.MatchString(id) {
		return "", "", "", "", fmt.Errorf("invalid ticket slug %q", id)
	}
	if project == "" {
		return "", "", "", "", fmt.Errorf("cannot resolve current project")
	}
	dir := filepath.Join(project, "docs/tasks")
	ref, err := ticketRef(currentProject(o), project, id)
	if err != nil {
		return "", "", "", "", err
	}
	return project, dir, id, ref, nil
}

func linkedPath(project, ref string) (string, string, error) {
	dir, id, err := referenceLocation(project, ref)
	if err != nil {
		return dir, "", err
	}
	t, err := find(dir, id)
	if err == nil {
		return dir, t.path, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return dir, "", err
	}
	archive, archiveErr := archiveDir(dirProject(dir))
	if archiveErr != nil {
		return dir, "", archiveErr
	}
	archived, archiveErr := find(archive, id)
	if archiveErr == nil {
		return archive, archived.path, nil
	}
	if !errors.Is(archiveErr, os.ErrNotExist) {
		return archive, "", archiveErr
	}
	if err != nil {
		return dir, "", err
	}
	return dir, "", os.ErrNotExist
}

func referenceLocation(project, ref string) (string, string, error) {
	parts := strings.Split(ref, "/")
	id := ref
	if len(parts) == 3 && (parts[0] == "git" || parts[0] == "work") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "", fmt.Errorf("determine linked project root: %w", err)
		}
		project = filepath.Join(home, parts[0], parts[1])
		id = parts[2]
	} else if len(parts) != 1 {
		return "", "", fmt.Errorf("unresolved project reference %s", ref)
	}
	if !slugRE.MatchString(id) {
		return "", "", fmt.Errorf("invalid ticket reference %s", ref)
	}
	return filepath.Join(project, "docs/tasks"), id, nil
}

func findWithArchive(dir, id string) (task, error) {
	t, err := find(dir, id)
	if err == nil {
		return t, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return task{}, err
	}
	archive, err := archiveDir(dirProject(dir))
	if err != nil {
		return task{}, err
	}
	return find(archive, id)
}

func isArchivedTask(path string) bool {
	archive, err := archiveDir(taskProject(path))
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(archive, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func findReferenceLocked(project, ref string) (string, task, error) {
	dir, id, err := referenceLocation(project, ref)
	if err != nil {
		return dir, task{}, err
	}
	release, err := lockTasks(dir)
	if err != nil {
		return dir, task{}, err
	}
	t, findErr := findWithArchive(dir, id)
	release()
	return dir, t, findErr
}

func parseTask(path, body string) (task, error) {
	name, ok := parseTaskName(filepath.Base(path))
	if !ok {
		return task{}, fmt.Errorf("invalid task name %s", path)
	}
	st, err := status(body)
	if err != nil {
		return task{}, fmt.Errorf("%s: %w", path, err)
	}
	return task{path: path, slug: name.slug, status: st, prio: name.prio, diff: name.diff, title: title(body), body: body}, nil
}

func attachBlocks(t task, o options, out, errout io.Writer, targets []string) int {
	resolved, code := validateBlockAttachments(t, o, targets, errout)
	if code != 0 {
		return code
	}
	var raiseFrom []string
	for _, target := range resolved {
		fresh, err := readTaskLocked(filepath.Dir(t.path), t.slug)
		if err != nil {
			return fail(errout, err)
		}
		targetTask, err := mutateTask(target.dir, target.id, func(current task) (string, error) {
			if current.status == "готово" || current.status == "отклонён" {
				return "", fmt.Errorf("target %s is closed", target.id)
			}
			body := current.body
			if !slices.Contains(headerLinks(body, blockedPrefix), target.back) {
				body, err = addHeaderLink(body, blockedPrefix, target.back)
				if err != nil {
					return "", err
				}
				body = journal(body, fmt.Sprintf("- %s note — %s: блокирует %s", timeStamp(), o.by, target.back))
			}
			return body, nil
		})
		if err != nil {
			return fail(errout, err)
		}
		_, err = mutateTask(filepath.Dir(t.path), fresh.slug, func(current task) (string, error) {
			body := current.body
			if !slices.Contains(headerLinks(body, blocksPrefix), target.ref) {
				body, err = addHeaderLink(body, blocksPrefix, target.ref)
				if err != nil {
					return "", err
				}
				body = journal(body, fmt.Sprintf("- %s note — %s: заблокирован %s", timeStamp(), o.by, target.ref))
			}
			return body, nil
		})
		if err != nil {
			fmt.Fprintf(errout, "tk: warning: target %s was linked, but source %s could not be updated: %v\n", target.ref, t.slug, err)
			return fail(errout, err)
		}
		raiseFrom = append(raiseFrom, targetTask.path)
	}
	resultCode := 0
	for _, path := range raiseFrom {
		if c := propagatePriority(path, o, out, errout); c != 0 {
			resultCode = c
		}
	}
	return resultCode
}

func validateBlockAttachments(t task, o options, targets []string, errout io.Writer) ([]blockTarget, int) {
	resolved := make([]blockTarget, 0, len(targets))
	sourceProject := taskProject(t.path)
	for _, raw := range targets {
		target, err := resolveBlockTarget(o, raw)
		if err != nil {
			return nil, fail(errout, err)
		}
		if target.status == "готово" || target.status == "отклонён" {
			return nil, fail(errout, fmt.Errorf("target %s is closed", raw))
		}
		if sameTicket(taskProject(t.path), t.slug, target.root, target.id) {
			return nil, fail(errout, errors.New("ticket cannot block itself"))
		}
		target.ref, err = ticketRef(sourceProject, target.root, target.id)
		if err != nil {
			return nil, fail(errout, err)
		}
		target.back, err = ticketRef(target.root, sourceProject, t.slug)
		if err != nil {
			return nil, fail(errout, err)
		}
		if _, err = addHeaderLink(target.body, blockedPrefix, target.back); err != nil {
			return nil, fail(errout, fmt.Errorf("target %s: %w", raw, err))
		}
		if _, err = addHeaderLink(t.body, blocksPrefix, target.ref); err != nil {
			return nil, fail(errout, fmt.Errorf("source %s: %w", t.slug, err))
		}
		fresh, err := readTaskLocked(filepath.Dir(t.path), t.slug)
		if err != nil {
			return nil, fail(errout, err)
		}
		chain, cycle := blockingChainTo(fresh, target, errout)
		if cycle {
			return nil, fail(errout, fmt.Errorf("blocking cycle: %s", strings.Join(chain, " → ")))
		}
		resolved = append(resolved, target)
	}
	return resolved, 0
}

func sameTicket(project, slug, otherProject, otherSlug string) bool {
	a, errA := normalizedRoot(project)
	b, errB := normalizedRoot(otherProject)
	return errA == nil && errB == nil && a == b && slug == otherSlug
}

func readTaskLocked(dir, id string) (task, error) {
	release, err := lockTasks(dir)
	if err != nil {
		return task{}, err
	}
	defer release()
	return findWithArchive(dir, id)
}

func mutateTask(dir, id string, edit func(task) (string, error)) (task, error) {
	release, err := lockTasks(dir)
	if err != nil {
		return task{}, err
	}
	defer release()
	t, err := findWithArchive(dir, id)
	if err != nil {
		return task{}, err
	}
	body, err := edit(t)
	if err != nil {
		return task{}, err
	}
	if body != t.body {
		if err := writeTask(t.path, body); err != nil {
			return task{}, err
		}
		t.body = body
	}
	return t, nil
}

func blockingChainTo(start task, target blockTarget, errout io.Writer) ([]string, bool) {
	type node struct {
		ticket task
		chain  []string
	}
	queue := []node{{start, []string{start.slug}}}
	visited := map[string]bool{}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		project := taskProject(n.ticket.path)
		for _, ref := range headerLinks(n.ticket.body, blockedPrefix) {
			dir, neighbor, err := findReferenceLocked(project, ref)
			if err != nil {
				fmt.Fprintf(errout, "tk: warning: cannot inspect cycle link %s from %s: %v\n", ref, n.ticket.slug, err)
				continue
			}
			root := taskProject(neighbor.path)
			if sameTicket(root, neighbor.slug, target.root, target.id) {
				return append(append([]string(nil), n.chain...), neighbor.slug), true
			}
			key, err := normalizedRoot(filepath.Join(dir, filepath.Base(neighbor.path)))
			if err != nil {
				fmt.Fprintf(errout, "tk: warning: cannot normalize cycle link %s from %s: %v\n", ref, n.ticket.slug, err)
				continue
			}
			if visited[key] {
				continue
			}
			visited[key] = true
			queue = append(queue, node{neighbor, append(append([]string(nil), n.chain...), neighbor.slug)})
		}
	}
	return nil, false
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
		return fmt.Errorf("read ticket %s: %w", path, err)
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
		key, err := normalizedRoot(path)
		if err != nil {
			fmt.Fprintf(errout, "tk: warning: cannot inspect blocking chain at %s: %v\n", path, err)
			code = exitUsage
			continue
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		b, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(errout, "tk: warning: cannot read blocking chain at %s: %v\n", path, err)
			code = exitUsage
			continue
		}
		source, err := parseTask(path, string(b))
		if err != nil {
			fmt.Fprintf(errout, "tk: warning: cannot read blocking chain at %s: %v\n", path, err)
			code = exitUsage
			continue
		}
		for _, ref := range headerLinks(source.body, blockedPrefix) {
			next, err := propagateLink(path, source, ref, o, out, errout)
			if err != nil {
				fmt.Fprintf(errout, "tk: warning: cannot propagate blocking link %s from %s: %v\n", ref, source.slug, err)
				code = exitUsage
				continue
			}
			if next != "" {
				queue = append(queue, next)
			}
		}
	}
	return code
}

func propagateLink(sourcePath string, source task, ref string, o options, out, errout io.Writer) (string, error) {
	dir, id, err := referenceLocation(taskProject(sourcePath), ref)
	if err != nil {
		fmt.Fprintf(errout, "tk: warning: blocking link %s from %s: %v\n", ref, source.slug, err)
		return "", err
	}
	release, err := lockTasks(dir)
	if err != nil {
		fmt.Fprintf(errout, "tk: warning: blocking link %s from %s: %v\n", ref, source.slug, err)
		return "", err
	}
	neighbor, err := findWithArchive(dir, id)
	if err != nil {
		rows, ledgerErr := readTransferLedger(dir)
		release()
		if ledgerErr != nil {
			fmt.Fprintf(errout, "tk: warning: blocking link %s from %s: transfer ledger: %v\n", ref, source.slug, ledgerErr)
			return "", ledgerErr
		}
		if row, ok := transferredRecord(rows, id); ok {
			note := fmt.Sprintf("блокирующий %s вне tk: подними в трекере", row.Key)
			if e := appendTaskNote(sourcePath, o, note); e != nil {
				fmt.Fprintf(errout, "tk: warning: blocking link %s from %s: %v\n", ref, source.slug, e)
				return "", e
			}
			fmt.Fprintln(out, note)
			return "", nil
		}
		fmt.Fprintf(errout, "tk: warning: missing blocking link %s from %s\n", ref, source.slug)
		return "", os.ErrNotExist
	}
	if isArchivedTask(neighbor.path) || neighbor.status == "готово" || neighbor.status == "отклонён" {
		release()
		return "", nil
	}
	if blockPrioRank(neighbor.prio) <= blockPrioRank(source.prio) {
		release()
		return neighbor.path, nil
	}
	newPath, err := raiseNeighborLocked(neighbor, source, sourcePath, o, ref, errout)
	release()
	if err != nil {
		return "", err
	}
	neighborProject, sourceProject := taskProject(neighbor.path), taskProject(sourcePath)
	neighborRef, refErr := ticketRef(sourceProject, neighborProject, neighbor.slug)
	blockedRef, blockedErr := ticketRef(neighborProject, sourceProject, source.slug)
	if refErr != nil {
		fmt.Fprintf(errout, "tk: warning: blocking link %s from %s: %v\n", ref, source.slug, refErr)
		return newPath, refErr
	}
	if blockedErr != nil {
		fmt.Fprintf(errout, "tk: warning: blocking link %s from %s: %v\n", ref, source.slug, blockedErr)
		return newPath, blockedErr
	}
	fmt.Fprintf(out, "поднят %s %s → %s: блокирует %s\n", neighborRef, normalizePrio(neighbor.prio), normalizePrio(source.prio), blockedRef)
	return newPath, nil
}

func raiseNeighborLocked(neighbor, source task, sourcePath string, o options, ref string, errout io.Writer) (string, error) {
	name, err := retaggedName(filepath.Base(neighbor.path), options{prio: normalizePrio(source.prio)})
	if err != nil {
		fmt.Fprintf(errout, "tk: warning: raise %s: %v\n", ref, err)
		return "", err
	}
	newPath := filepath.Join(filepath.Dir(neighbor.path), name)
	blockedRef, err := ticketRef(taskProject(neighbor.path), taskProject(sourcePath), source.slug)
	if err != nil {
		fmt.Fprintf(errout, "tk: warning: raise %s: %v\n", ref, err)
		return "", err
	}
	updated := neighbor.body
	if blockPrioRank(source.prio) <= 1 && lineValue(updated, "Причина: ") == "" {
		updated, err = setHeaderValue(updated, "Причина: ", "блокирует "+blockedRef)
		if err != nil {
			fmt.Fprintf(errout, "tk: warning: raise %s: %v\n", ref, err)
			return "", err
		}
	}
	updated = journal(updated, fmt.Sprintf("- %s note — %s: приоритет %s → %s: блокирует %s", timeStamp(), o.by, normalizePrio(neighbor.prio), normalizePrio(source.prio), blockedRef))
	if newPath != neighbor.path {
		updated = journal(updated, renameJournal(o.by, neighbor.path, newPath))
		if _, statErr := os.Stat(newPath); statErr == nil {
			err = fmt.Errorf("target name already exists: %s", filepath.Base(newPath))
			fmt.Fprintf(errout, "tk: warning: raise %s: %v\n", ref, err)
			return "", err
		} else if !errors.Is(statErr, os.ErrNotExist) {
			fmt.Fprintf(errout, "tk: warning: raise %s: %v\n", ref, statErr)
			return "", statErr
		}
		if err = renameTaskFile(neighbor.path, newPath); err != nil {
			fmt.Fprintf(errout, "tk: warning: raise %s: %v\n", ref, err)
			return "", err
		}
	}
	if err = writeRenamedTask(neighbor.path, newPath, updated); err != nil {
		fmt.Fprintf(errout, "tk: warning: raise %s: %v\n", ref, err)
		return "", err
	}
	return newPath, nil
}

func transferredRecord(rows []transferRecord, id string) (transferRecord, bool) {
	id = strings.TrimSuffix(id, ".md")
	// The ledger only grows and a slug can come back: the latest record is the live one.
	for _, row := range slices.Backward(rows) {
		if row.Slug == id || row.Stem == id {
			return row, true
		}
	}
	return transferRecord{}, false
}

func appendTaskNote(path string, o options, note string) error {
	return updateTicket(path, func(s string) string { return journal(s, "- "+timeStamp()+" note — "+o.by+": "+note) })
}

func removeBlocks(t task, o options, errout io.Writer, refs []string) int {
	for _, raw := range refs {
		project, dir, id, _, err := resolveBlockLocation(o, raw)
		if err != nil {
			return fail(errout, err)
		}
		canonical, err := ticketRef(currentProject(o), project, id)
		if err != nil {
			return fail(errout, err)
		}
		back, err := ticketRef(project, currentProject(o), t.slug)
		if err != nil {
			return fail(errout, err)
		}
		_, err = mutateTask(dir, id, func(current task) (string, error) {
			if !slices.Contains(headerLinks(current.body, blockedPrefix), back) {
				return current.body, nil
			}
			body := removeHeaderLink(current.body, blockedPrefix, back)
			return journal(body, fmt.Sprintf("- %s note — %s: связь с %s снята", timeStamp(), o.by, back)), nil
		})
		missing := errors.Is(err, os.ErrNotExist)
		if err != nil && !missing {
			return fail(errout, err)
		}
		if missing {
			fmt.Fprintf(errout, "tk: warning: blocking ticket %s not found; removing reverse reference\n", raw)
		}
		_, err = mutateTask(filepath.Dir(t.path), t.slug, func(current task) (string, error) {
			if !slices.Contains(headerLinks(current.body, blocksPrefix), canonical) {
				return current.body, nil
			}
			body := removeHeaderLink(current.body, blocksPrefix, canonical)
			return journal(body, fmt.Sprintf("- %s note — %s: связь с %s снята", timeStamp(), o.by, canonical)), nil
		})
		if err != nil {
			return fail(errout, err)
		}
	}
	return 0
}

func blockMark(t task, errout io.Writer) string {
	project := taskProject(t.path)
	for _, ref := range headerLinks(t.body, blockedPrefix) {
		dir, path, err := linkedPath(project, ref)
		if err == nil {
			if isArchivedTask(path) {
				continue
			}
			if b, e := os.ReadFile(path); e == nil {
				st, statusErr := status(string(b))
				if statusErr != nil {
					fmt.Fprintf(errout, "tk: warning: cannot read blocking ticket %s: %v\n", ref, statusErr)
					continue
				}
				if st != "готово" && st != "отклонён" {
					return " заблокирован: " + ref
				}
			} else if !errors.Is(e, os.ErrNotExist) {
				fmt.Fprintf(errout, "tk: warning: cannot read blocking ticket %s: %v\n", ref, e)
			}
			continue
		}
		rows, ledgerErr := readTransferLedger(dir)
		if ledgerErr != nil {
			fmt.Fprintf(errout, "tk: warning: cannot read transfer ledger for %s: %v\n", ref, ledgerErr)
			continue
		}
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
	for _, ref := range headerLinks(t.body, blocksPrefix) {
		dir, id, back, err := reverseTaskRef(t, ref)
		if err != nil {
			fmt.Fprintf(errout, "tk: warning: cannot unblock %s: %v\n", ref, err)
			continue
		}
		removed := false
		if _, err = mutateTask(dir, id, func(current task) (string, error) {
			if !slices.Contains(headerLinks(current.body, blockedPrefix), back) {
				return current.body, nil
			}
			removed = true
			return journal(removeHeaderLink(current.body, blockedPrefix, back), "- "+timeStamp()+" note — "+o.by+": блокирующий "+back+" закрыт"), nil
		}); err != nil {
			fmt.Fprintf(errout, "tk: warning: cannot update %s: %v\n", ref, err)
			continue
		}
		if removed {
			fmt.Fprintln(out, "разблокирован "+ref)
		}
	}
	for _, ref := range headerLinks(t.body, blockedPrefix) {
		dir, id, back, err := reverseTaskRef(t, ref)
		if err != nil {
			fmt.Fprintf(errout, "tk: warning: cannot update %s: %v\n", ref, err)
			continue
		}
		if _, err = mutateTask(dir, id, func(current task) (string, error) {
			if !slices.Contains(headerLinks(current.body, blocksPrefix), back) {
				return current.body, nil
			}
			return journal(removeHeaderLink(current.body, blocksPrefix, back), "- "+timeStamp()+" note — "+o.by+": заблокированный "+back+" закрыт, приоритет не менялся: понизь, если не нужен"), nil
		}); err != nil {
			fmt.Fprintf(errout, "tk: warning: cannot update %s: %v\n", ref, err)
		}
	}
	return code
}

func dirProject(dir string) string {
	if filepath.Base(dir) == "archive" {
		dir = filepath.Dir(dir)
	}
	return filepath.Dir(filepath.Dir(dir))
}

func taskProject(path string) string {
	dir := filepath.Dir(path)
	if filepath.Base(dir) == "archive" {
		dir = filepath.Dir(dir)
	}
	return filepath.Dir(filepath.Dir(dir))
}

func reverseTaskRef(t task, ref string) (string, string, string, error) {
	project := taskProject(t.path)
	dir, id, err := referenceLocation(project, ref)
	if err != nil {
		return "", "", "", err
	}
	name, ok := parseTaskName(filepath.Base(t.path))
	if !ok {
		return "", "", "", fmt.Errorf("invalid task path %s", t.path)
	}
	back, err := ticketRef(dirProject(dir), project, name.slug)
	if err != nil {
		return "", "", "", err
	}
	return dir, id, back, nil
}

func notePriorityLowered(t task, o options, errout io.Writer) {
	if o.prio == "" || blockPrioRank(o.prio) <= blockPrioRank(t.prio) {
		return
	}
	for _, ref := range headerLinks(t.body, blockedPrefix) {
		dir, id, back, err := reverseTaskRef(t, ref)
		if err != nil {
			fmt.Fprintf(errout, "tk: warning: cannot notify %s: %v\n", ref, err)
			continue
		}
		if _, err = mutateTask(dir, id, func(current task) (string, error) {
			return journal(current.body, "- "+timeStamp()+" note — "+o.by+": заблокированный "+back+" понижен, приоритет не менялся: понизь, если не нужен"), nil
		}); err != nil {
			fmt.Fprintf(errout, "tk: warning: cannot notify %s: %v\n", ref, err)
		}
	}
}

func validatePriorityFloor(t task, o options, errout io.Writer) int {
	if o.prio == "" || blockPrioRank(o.prio) <= blockPrioRank(t.prio) {
		return 0
	}
	project := taskProject(t.path)
	for _, ref := range headerLinks(t.body, blocksPrefix) {
		dir, id, err := referenceLocation(project, ref)
		if err != nil {
			fmt.Fprintf(errout, "tk: cannot inspect blocking ticket %s: %v\n", ref, err)
			return exitUsage
		}
		unblocked := false
		for _, raw := range o.unblocks {
			up, ud, uid, _, resolveErr := resolveBlockLocation(o, raw)
			if resolveErr == nil && sameTicket(up, uid, dirProject(ud), id) {
				unblocked = true
				break
			}
		}
		if unblocked {
			continue
		}
		target, err := findWithArchive(dir, id)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			fmt.Fprintf(errout, "tk: cannot read blocking ticket %s: %v\n", ref, err)
			return exitUsage
		}
		if isArchivedTask(target.path) || target.status == "готово" || target.status == "отклонён" {
			continue
		}
		if blockPrioRank(o.prio) > blockPrioRank(target.prio) {
			fmt.Fprintf(errout, "tk: cannot lower priority below blocking ticket %s (%s); use --unblocks %s or lower that ticket first\n", ref, normalizePrio(target.prio), ref)
			return exitUsage
		}
	}
	return 0
}
