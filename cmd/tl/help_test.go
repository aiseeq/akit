package main

import (
	"errors"
	"testing"

	"github.com/aiseeq/akit/clihelp"
	"time"
)

func TestMachineHelp(t *testing.T) {
	clihelp.CheckSource(t, ".", machineHelp, []string{"main", "start", "parseArgs", "setSwitch", "setFlag", "setOtherFlag"}, map[string]string{"-": "prefix used to identify flags", "--": "separator is refused; tl treats positional words as refs", "--heavy": "internal graft command argument", "--local": "internal graft command argument", "--limit": "recognized only to reject a foreign event-count flag", "--head": "recognized only to reject a foreign event-count flag", "-n": "recognized only to reject a foreign event-count flag"})
	if machineHelp.Commands[0].Rest != "args" || !machineHelp.Commands[0].NoDashdash {
		t.Fatal("help does not describe positional session or reference arguments")
	}
	if _, e := parseArgs([]string{"--", "session"}, time.Now()); e == nil {
		t.Error("parseArgs accepted -- as a separator")
	}
	values := map[string]string{"--role": "user", "--since": "1h", "--until": "1h", "--gap": "1m", "--key-gap": "1m", "--grep": "x", "--raw": "x", "-C": "0", "--project": "x", "--wait": "go", "--q": "50", "--max": "1", "--width": "1"}
	companions := map[string][]string{"--gap": {"--all", "--worktime", "--grep", "x"}, "--key-gap": {"--all", "--worktime", "--grep", "x"}, "--q": {"--wait", "go"}}
	clihelp.CheckFlags(t, machineHelp, func(f string) []string { return tlFlagArgs(f, values, companions) }, func(f string) []string { return tlMissingArgs(f, companions) }, func(a []string) error {
		_, e := parseArgs(a, time.Now())
		if errors.Is(e, errHelp) || errors.Is(e, errHelpJSON) {
			return nil
		}
		return e
	})
	clihelp.CheckHelpJSON(t, "tl", main)
}
func tlFlagArgs(f string, values map[string]string, companions map[string][]string) []string {
	if f == "-h" || f == "--help" || f == "--help-json" {
		return []string{f}
	}
	a := append([]string{}, companions[f]...)
	a = append(a, f)
	if v, ok := values[f]; ok {
		a = append(a, v)
	}
	if f == "--codex" || f == "--codex-ext" || f == "--project" || f == "--no-subagents" {
		a = append([]string{"--all"}, a...)
	}
	if f == "--worktime" {
		a = append(a, "--all", "--grep", "x")
	}
	return a
}
func tlMissingArgs(f string, companions map[string][]string) []string {
	if f == "-h" || f == "--help" || f == "--help-json" {
		return []string{f}
	}
	a := append([]string{}, companions[f]...)
	a = append(a, f)
	if f == "--codex" || f == "--codex-ext" || f == "--project" || f == "--no-subagents" {
		a = append([]string{"--all"}, a...)
	}
	if f == "--worktime" {
		a = append(a, "--all", "--grep", "x")
	}
	return a
}
