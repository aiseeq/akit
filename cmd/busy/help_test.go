package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/aiseeq/akit/clihelp"
)

func TestMachineHelp(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	clihelp.CheckSource(t, ".", machineHelp, []string{"parseArgs", "setSwitch", "set"}, map[string]string{"--": "prefix that distinguishes flags from positional arguments"})
	if !machineHelp.Commands[0].NoDashdash {
		t.Fatal("help does not say that -- is refused")
	}
	if _, err := parseArgs([]string{"--"}, time.Now()); err == nil {
		t.Error("parseArgs accepted --")
	}
	values := map[string]string{"--runnable": "2", "--mem-some": "5", "--io-full": "5", "--dirty-mb": "10", "--window": "1s", "--proc": "/proc", "--half-until": "14:45", "--half": "1h", "--quiet-until": "14:45", "--quiet": "1h", "--why": "test"}
	clihelp.CheckFlags(t, machineHelp, func(flag string) []string {
		args := []string{flag}
		if value, ok := values[flag]; ok {
			args = append(args, value)
		}
		if flag == "--why" {
			args = append([]string{"--quiet", "1h"}, args...)
		}
		return args
	}, func(flag string) []string { return []string{flag} }, func(args []string) error {
		_, err := parseArgs(args, time.Now())
		if errors.Is(err, errHelp) || errors.Is(err, errHelpJSON) {
			return nil
		}
		return err
	})
	checkBusyHelpJSON(t)
}

func checkBusyHelpJSON(t *testing.T) {
	t.Helper()
	if os.Getenv("AKIT_HELP_JSON_CHILD") == "busy" {
		os.Args = []string{"busy", "--help-json"}
		main()
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestMachineHelp$")
	cmd.Env = append(os.Environ(), "AKIT_HELP_JSON_CHILD=busy")
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("--help-json: %v", err)
	}
	var got clihelp.Help
	if err := json.Unmarshal(raw, &got); err != nil || got.Tool != "busy" {
		t.Fatalf("--help-json = %q, tool %q, error %v", raw, got.Tool, err)
	}
}
