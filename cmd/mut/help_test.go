package main

import (
	"errors"
	"testing"

	"github.com/aiseeq/akit/clihelp"
)

func TestMachineHelp(t *testing.T) {
	clihelp.CheckSource(t, ".", machineHelp, []string{"main", "parseArgs"}, map[string]string{"--": "ends option parsing and begins go test args", "-": "prefix used to reject unknown flags", "-count": "internal go test argument normalization", "-json": "internal go test JSON output mode", "--heavy": "internal graft command argument", "--- SKIP": "go test output marker, not a mut flag"})
	if machineHelp.Commands[0].Rest != "args" || machineHelp.Commands[0].DashArgs {
		t.Fatal("help does not describe positional and test arguments")
	}
	if _, err := parseArgs([]string{"-1", "b", "f", "--", "x"}); err == nil {
		t.Fatal("parseArgs accepted a leading-dash OLD argument before --")
	}
	if _, err := parseArgs([]string{"old", "new", "file.go", "--", "-count=1"}); err != nil {
		t.Fatalf("parseArgs rejected OLD NEW FILE before --: %v", err)
	}
	for _, a := range [][]string{{"--test-db"}} {
		if _, e := parseArgs(a); e == nil {
			t.Errorf("accepted %q", a)
		}
	}
	vals := map[string]string{"-F": "-", "--env": "PATH"}
	clihelp.CheckFlags(t, machineHelp, func(f string) []string {
		a := []string{f}
		if v, ok := vals[f]; ok {
			a = append(a, v)
		}
		if f == "-h" || f == "--help" || f == "--help-json" {
			return a
		}
		return append(a, "--", "-count=1")
	}, func(f string) []string {
		if f == "-h" || f == "--help" || f == "--help-json" {
			return []string{f}
		}
		return []string{f, "--", "-count=1"}
	}, func(a []string) error {
		_, e := parseArgs(a)
		if errors.Is(e, errHelp) || errors.Is(e, errHelpJSON) {
			return nil
		}
		return e
	})
	clihelp.CheckHelpJSON(t, "mut", main)
}
