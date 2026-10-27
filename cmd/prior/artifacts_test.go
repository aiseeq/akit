package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestArtifacts(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, ".claude/improve/state/artifacts.tsv"), "# чей, название, ссылка, обновлён\n"+
		"mine\tDemo в движении\thttps://claude.ai/artifact/b\t2026-10-16\n"+
		"mine\tКак Demo подключить оператора\thttps://claude.ai/artifact/c\t2026-10-01\n"+
		"кривая строка\n"+
		"shared\tDemo в Claude\thttps://claude.ai/artifact/a\t2026-10-26\n"+
		"mine\tВыплаты\thttps://claude.ai/artifact/d\t2026-10-26\n")
	got := sectionOf(runPrior(t, home, "инструкция", "Demo", "оператор"), "артефакты")
	want := []string{
		"(строка не из 4 колонок: кривая строка)",
		"«Как Demo подключить оператора» https://claude.ai/artifact/c 2026-10-01 (mine)",
		"«Demo в Claude» https://claude.ai/artifact/a 2026-10-26 (shared)",
		"«Demo в движении» https://claude.ai/artifact/b 2026-10-16 (mine)",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("артефакты:\n%s", strings.Join(got, "\n"))
	}
	if got := sectionOf(runPrior(t, t.TempDir(), "x"), "артефакты"); len(got) != 1 || !strings.HasPrefix(got[0], "(нет списка: ") {
		t.Errorf("нет файла: %q", got)
	}
}

func TestMemories(t *testing.T) {
	home := t.TempDir()
	p := filepath.Join(home, ".claude/projects")
	mem := func(project, name, description, body string) {
		write(t, filepath.Join(p, project, "memory", name+".md"), fmt.Sprintf("---\nname: %s\ndescription: %q\nmetadata:\n"+
			"  type: reference\n  originSessionId: demo-оператор\n---\n\n%s\n", name, description, body))
	}
	mem("-home-user-work", "demo-operator-guide-artifact", "Инструкция подключения оператора Demo - Artifact",
		"Пошаговая инструкция для нового оператора Demo.\nDemo\n**Why:** забыл")
	mem("-home-user-git-x", "demo-stand", "стенд", "Demo стенд\nDemo ещё")
	mem("-home-user-git-x", "other", "другое", "служебные поля шапки не в счёт")
	write(t, filepath.Join(p, "-home-user-work/memory/MEMORY.md"), "- [Demo](demo.md) — инструкция оператора\n")
	got := sectionOf(runPrior(t, home, "инструкция", "Demo", "оператор"), "память")
	want := []string{
		"-home-user-work/memory/demo-operator-guide-artifact.md: Инструкция подключения оператора Demo - Artifact",
		"  Пошаговая инструкция для нового оператора Demo.",
		"-home-user-git-x/memory/demo-stand.md: стенд",
		"  Demo стенд",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("память:\n%s", strings.Join(got, "\n"))
	}
}
