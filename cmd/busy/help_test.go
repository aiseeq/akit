package main

import (
	"errors"
	"testing"
	"time"

	"github.com/aiseeq/akit/clihelp"
)

func TestMachineHelp(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	// A fixed clock keeps the sample --half-until and --quiet-until times in the future.
	now := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.Local)
	clihelp.CheckSource(t, ".", machineHelp, []string{"parseArgs", "setSwitch", "set"}, map[string]string{"--": "prefix that distinguishes flags from positional arguments"})
	if !machineHelp.Commands[0].NoDashdash {
		t.Fatal("help does not say that -- is refused")
	}
	if _, err := parseArgs([]string{"--"}, now); err == nil {
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
		_, err := parseArgs(args, now)
		if errors.Is(err, errHelp) || errors.Is(err, errHelpJSON) {
			return nil
		}
		return err
	})
	clihelp.CheckHelpJSON(t, "busy", main)
}
