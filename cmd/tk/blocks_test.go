package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCloseBlockingRelations(t *testing.T) {
	for _, tc := range []struct {
		name, closedSlug, closedBody, neighborSlug, neighborBody, wantOut, wantRemoved string
	}{
		{
			name: "blocking ticket closes", closedSlug: "x", closedBody: "# x\n\nСтатус: готово\nБлокирует: t\n",
			neighborSlug: "t", neighborBody: "# t\n\nСтатус: открыт\nЗаблокирован: x\n",
			wantOut: "разблокирован t", wantRemoved: blockedPrefix + "x",
		},
		{
			name: "blocked ticket closes", closedSlug: "t", closedBody: "# t\n\nСтатус: готово\nЗаблокирован: x\n",
			neighborSlug: "x", neighborBody: "# x\n\nСтатус: открыт\nБлокирует: t\n",
			wantRemoved: blocksPrefix + "t",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "docs", "tasks")
			if err := os.MkdirAll(dir, 0755); err != nil {
				t.Fatal(err)
			}
			closedPath := filepath.Join(dir, "20260101-0000-MED-"+tc.closedSlug+".md")
			neighborPath := filepath.Join(dir, "20260101-0000-MED-"+tc.neighborSlug+".md")
			if err := os.WriteFile(closedPath, []byte(tc.closedBody), 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(neighborPath, []byte(tc.neighborBody), 0644); err != nil {
				t.Fatal(err)
			}
			var out, errout bytes.Buffer
			closed, err := find(dir, tc.closedSlug)
			if err != nil {
				t.Fatal(err)
			}
			if code := closeBlockRelations(closed, options{by: "tester"}, &out, &errout, 0); code != 0 {
				t.Fatalf("code=%d stderr=%s", code, errout.String())
			}
			got, err := os.ReadFile(neighborPath)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(got), tc.wantRemoved) {
				t.Fatalf("relation remains:\n%s", got)
			}
			if tc.wantOut != "" && !strings.Contains(out.String(), tc.wantOut) {
				t.Fatalf("stdout=%q, want %q", out.String(), tc.wantOut)
			}
		})
	}
}

func TestAttachBlocksRejectsClosedTargetWithoutWrites(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "docs", "tasks")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	xPath := filepath.Join(dir, "20260101-0000-MED-x.md")
	tPath := filepath.Join(dir, "20260101-0000-MED-t.md")
	xBody := "# x\n\nСтатус: открыт\n"
	if err := os.WriteFile(xPath, []byte(xBody), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tPath, []byte("# t\n\nСтатус: готово\n"), 0644); err != nil {
		t.Fatal(err)
	}
	x, err := find(dir, "x")
	if err != nil {
		t.Fatal(err)
	}
	var out, errout bytes.Buffer
	if code := attachBlocks(x, options{baseRoot: root, mainRoot: root, by: "tester"}, &out, &errout, []string{"t"}); code == 0 {
		t.Fatal("closed target accepted")
	}
	got, err := os.ReadFile(xPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != xBody {
		t.Fatalf("blocker changed on rejected link:\n%s", got)
	}
}
