// pstop requests a process to exit, then stops its observed descendants.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/akit/proc"
	"golang.org/x/sys/unix"
)

const usage = `Usage: pstop [--own-unit] [--grace D] [--signal S] PID

Stop a descendant of pstop itself or of the consecutive shells directly above
it (bash, sh, dash, zsh, ksh, mksh; told by the program they run, so scripts
count). This permits calls from a shell, a script, $(...) and bash -c. A
non-shell (a terminal, a multiplexer, an agent, any program) and a service
manager are never owners, even as the direct parent: pstop does not reach jobs
of another terminal tab or another agent call. A program stopping its own
child uses proc.StopChild. Neither rule is a security boundary: jobs of one
shell can stop each other, and a shell is told by its program file name.
The request signal goes to the processes observed before it; processes found
later, such as a cleanup the target starts on the request, are not signalled
and get SIGKILL only if still alive after the grace. The result reports whether
SIGKILL was needed and how many processes received it. A failure for one
process does not stop the others; pstop then exits 1 naming each failed PID.
PID must identify the original process when pstop starts: a PID reused before
then cannot be distinguished. Descendants already reparented before discovery
or created and reparented between scans may escape; use a dedicated cgroup for
strict tree containment. pidfd support is required.

Grace defaults to 2s, accepts 0 through 24h; signal defaults to TERM.
A zombie is reported as already exited; a reaped PID is an error.

--own-unit is for a reparented background process in your own unit after its
parent process exited. Accepted cgroup leaves are agents.slice/agent-*.scope,
app.slice/run-*.scope, and worker-* scope/service under your user@<uid>.service.
It is a convenience guard, not a security boundary.
`

var stopFromShellAction = proc.StopFromShell
var stopOwnUnitAction = proc.StopOwnUnit

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pstop", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(fs.Output(), usage) }
	grace := fs.Duration("grace", 2*time.Second, "grace period")
	sigName := fs.String("signal", "TERM", "signal name or number")
	ownUnit := fs.Bool("own-unit", false, "stop a process in this caller's agent scope")
	if err := fs.Parse(args); err == flag.ErrHelp {
		return 0
	} else if err != nil {
		fmt.Fprintln(stderr, "pstop:", err)
		return 1
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	pid, err := strconv.Atoi(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(stderr, "pstop:", err)
		return 1
	}
	sig, err := parseSignal(*sigName)
	if err != nil {
		fmt.Fprintln(stderr, "pstop:", err)
		return 1
	}
	started := time.Now()
	var result proc.StopResult
	if *ownUnit {
		result, err = stopOwnUnitAction(pid, *grace, sig)
	} else {
		result, err = stopFromShellAction(pid, *grace, sig)
	}
	if result.Degraded {
		fmt.Fprintln(stderr, "pstop: degraded stop; descendants may remain")
	}
	if err != nil {
		if result.Killed > 0 {
			fmt.Fprintln(stdout, killedLine(pid, result.Killed, *grace))
		}
		fmt.Fprintln(stderr, "pstop:", err)
		return 1
	}
	if result.AlreadyExited {
		fmt.Fprintf(stdout, "pstop: %d already exited\n", pid)
	} else if result.Killed > 0 {
		fmt.Fprintln(stdout, killedLine(pid, result.Killed, *grace))
	} else {
		fmt.Fprintf(stdout, "pstop: %d exited after %s in %s\n", pid, unix.SignalName(sig), time.Since(started).Round(time.Millisecond))
	}
	return 0
}

func killedLine(pid, killed int, grace time.Duration) string {
	noun := "processes"
	if killed == 1 {
		noun = "process"
	}
	return fmt.Sprintf("pstop: %d killed %d %s after %s grace", pid, killed, noun, grace)
}

func parseSignal(s string) (unix.Signal, error) {
	s = strings.TrimPrefix(strings.ToUpper(s), "SIG")
	if n, err := strconv.Atoi(s); err == nil && n > 0 && n <= 64 {
		return unix.Signal(n), nil
	}
	for n := 1; n <= 64; n++ {
		sig := unix.Signal(n)
		if strings.TrimPrefix(unix.SignalName(sig), "SIG") == s {
			return sig, nil
		}
	}
	return 0, fmt.Errorf("invalid signal %q", s)
}
