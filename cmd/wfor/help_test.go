package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/clihelp"
)

func TestMachineHelp(t *testing.T) {
	if !strings.Contains(usage, "--help-json") {
		t.Fatal("-h usage does not mention --help-json")
	}
	clihelp.CheckSource(t, ".", machineHelp, []string{"main", "parseArgs", "arg"}, map[string]string{"-": "prefix used to reject unknown flags"})
	if !machineHelp.Commands[0].NoEquals || !machineHelp.Commands[0].NoDashdash || machineHelp.Commands[0].Rest != "args" {
		t.Fatal("help does not describe argument parsing")
	}
	for _, args := range [][]string{{"--timeout=1s", "idle"}, {"--", "idle"}} {
		if _, err := parseArgs(args, time.Now); err == nil {
			t.Errorf("accepted %q", args)
		}
	}
	values := map[string]string{"--timeout": "1s", "--every": "1s", "--after": "1"}
	clihelp.CheckFlags(t, machineHelp, func(f string) []string {
		a := []string{f}
		if v, ok := values[f]; ok {
			a = append(a, v)
		}
		if f == "-h" || f == "--help" || f == "--help-json" {
			return a
		}
		return append(a, "idle")
	}, func(f string) []string {
		if f == "-h" || f == "--help" || f == "--help-json" {
			return []string{f}
		}
		return []string{f, "idle"}
	}, func(a []string) error {
		_, e := parseArgs(a, time.Now)
		if errors.Is(e, errHelp) || errors.Is(e, errHelpJSON) {
			return nil
		}
		return e
	})
	clihelp.CheckHelpJSON(t, "wfor", main)
}
