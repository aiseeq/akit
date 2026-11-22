package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/aiseeq/akit/queuegate"
)

func TestTicketLookupDoesNotParseUnrelatedPlans(t *testing.T) {
	var body strings.Builder
	body.WriteString("# Another ticket\n\nСтатус: открыт\n\n## План\n")
	for i := range 40 {
		fmt.Fprintf(&body, "- check-%d: true\n", i)
	}
	body.WriteString("\n## Журнал\n- 2026-11-01 10:00 переименован — worker: former → other\n")
	ticket := task{path: "/repo/docs/tasks/20261101-1000-MED-other.md", slug: "other", body: body.String()}
	if !matchesID(ticket, "former") || matchesID(ticket, "absent") {
		t.Fatal("lookup must preserve old names and reject unrelated names")
	}
	allocs := testing.AllocsPerRun(3, func() { _ = matchesID(ticket, "absent") })
	if allocs > 100 {
		t.Fatalf("looking up an unrelated ID allocated %.0f times; ticket plans should not be parsed", allocs)
	}
	ticket.status = "открыт"
	if !eligibleForNext(ticket, "worker") {
		t.Fatal("open ticket should be eligible")
	}
	allocs = testing.AllocsPerRun(3, func() { _ = eligibleForNext(ticket, "worker") })
	if allocs > 100 {
		t.Fatalf("checking an open ticket allocated %.0f times; ticket plans should not be parsed", allocs)
	}

	working := ticketWithPlan("working", "в работе", "Исполнитель: worker\n")
	gate := queuegate.Scope{Valid: true}
	if !eligibleForNext(working, "worker") {
		t.Fatal("ticket assigned to the actor should be eligible")
	}
	if got := nextGroup(working, "worker", gate); got != 1 {
		t.Fatalf("nextGroup(working) = %d, want 1", got)
	}
	allocs = testing.AllocsPerRun(3, func() {
		_ = eligibleForNext(working, "worker")
		_ = nextGroup(working, "worker", gate)
	})
	if allocs > 100 {
		t.Fatalf("checking a working ticket allocated %.0f times; ticket plans should not be parsed", allocs)
	}

	waiting := ticketWithPlan("waiting", "ждёт", "Ждёт: 2020-01-01 10:00\n")
	if !eligibleForNext(waiting, "worker") {
		t.Fatal("ticket with a past wait deadline should be eligible")
	}
	allocs = testing.AllocsPerRun(3, func() { _ = eligibleForNext(waiting, "worker") })
	if allocs > 100 {
		t.Fatalf("checking a due ticket allocated %.0f times; ticket plans should not be parsed", allocs)
	}

	blocked := ticketWithPlan("a-blocked", "открыт", "Заблокирован: z-blocker\n")
	free := ticketWithPlan("b-free", "открыт", "")
	blocker := task{slug: "z-blocker", status: "в работе", body: "Исполнитель: someone-else\n"}
	ordered := orderedNextCandidates([]task{blocked, free, blocker}, "worker", gate)
	if len(ordered) != 2 || ordered[0].slug != "b-free" || ordered[1].slug != "a-blocked" {
		t.Fatalf("ordered candidates = %v, want b-free before a-blocked", taskSlugs(ordered))
	}
	allocs = testing.AllocsPerRun(3, func() {
		_ = orderedNextCandidates([]task{blocked, free, blocker}, "worker", gate)
	})
	if allocs > 100 {
		t.Fatalf("ordering candidates allocated %.0f times; ticket plans should not be parsed", allocs)
	}
}

func ticketWithPlan(slug, status, fields string) task {
	var body strings.Builder
	fmt.Fprintf(&body, "# %s\n\nСтатус: %s\n%s\n## План\n", slug, status, fields)
	for i := range 40 {
		fmt.Fprintf(&body, "- check-%d: true\n", i)
	}
	return task{path: "/repo/docs/tasks/" + slug + ".md", slug: slug, status: status, prio: "MED", body: body.String()}
}
