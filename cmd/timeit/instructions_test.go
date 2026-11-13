package main

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestParsePerfInstructions(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int64
		err  string
	}{
		{name: "hybrid sum without scaling", in: "# started on x\n\n2584271977,,cpu_atom/instructions/u,908508704,99.00,,\n1358,,cpu_core/instructions/u,5714730,0.00,,\n", want: 2584273335},
		{name: "core type not used", in: "100,,cpu_atom/instructions/u,1,100.00,,\n<not counted>,,cpu_core/instructions/u,0,0.00,,\n", want: 100},
		{name: "single pmu", in: "42,,instructions:u,1,100.00,,\n", want: 42},
		{name: "nothing counted", in: "<not counted>,,instructions:u,0,0.00,,\n", err: "counted no instructions"},
		{name: "not supported", in: "<not supported>,,instructions:u,0,0.00,,\n", err: "paranoid"},
		{name: "empty", in: "", err: "counted no instructions"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePerfInstructions([]byte(tt.in))
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("err %v, want %q", err, tt.err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("got %d, %v; want %d", got, err, tt.want)
			}
		})
	}
}

func TestMedianCount(t *testing.T) {
	if got := medianCount([]int64{9, 1, 5}); got != 5 {
		t.Fatalf("odd: %d", got)
	}
	if got := medianCount([]int64{10, 4, 7, 1}); got != 5 {
		t.Fatalf("even: %d", got)
	}
}

func TestInstructionsFlag(t *testing.T) {
	opts, err := parseArgs([]string{"--instructions", "--runs", "2", "--", "true"})
	if err != nil || !opts.instructions || opts.runs != 2 {
		t.Fatalf("%+v, %v", opts, err)
	}
	if args := perfArgs([]string{"prog", "--x"}); strings.Join(args, " ") != "perf stat --no-scale -x, -e instructions:u --log-fd 3 -- prog --x" {
		t.Fatalf("perf args %q", args)
	}
}

func TestRunPrintsMedianInstructions(t *testing.T) {
	counts := []int64{300, 100, 200}
	var out, stderr bytes.Buffer
	opts := options{runs: 3, budget: time.Hour, limit: time.Hour, okExit: map[int]bool{0: true}, instructions: true}
	i := 0
	code := run(opts, &out, &stderr, time.Now, func(options) (result, time.Duration, error) {
		res := result{instructions: counts[i]}
		i++
		return res, time.Second, nil
	})
	if code != 0 || strings.TrimSpace(out.String()) != "200" {
		t.Fatalf("code %d, out %q, stderr %q", code, out.String(), stderr.String())
	}
}

func TestRunReportsCountError(t *testing.T) {
	var out, stderr bytes.Buffer
	opts := options{runs: 1, budget: time.Hour, limit: time.Hour, okExit: map[int]bool{0: true}, instructions: true}
	code := run(opts, &out, &stderr, time.Now, func(options) (result, time.Duration, error) {
		_, err := parsePerfInstructions(nil)
		return result{countErr: err}, time.Second, nil
	})
	if code != exitError || out.Len() != 0 || !strings.Contains(stderr.String(), "perf_event_paranoid") {
		t.Fatalf("code %d, out %q, stderr %q", code, out.String(), stderr.String())
	}
}

func TestRunCommandCountsInstructions(t *testing.T) {
	if _, err := exec.LookPath("perf"); err != nil {
		t.Skip("perf not installed")
	}
	opts := options{instructions: true, command: []string{"true"}}
	res, _, err := runCommand(opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupResult(res)
	if res.countErr != nil {
		t.Skipf("perf cannot count here: %v", res.countErr)
	}
	if res.code != 0 || res.instructions <= 0 {
		t.Fatalf("code %d, instructions %d", res.code, res.instructions)
	}
}

func TestInstructionsEnv(t *testing.T) {
	got := instructionsEnv([]string{"A=1"})
	if strings.Join(got, " ") != "A=1 GOMAXPROCS=1" {
		t.Fatalf("unset: %q", got)
	}
	if got := instructionsEnv([]string{"GOMAXPROCS=4"}); strings.Join(got, " ") != "GOMAXPROCS=4" {
		t.Fatalf("set: %q", got)
	}
}

func TestCountErrorWinsOverExitAndStderrMatch(t *testing.T) {
	var out, stderr bytes.Buffer
	opts := options{runs: 1, budget: time.Hour, limit: time.Hour, okExit: map[int]bool{0: true}, instructions: true, failOnStderr: "Error"}
	code := run(opts, &out, &stderr, time.Now, func(options) (result, time.Duration, error) {
		_, err := parsePerfInstructions(nil)
		return result{code: 1, stderrData: []byte("Error:\nNo supported events found.\n"), countErr: err}, time.Second, nil
	})
	if code != exitError || out.Len() != 0 || !strings.Contains(stderr.String(), "No supported events") || !strings.Contains(stderr.String(), "perf_event_paranoid") || strings.Contains(stderr.String(), "stderr has") {
		t.Fatalf("code %d, out %q, stderr %q", code, out.String(), stderr.String())
	}
}

func TestMissingCommandUnderPerfIsLaunchError(t *testing.T) {
	if _, err := exec.LookPath("perf"); err != nil {
		t.Skip("perf not installed")
	}
	var out, stderr bytes.Buffer
	opts := options{runs: 1, budget: time.Hour, limit: time.Hour, okExit: map[int]bool{0: true}, instructions: true, command: []string{"/no/such/timeit-command"}}
	code := run(opts, &out, &stderr, time.Now, func(o options) (result, time.Duration, error) { return runCommand(o, nil) })
	if code != exitError {
		t.Fatalf("code %d, stderr %q", code, stderr.String())
	}
}

func TestInstructionsChildSeesGOMAXPROCS(t *testing.T) {
	if _, err := exec.LookPath("perf"); err != nil {
		t.Skip("perf not installed")
	}
	t.Setenv("GOMAXPROCS", "")
	if err := os.Unsetenv("GOMAXPROCS"); err != nil {
		t.Fatal(err)
	}
	res, _, err := runCommand(options{instructions: true, command: []string{"/bin/sh", "-c", `test "$GOMAXPROCS" = 1`}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupResult(res)
	if res.countErr != nil {
		t.Skipf("perf cannot count here: %v", res.countErr)
	}
	if res.code != 0 {
		t.Fatalf("child did not get GOMAXPROCS=1: exit %d", res.code)
	}
}
