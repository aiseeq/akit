package clihelp

import (
	"encoding/json"
	"os"
	"os/exec"
)

// CheckHelpJSON runs the current test as a child, asks its command to print JSON help,
// and checks that the output identifies the expected tool.
func CheckHelpJSON(t TestReporter, tool string, runMain func()) {
	t.Helper()
	if os.Getenv("AKIT_HELP_JSON_CHILD") == tool {
		os.Args = []string{tool, "--help-json"}
		runMain()
		os.Exit(0)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestMachineHelp$")
	cmd.Env = append(os.Environ(), "AKIT_HELP_JSON_CHILD="+tool)
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("--help-json: %v", err)
		return
	}
	var got Help
	if err := json.Unmarshal(raw, &got); err != nil || got.Tool != tool {
		t.Fatalf("--help-json = %q, tool %q, error %v", raw, got.Tool, err)
	}
}
