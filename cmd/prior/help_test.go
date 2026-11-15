package main

import (
	"errors"
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
	clihelp.CheckHelpJSON(t, "prior", main)
}
