package main

import (
	"encoding/json"
	"errors"
	"github.com/aiseeq/akit/clihelp"
	"os"
	"os/exec"
	"testing"
)

func TestMachineHelp(t *testing.T) {
	clihelp.CheckSource(t, ".", machineHelp, []string{"parseArgs"}, map[string]string{"--": "terminates flag parsing"})
	if !machineHelp.Commands[0].DashArgs {
		t.Fatal("help does not describe leading-dash argument detection")
	}
	for _, old := range []string{"-1", "- item"} {
		opts, err := parseArgs([]string{old, "new", "file"})
		if err != nil {
			t.Errorf("parseArgs rejected OLD %q: %v", old, err)
			continue
		}
		if len(opts.args) != 3 || opts.args[0] != old {
			t.Errorf("parseArgs(%q) args = %q", old, opts.args)
		}
	}
	clihelp.CheckFlags(t, machineHelp, func(flag string) []string {
		args := []string{flag}
		if flag == "-n" {
			args = append(args, "1")
		}
		if flag == "-F" {
			args = append(args, "-")
		}
		return args
	}, func(flag string) []string { return []string{flag} }, func(args []string) error {
		_, err := parseArgs(args)
		if errors.Is(err, errHelp) || errors.Is(err, errHelpJSON) {
			return nil
		}
		return err
	})
	checkHelpJSON(t, "sub")
}

func checkHelpJSON(t *testing.T, name string) {
	t.Helper()
	if os.Getenv("AKIT_HELP_JSON_CHILD") == name {
		os.Args = []string{name, "--help-json"}
		main()
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestMachineHelp$")
	cmd.Env = append(os.Environ(), "AKIT_HELP_JSON_CHILD="+name)
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("--help-json: %v", err)
	}
	var got clihelp.Help
	if err := json.Unmarshal(raw, &got); err != nil || got.Tool != name {
		t.Fatalf("--help-json = %q, tool %q, error %v", raw, got.Tool, err)
	}
}
