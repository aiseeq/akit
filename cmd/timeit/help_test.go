package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
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
	checkTimeitHelpJSON(t)
}

func checkTimeitHelpJSON(t *testing.T) {
	t.Helper()
	if os.Getenv("AKIT_HELP_JSON_CHILD") == "timeit" {
		os.Args = []string{"timeit", "--help-json"}
		main()
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestMachineHelp$")
	cmd.Env = append(os.Environ(), "AKIT_HELP_JSON_CHILD=timeit")
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("--help-json: %v", err)
	}
	var got clihelp.Help
	if err := json.Unmarshal(raw, &got); err != nil || got.Tool != "timeit" {
		t.Fatalf("--help-json = %q, tool %q, error %v", raw, got.Tool, err)
	}
}
