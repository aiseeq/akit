// stop requests a process to exit, then stops its observed descendants.
package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/akit/proc"
	"golang.org/x/sys/unix"
)

const usage = `Usage: stop [--grace D] [--signal S] PID

Stop a descendant of stop itself or of the consecutive shells directly above
it (bash, sh, dash, zsh, ksh, mksh; told by the program they run, so scripts
count). This permits calls from a shell, a script, $(...) and bash -c. A
non-shell (a terminal, a multiplexer, an agent, any program) and a service
manager are never owners, even as the direct parent: stop does not reach jobs
of another terminal tab or another agent call. A program stopping its own
child uses proc.StopChild. Neither rule is a security boundary: jobs of one
shell can stop each other, and a shell is told by its program file name.
The request signal goes to the processes observed before it; processes found
later, such as a cleanup the target starts on the request, are not signalled
and get SIGKILL only if still alive after the grace. The result reports whether
SIGKILL was needed and how many processes received it. A failure for one
process does not stop the others; stop then exits 1 naming each failed PID.
PID must identify the original process when stop starts: a PID reused before
then cannot be distinguished. Descendants already reparented before discovery
or created and reparented between scans may escape; use a dedicated cgroup for
strict tree containment. pidfd support is required.

Grace defaults to 2s, accepts 0 through 24h; signal defaults to TERM.
A zombie is reported as already exited; a reaped PID is an error.
`

func main() {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(fs.Output(), usage) }
	grace := fs.Duration("grace", 2*time.Second, "grace period")
	sigName := fs.String("signal", "TERM", "signal name or number")
	if err := fs.Parse(os.Args[1:]); err == flag.ErrHelp {
		os.Exit(0)
	} else if err != nil {
		fail(err)
	}
	if fs.NArg() != 1 {
		fs.Usage()
		os.Exit(2)
	}
	pid, err := strconv.Atoi(fs.Arg(0))
	if err != nil {
		fail(err)
	}
	sig, err := parseSignal(*sigName)
	if err != nil {
		fail(err)
	}
	started := time.Now()
	result, err := proc.StopFromShell(pid, *grace, sig)
	if err != nil {
		if result.Killed > 0 {
			fmt.Println(killedLine(pid, result.Killed, *grace))
		}
		fail(err)
	}
	if result.AlreadyExited {
		fmt.Printf("stop: %d already exited\n", pid)
	} else if result.Killed > 0 {
		fmt.Println(killedLine(pid, result.Killed, *grace))
	} else {
		fmt.Printf("stop: %d exited after %s in %s\n", pid, unix.SignalName(sig), time.Since(started).Round(time.Millisecond))
	}
}

func killedLine(pid, killed int, grace time.Duration) string {
	noun := "processes"
	if killed == 1 {
		noun = "process"
	}
	return fmt.Sprintf("stop: %d killed %d %s after %s grace", pid, killed, noun, grace)
}

func fail(err error) { fmt.Fprintln(os.Stderr, "stop:", err); os.Exit(1) }

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
