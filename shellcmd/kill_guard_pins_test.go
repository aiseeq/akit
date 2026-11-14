package shellcmd

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/syntax"
)

func TestUnknownWrapperRoutesPinned(t *testing.T) {
	args := []string{"--unknown", "bash", "-s"}
	f, err := syntax.NewParser().Parse(strings.NewReader("strace --unknown bash -s"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := pipedShellName(f.Stmts[0], "strace --unknown bash -s", nil); ok {
		t.Error("unknown pipeline wrapper accepted")
	}
	if _, _, _, ok := shellInputNextTarget("strace", args, 0); ok {
		t.Error("unknown shell input wrapper accepted")
	}
	c := cmd{call: Call{Name: "strace", Args: []string{"--unknown", "sudo", "-s"}}}
	if _, ok := sudoShellTarget(c); ok {
		t.Error("unknown sudo route accepted")
	}
	if _, _, ok := unwrap("strace", args); ok {
		t.Error("unknown wrapper accepted")
	}
}

func TestWrapperFlagsPreserveKill(t *testing.T) {
	for _, src := range []string{"strace -fo /tmp/x kill -1", "unshare -Urf --mount-proc kill -1", "sudo -A kill -1"} {
		calls, err := Find(src, "kill")
		if err != nil || len(calls) == 0 {
			t.Errorf("%s: calls %v, err %v", src, calls, err)
		}
		name, args, _ := strings.Cut(src, " ")
		if !wrapperOptionsKnown(name, strings.Fields(args)) {
			t.Errorf("%s: flags rejected", src)
		}
	}
}

func nestedPins(depth int, wrapper string) string {
	var b strings.Builder
	for i := range depth {
		b.WriteString(wrapper + " <<'T" + strconv.Itoa(i) + "'\n")
	}
	b.WriteString(strings.Repeat("# padding\n", 150) + "kill -1\n")
	for i := depth - 1; i >= 0; i-- {
		b.WriteString("T" + strconv.Itoa(i) + "\n")
	}
	return b.String()
}
func TestNestedPins(t *testing.T) {
	for _, tc := range []struct {
		depth   int
		wrapper string
	}{{30, "bash"}, {10, "sudo -u a nice timeout 5 bash -s"}} {
		calls, err := Find(nestedPins(tc.depth, tc.wrapper), "kill")
		if err != nil || len(calls) == 0 {
			t.Errorf("depth %d: kills %d err %v", tc.depth, len(calls), err)
		}
	}
}
func TestNestedBudgetGrowthPins(t *testing.T) {
	a, b := newNestedParseBudget(10000).limit, newNestedParseBudget(20000).limit
	if b != 2*a || a != (maxNestedScripts+1)*10000 {
		t.Fatalf("budget growth: %d, %d", a, b)
	}
}
func TestWrapperViaPins(t *testing.T) {
	calls, err := Find("sudo nice bash <<E\nkill -1\nE\n", "kill")
	if err != nil || len(calls) != 1 {
		t.Fatalf("calls %v err %v", calls, err)
	}
	if !reflect.DeepEqual(calls[0].Via, []string{"sudo", "nice", "bash"}) {
		t.Fatalf("via %v", calls[0].Via)
	}
}

func TestFlagTablesPinned(t *testing.T) {
	for _, tc := range []struct{ name, flag string }{
		{"sudo", "-n"}, {"sudo", "-A"}, {"timeout", "-v"}, {"strace", "-f"}, {"unshare", "-r"},
		{"setpriv", "--init-groups"}, {"watch", "-d"}, {"parallel", "-k"}, {"flock", "-x"},
		{"systemd-run", "-q"}, {"doas", "-n"}, {"ionice", "-t"}, {"xargs", "-i"},
		{"script", "-q"}, {"su", "-l"}, {"runuser", "-l"},
	} {
		t.Run(tc.name+tc.flag, func(t *testing.T) {
			if !wrapperOptionsKnown(tc.name, []string{tc.flag, "kill"}) {
				t.Errorf("%s %s rejected", tc.name, tc.flag)
			}
		})
	}
}

func TestWrapperWithoutStdinAvoidsResolvingInput(t *testing.T) {
	src := strings.Repeat("nice ", 2000) + "kill -1"
	file, err := syntax.NewParser().Parse(strings.NewReader(src), "")
	if err != nil {
		t.Fatal(err)
	}
	allocs := testing.AllocsPerRun(1, func() { _, _, _, _ = shellInputName(file.Stmts[0], src, nil, nil) })
	if allocs > 10 {
		t.Fatalf("input resolution with no stdin used %.0f allocations", allocs)
	}
}

func TestScriptInvocationWorkBound(t *testing.T) {
	for _, prefix := range []string{"eval ", "watch ", "env -S "} {
		t.Run(prefix, func(t *testing.T) {
			src := strings.Repeat(prefix, 2000) + "kill -1"
			budget := newNestedParseBudget(len(src))
			budget.trackWork = true
			var calls []Call
			err := parseWithBudget(src, 0, nil, map[string]string{}, &calls, budget)
			if err == nil && callsNamed(calls, "kill") == 0 {
				t.Fatal("kill missed without an error")
			}
			if budget.parsedBytes > 6*len(src) {
				t.Fatalf("parsed %d bytes for input of %d bytes", budget.parsedBytes, len(src))
			}
		})
	}
}

func TestWrapperExecScanGrowth(t *testing.T) {
	for _, route := range []string{"heredoc", "pipeline", "process-substitution", "without-stdin"} {
		t.Run(route, func(t *testing.T) {
			steps := func(n int) int {
				src := strings.Repeat("nice ", n) + "bash -s"
				if route == "process-substitution" {
					src = strings.Repeat("nice ", n) + "bash <(x)"
				}
				file, err := syntax.NewParser().Parse(strings.NewReader(src), "")
				if err != nil {
					t.Fatal(err)
				}
				work := 0
				switch route {
				case "without-stdin":
					if _, ok := execTargetWithWork(strings.Fields(src), &work); ok {
						t.Fatal("unexpected exec target")
					}
				case "heredoc":
					args := append(strings.Fields(strings.Repeat("nice ", n-1)), "bash", "-s")
					name, _, err := resolveShellInputWithWork("nice", args, true, &work)
					if err != nil || name != "bash" {
						t.Fatalf("name=%q err=%v", name, err)
					}
				case "pipeline":
					if _, _, ok := pipedShellNameWithWork(file.Stmts[0], src, nil, &work); !ok {
						t.Fatal("pipeline shell missed")
					}
				case "process-substitution":
					call := file.Stmts[0].Cmd.(*syntax.CallExpr)
					budget := newNestedParseBudget(len(src))
					info := collectWalkInfo(file, src, nil, budget)
					index := info.processSubstitutions
					if index == nil {
						t.Fatal("process-substitution index was not built")
					}
					if !shellInputTargetWithIndexAndWork(call, src, nil, index, &work) {
						t.Fatal("shell input missed")
					}
				}
				return work
			}
			a, b := steps(500), steps(2000)
			t.Logf("exec argument steps: 500=%d 2000=%d", a, b)
			if a == 0 || b > 5*a {
				t.Fatalf("fourfold input grew from %d to %d steps", a, b)
			}
		})
	}
}

func TestWrapperExecScanMatchesExecTarget(t *testing.T) {
	words := []string{"exec", "--", "nice", "kill", "-x"}
	args := make([]string, 5)
	var visit func(int)
	visit = func(i int) {
		if i < len(args) {
			for _, word := range words {
				args[i] = word
				visit(i + 1)
			}
			return
		}
		var scan wrapperExecScan
		for j := range args {
			got, ok := scan.find(args[j:])
			want, wantOK := execTarget(args[j:])
			if ok != wantOK || !reflect.DeepEqual(got, want) {
				t.Fatalf("args=%q suffix=%d got=%q,%v want=%q,%v", args, j, got, ok, want, wantOK)
			}
		}
	}
	visit(0)
}

func TestLongWrapperChainsWithInput(t *testing.T) {
	for _, prefix := range []string{"", "sudo -u x "} {
		chain := prefix + strings.Repeat("nice ", 1000) + "kill -9 $PPID"
		for _, src := range []string{
			chain + " < file", chain + " <<EOF\necho data\nEOF\n", chain + " <<< data", "printf data | " + chain,
		} {
			calls, err := Find(src, "kill")
			if err != nil || len(calls) != 1 {
				t.Fatalf("input %q: kills=%d err=%v", src[:min(len(src), 60)], len(calls), err)
			}
		}
	}
}

func TestUnwrapMainRejectsDeepChainWithoutAllocating(t *testing.T) {
	args := strings.Fields(strings.Repeat("nice ", maxNestedScripts+1) + "kill")
	allocs := testing.AllocsPerRun(10, func() { _, _, _ = unwrapMain("nice", args) })
	if allocs != 0 {
		t.Fatalf("deep rejected chain: %.0f allocations", allocs)
	}
}

func TestUnwrapNonWrapperWithoutAllocating(t *testing.T) {
	for _, tc := range []struct {
		name   string
		unwrap func(string, []string) ([]string, []string, bool)
	}{
		{"current", unwrap},
		{"compatible", unwrapMain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"one", "two"}
			if _, _, ok := tc.unwrap("echo", args); ok {
				t.Fatal("ordinary command treated as wrapper")
			}
			if allocs := testing.AllocsPerRun(10, func() { _, _, _ = tc.unwrap("echo", args) }); allocs != 0 {
				t.Fatalf("ordinary command: %.0f allocations", allocs)
			}
		})
	}
}
