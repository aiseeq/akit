package main

import (
	"fmt"
	"strings"
	"testing"
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
}
