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
	clihelp.CheckSource(t, ".", machineHelp, []string{"parseArgs"}, map[string]string{"-": "prefix used to reject unrecognized leading-dash words"})
	if !machineHelp.Commands[0].NoDashdash {
		t.Fatal("help does not describe -- as an ordinary search word")
	}
	if _, err := parseArgs([]string{"--", "foo"}); err == nil {
		t.Error("parseArgs accepted -- as a separator")
	}
	if !machineHelp.Commands[0].NoEquals {
		t.Fatal("help does not say that --flag=value is refused")
	}
	for _, flag := range []string{"-i=1", "--ignore-case=1"} {
		if _, err := parseArgs([]string{flag, "foo"}); err == nil {
			t.Errorf("parseArgs accepted %s", flag)
		}
	}
	clihelp.CheckFlags(t, machineHelp, func(flag string) []string {
		args := []string{flag}
		if flag != "-h" && flag != "--help" && flag != "--help-json" {
			args = append(args, "topic")
		}
		return args
	}, func(flag string) []string {
		if flag == "-h" || flag == "--help" || flag == "--help-json" {
			return []string{flag}
		}
		return []string{flag, "topic"}
	}, func(args []string) error {
		_, err := parseArgs(args)
		if errors.Is(err, errHelp) || errors.Is(err, errHelpJSON) {
			return nil
		}
		return err
	})
	checkPriorHelpJSON(t)
}

func checkPriorHelpJSON(t *testing.T) {
	t.Helper()
	if os.Getenv("AKIT_HELP_JSON_CHILD") == "prior" {
		os.Args = []string{"prior", "--help-json"}
		main()
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestMachineHelp$")
	cmd.Env = append(os.Environ(), "AKIT_HELP_JSON_CHILD=prior")
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("--help-json: %v", err)
	}
	var got clihelp.Help
	if err := json.Unmarshal(raw, &got); err != nil || got.Tool != "prior" {
		t.Fatalf("--help-json = %q, tool %q, error %v", raw, got.Tool, err)
	}
}
