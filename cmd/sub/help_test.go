package main

import (
	"errors"
	"testing"

	"github.com/aiseeq/akit/clihelp"
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
	clihelp.CheckHelpJSON(t, "sub", main)
}
