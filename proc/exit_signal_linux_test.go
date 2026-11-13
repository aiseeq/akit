//go:build linux

package proc

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"testing"
)

func TestExitWithSignalHelper(t *testing.T) {
	value := os.Getenv("PROC_EXIT_SIGNAL")
	if value == "" {
		return
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		t.Fatal(err)
	}
	sig := syscall.Signal(n)
	if os.Getenv("PROC_EXIT_NOTIFY") == "1" {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, sig)
	}
	if os.Getenv("PROC_EXIT_NO_DELIVERY") == "1" {
		sendExitSignal = func(syscall.Signal) error { return nil }
	}
	ExitWithSignal(sig)
	t.Fatal("ExitWithSignal returned")
}

func TestExitWithSignal(t *testing.T) {
	cases := []struct {
		name       string
		sig        syscall.Signal
		signaled   bool
		notify     bool
		ignore     bool
		noDelivery bool
	}{
		{"HUP", syscall.SIGHUP, true, false, false, false},
		{"INT", syscall.SIGINT, true, false, false, false},
		{"TERM", syscall.SIGTERM, true, false, false, false},
		{"KILL", syscall.SIGKILL, true, false, false, false},
		{"TERM-notify", syscall.SIGTERM, true, true, false, false},
		{"HUP-inherited-ignore", syscall.SIGHUP, false, false, true, false},
		{"TERM-undelivered", syscall.SIGTERM, false, false, false, true},
		{"STOP", syscall.SIGSTOP, false, false, false, false},
		{"TSTP", syscall.SIGTSTP, false, false, false, false},
		{"TTIN", syscall.SIGTTIN, false, false, false, false},
		{"TTOU", syscall.SIGTTOU, false, false, false, false},
		{"CHLD", syscall.SIGCHLD, false, false, false, false},
		{"URG", syscall.SIGURG, false, false, false, false},
		{"WINCH", syscall.SIGWINCH, false, false, false, false},
		{"CONT", syscall.SIGCONT, false, false, false, false},
		{"QUIT", syscall.SIGQUIT, false, false, false, false},
		{"ABRT", syscall.SIGABRT, false, false, false, false},
		{"SEGV", syscall.SIGSEGV, false, false, false, false},
		{"PIPE", syscall.SIGPIPE, false, false, false, false},
		{"USR1", syscall.SIGUSR1, false, false, false, false},
		{"ALRM", syscall.SIGALRM, false, false, false, false},
		{"zero", 0, false, false, false, false},
		{"negative", -1, false, false, false, false},
		{"too-large", 65, false, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"-test.run=^TestExitWithSignalHelper$"}
			var cmd *exec.Cmd
			if tc.ignore {
				cmd = exec.Command("sh", "-c", `trap '' HUP; exec "$@"`, "sh", os.Args[0], args[0])
			} else {
				cmd = exec.Command(os.Args[0], args...)
			}
			cmd.Env = append(os.Environ(), fmt.Sprintf("PROC_EXIT_SIGNAL=%d", tc.sig))
			if tc.notify {
				cmd.Env = append(cmd.Env, "PROC_EXIT_NOTIFY=1")
			}
			if tc.noDelivery {
				cmd.Env = append(cmd.Env, "PROC_EXIT_NO_DELIVERY=1")
			}
			output, err := cmd.CombinedOutput()
			if _, ok := err.(*exec.ExitError); !ok {
				t.Fatalf("expected process exit, got %v: %s", err, output)
			}
			status := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if tc.signaled {
				if !status.Signaled() || status.Signal() != tc.sig {
					t.Fatalf("status = %v, want signal %v: %s", status, tc.sig, output)
				}
			} else {
				want := 1
				if tc.sig > 0 && tc.sig <= 64 {
					want = 128 + int(tc.sig)
				}
				if status.Signaled() || cmd.ProcessState.ExitCode() != want {
					t.Fatalf("status = %v, want exit %d: %s", status, want, output)
				}
			}
		})
	}
}
