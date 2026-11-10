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

Stop a descendant of the calling process's parent or its consecutive shell
ancestors (bash, sh, dash, zsh, ksh). This permits direct, $(...) and bash -c
calls. A service manager is never an owner. This rule is not an isolation
boundary between jobs of the same shell. The request signal goes to every
observed process, including those discovered while waiting. The result reports
whether SIGKILL was needed and how many processes received it.
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
		fail(err)
	}
	if result.AlreadyExited {
		fmt.Printf("stop: %d already exited\n", pid)
	} else if result.Killed > 0 {
		fmt.Printf("stop: %d killed %d processes after %s grace\n", pid, result.Killed, *grace)
	} else {
		fmt.Printf("stop: %d exited after %s in %s\n", pid, unix.SignalName(sig), time.Since(started).Round(time.Millisecond))
	}
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
