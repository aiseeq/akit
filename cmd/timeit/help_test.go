package main

import (
	"errors"
	"testing"

	"github.com/aiseeq/akit/clihelp"
)

func TestMachineHelp(t *testing.T) {
	clihelp.CheckSource(t, ".", machineHelp, []string{"parseArgs", "setValueFlag"}, map[string]string{"--": "starts the wrapped command"})
	if machineHelp.Commands[0].Rest != "only-after-dashdash" || !machineHelp.Commands[0].NoEquals {
		t.Fatal("help does not describe the positional and equals restrictions")
	}
	if _, err := parseArgs([]string{"--runs=5", "--", "true"}); err == nil {
		t.Error("parseArgs accepted --runs=5")
	}
	if _, err := parseArgs([]string{"--runs", "5", "word", "--", "true"}); err == nil {
		t.Error("parseArgs accepted a word before --")
	}
	values := map[string]string{"--runs": "1", "--budget": "1s", "--limit": "1s", "--ok-exit": "0", "--fail-on-stderr": "x", "--stdin": "/dev/null"}
	clihelp.CheckFlags(t, machineHelp, func(flag string) []string {
		args := []string{flag}
		if value, ok := values[flag]; ok {
			args = append(args, value)
		}
		if flag == "--" {
			args = append(args, "true")
		} else {
			args = append(args, "--", "true")
		}
		return args
	}, func(flag string) []string {
		if flag == "-h" || flag == "--help" || flag == "--help-json" {
			return []string{flag}
		}
		return []string{flag, "--", "true"}
	}, func(args []string) error {
		_, err := parseArgs(args)
		if errors.Is(err, errHelp) || errors.Is(err, errHelpJSON) {
			return nil
		}
		return err
	})
	clihelp.CheckHelpJSON(t, "timeit", main)
}
