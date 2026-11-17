package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/shellcmd"
)

type reviewCase struct {
	code, command, output string
	codes                 []int
	faults, outcomes      []string
}

func TestReviewProbeAllForms(t *testing.T) {
	file, err := os.Open("review_cases.tsv")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	line := 0
	for scanner.Scan() {
		parts := strings.SplitN(scanner.Text(), "\t", 6)
		if len(parts) != 6 {
			t.Fatalf("case row %d has %d fields", line+1, len(parts))
		}
		line++
		t.Run(fmt.Sprintf("%02d", line), func(t *testing.T) {
			tc := reviewCase{
				code: parts[0], command: strings.ReplaceAll(parts[1], `\n`, "\n"), output: strings.ReplaceAll(parts[2], `\n`, "\n"),
				codes: parseReviewCodes(t, parts[3]), faults: strings.Split(parts[4], ","),
				outcomes: strings.Split(parts[5], ","),
			}
			runReviewCase(t, line, tc)
		})
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if line != 91 {
		t.Fatalf("review case count = %d, want 91", line)
	}
}

func TestReviewR2ProbeInputsCopied(t *testing.T) {
	probe, err := os.Open("review_r2_allcases.tsv")
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	cases, err := os.Open("review_cases.tsv")
	if err != nil {
		t.Fatal(err)
	}
	defer cases.Close()

	covered := map[string]bool{}
	caseScanner := bufio.NewScanner(cases)
	for caseScanner.Scan() {
		parts := strings.SplitN(caseScanner.Text(), "\t", 6)
		if len(parts) != 6 {
			t.Fatalf("case row has %d fields", len(parts))
		}
		covered[strings.Join([]string{parts[0], strings.ReplaceAll(parts[1], `\n`, "\n"), strings.ReplaceAll(parts[2], `\n`, "\n")}, "\t")] = true
	}
	if err := caseScanner.Err(); err != nil {
		t.Fatal(err)
	}

	probeScanner := bufio.NewScanner(probe)
	count := 0
	for probeScanner.Scan() {
		parts := strings.SplitN(probeScanner.Text(), "\t", 3)
		if len(parts) != 3 {
			t.Fatalf("probe row %d has %d fields", count+1, len(parts))
		}
		key := strings.Join([]string{parts[0], strings.ReplaceAll(parts[1], `\n`, "\n"), strings.ReplaceAll(parts[2], `\n`, "\n")}, "\t")
		if !covered[key] {
			t.Errorf("review probe row %d not covered by expected package cases: %q", count+1, parts[1])
		}
		count++
	}
	if err := probeScanner.Err(); err != nil {
		t.Fatal(err)
	}
	if count != 85 {
		t.Fatalf("copied reviewer probe has %d forms, want 85", count)
	}
}

func runReviewCase(t *testing.T, number int, tc reviewCase) {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	since := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	path := filepath.Join(home, ".local", "state", "worker", "sample", "codex", "sessions", "2026", "10", "31", "rollout-synthetic.jsonl")
	args, _ := json.Marshal(map[string]string{"cmd": tc.command})
	body := tc.output
	if tc.code != "-" {
		body = "Process exited with code " + tc.code + "\nOutput:\n" + tc.output
	} else {
		body = "Script completed\nOutput:\n" + tc.output
	}
	callLine, _ := json.Marshal(map[string]any{"timestamp": "2026-10-31T13:00:00Z", "type": "response_item", "payload": map[string]any{"type": "function_call", "name": "exec_command", "call_id": "c1", "arguments": string(args)}})
	resultLine, _ := json.Marshal(map[string]any{"timestamp": "2026-10-31T13:00:01Z", "type": "response_item", "payload": map[string]any{"type": "function_call_output", "call_id": "c1", "output": body}})
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(string(callLine)+"\n"+string(resultLine)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, until, until); err != nil {
		t.Fatal(err)
	}
	u := newRun(since, until, 1, filepath.Join(root, "out"), home)
	if err := u.runUptake(since, until); err != nil {
		t.Fatal(err)
	}

	commandCode := -1
	if tc.code != "-" {
		commandCode, _ = strconv.Atoi(tc.code)
	}
	calls, err := shellcmd.Find(tc.command, "sub", "tl")
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != len(tc.codes) || len(calls) != len(tc.faults) || len(calls) != len(tc.outcomes) {
		t.Fatalf("%q has %d parsed calls; expected codes=%v faults=%v outcomes=%v", tc.command, len(calls), tc.codes, tc.faults, tc.outcomes)
	}
	ordinals := map[string]int{}
	wantMetrics := map[string]int{}
	for i, call := range calls {
		ordinal := ordinals[call.Name]
		ordinals[call.Name]++
		got := u.attributeUtility(call.Name, tc.command, body, commandCode, ordinal)
		gotOutcome := u.outcome(call.Name, got.output, got.code)
		if gotOutcome == "unknown" && u.callHasTruncator(call.Name, tc.command, ordinal) {
			gotOutcome = "truncated"
		}
		wantFault := tc.faults[i]
		if wantFault == "-" {
			wantFault = ""
		}
		if got.code != tc.codes[i] || got.fault != wantFault || gotOutcome != tc.outcomes[i] {
			t.Errorf("call %d: %q, exit %s, output %q -> code=%d fault=%q outcome=%q; want code=%d fault=%q outcome=%q",
				i+1, tc.command, tc.code, tc.output, got.code, got.fault, gotOutcome, tc.codes[i], wantFault, tc.outcomes[i])
		}
		wantMetrics[call.Name+"\tcalls"]++
		wantMetrics[call.Name+"\t"+u.callMetric(tc.codes[i])]++
		wantMetrics[call.Name+"\t"+tc.outcomes[i]]++
		if wantFault != "" {
			wantMetrics[call.Name+"\t"+wantFault]++
		}
	}
	gotMetrics := map[string]int{}
	for key, value := range u.counts {
		fields := strings.Split(key, "\t")
		if len(fields) == 4 && value > 0 && (fields[2] == "tl" || fields[2] == "sub") {
			gotMetrics[fields[2]+"\t"+fields[3]] += value
		}
	}
	if !reflect.DeepEqual(gotMetrics, wantMetrics) {
		t.Errorf("case %d aggregate counts = %v; want %v", number, gotMetrics, wantMetrics)
	}
	t.Logf("%s; command exit %s; output %q -> %v", tc.command, tc.code, tc.output, reviewCallResults(tc.codes, tc.faults, tc.outcomes))
}

func TestRepeatedToolFaultIgnoresSeparateCommands(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	for _, tc := range []struct {
		command   string
		code      int
		output    string
		want      int
		wantFault string
	}{
		{"grep 'tl: invalid ref a' notes; tl a", 0, "tl: invalid ref a\n", 0, "refusal"},
		{"tl a; cat notes", 0, "tl: invalid ref a\n", -1, "refusal"},
		{"cat > f <<EOF\nbody\nEOF\ntl a", 1, "tl: invalid ref a\n", 1, "refusal"},
		{`tl a "$(cat notes)"`, 1, "tl: invalid ref a\n", 1, "refusal"},
	} {
		calls, err := shellcmd.Find(tc.command, "tl")
		if err != nil || len(calls) != 1 {
			t.Fatalf("Find(%q) = %#v, %v", tc.command, calls, err)
		}
		got := u.attributeUtility("tl", tc.command, tc.output, tc.code, 0)
		if got.code != tc.want {
			t.Errorf("%q: code=%d, want %d", tc.command, got.code, tc.want)
		}
		if got.fault != tc.wantFault {
			calls, _ := shellcmd.Calls(tc.command)
			t.Logf("parsed source calls: %+v", calls)
			t.Errorf("%q: fault=%q, want %q", tc.command, got.fault, tc.wantFault)
		}
	}
}

func parseReviewCodes(t *testing.T, value string) []int {
	t.Helper()
	var codes []int
	for item := range strings.SplitSeq(value, ",") {
		if item == "?" {
			codes = append(codes, -1)
			continue
		}
		code, err := strconv.Atoi(item)
		if err != nil {
			t.Fatalf("invalid review code %q: %v", item, err)
		}
		codes = append(codes, code)
	}
	return codes
}

func reviewCallResults(codes []int, faults, outcomes []string) []string {
	results := make([]string, len(codes))
	for i, code := range codes {
		fault := faults[i]
		if fault == "-" {
			fault = "none"
		}
		results[i] = fmt.Sprintf("exit=%s fault=%s outcome=%s", metricCode(code), fault, outcomes[i])
	}
	return results
}

func metricCode(code int) string {
	if code < 0 {
		return "unknown"
	}
	return strconv.Itoa(code)
}
