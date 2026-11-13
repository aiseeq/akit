//go:build linux

package proc

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"testing"
	"time"
)

const exitSignalObserved = "unexpected signal delivered"

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
	if os.Getenv("PROC_EXIT_OBSERVE") == "1" {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, sig)
		send := sendExitSignal
		sendExitSignal = func(sig syscall.Signal) error {
			err := send(sig)
			if err == nil {
				select {
				case <-ch:
					fmt.Fprintln(os.Stderr, exitSignalObserved)
				case <-time.After(time.Second):
				}
			}
			return err
		}
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
		blocked    bool
	}{
		{"USR1-inherited-block", syscall.SIGUSR1, true, false, false, false, true},
		{"HUP", syscall.SIGHUP, true, false, false, false, false},
		{"INT", syscall.SIGINT, true, false, false, false, false},
		{"TERM", syscall.SIGTERM, true, false, false, false, false},
		{"KILL", syscall.SIGKILL, true, false, false, false, false},
		{"TERM-notify", syscall.SIGTERM, true, true, false, false, false},
		{"HUP-inherited-ignore", syscall.SIGHUP, false, false, true, false, false},
		{"TERM-undelivered", syscall.SIGTERM, false, false, false, true, false},
		{"STOP", syscall.SIGSTOP, false, false, false, false, false},
		{"TSTP", syscall.SIGTSTP, false, false, false, false, false},
		{"TTIN", syscall.SIGTTIN, false, false, false, false, false},
		{"TTOU", syscall.SIGTTOU, false, false, false, false, false},
		{"CHLD", syscall.SIGCHLD, false, false, false, false, false},
		{"URG", syscall.SIGURG, false, false, false, false, false},
		{"WINCH", syscall.SIGWINCH, false, false, false, false, false},
		{"CONT", syscall.SIGCONT, false, false, false, false, false},
		{"QUIT", syscall.SIGQUIT, true, false, false, false, false},
		{"ABRT", syscall.SIGABRT, true, false, false, false, false},
		{"SEGV", syscall.SIGSEGV, true, false, false, false, false},
		{"PIPE", syscall.SIGPIPE, true, false, false, false, false},
		{"USR1", syscall.SIGUSR1, true, false, false, false, false},
		{"ALRM", syscall.SIGALRM, true, false, false, false, false},
		{"USR2-notify", syscall.SIGUSR2, true, true, false, false, false},
		{"RTMIN+2", 36, true, false, false, false, false},
		{"ABRT-undelivered", syscall.SIGABRT, false, false, false, true, false},
		{"zero", 0, false, false, false, false, false},
		{"negative", -1, false, false, false, false, false},
		{"too-large", 65, false, false, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			args := []string{"-test.run=^TestExitWithSignalHelper$"}
			var cmd *exec.Cmd
			if tc.blocked {
				python, err := exec.LookPath("python3")
				if err != nil {
					t.Skip("python3 not installed: needed to start the child with a blocked signal")
				}
				// The blocked mask survives exec, and Go keeps it on its threads.
				script := fmt.Sprintf(`import os, signal, sys; signal.pthread_sigmask(signal.SIG_BLOCK, [%d]); os.execv(sys.argv[1], sys.argv[1:])`, tc.sig)
				cmd = exec.CommandContext(ctx, python, "-I", "-c", script, os.Args[0], args[0])
			} else if tc.ignore {
				cmd = exec.CommandContext(ctx, "sh", "-c", `trap '' HUP; exec "$@"`, "sh", os.Args[0], args[0])
			} else {
				cmd = exec.CommandContext(ctx, os.Args[0], args...)
			}
			cmd.Env = append(os.Environ(), fmt.Sprintf("PROC_EXIT_SIGNAL=%d", tc.sig))
			if tc.notify {
				cmd.Env = append(cmd.Env, "PROC_EXIT_NOTIFY=1")
			}
			if tc.noDelivery {
				cmd.Env = append(cmd.Env, "PROC_EXIT_NO_DELIVERY=1")
			}
			switch tc.sig {
			case syscall.SIGCHLD, syscall.SIGURG, syscall.SIGWINCH, syscall.SIGCONT:
				cmd.Env = append(cmd.Env, "PROC_EXIT_OBSERVE=1")
			}
			output, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("child timed out: %s", output)
			}
			if bytes.Contains(output, []byte(exitSignalObserved)) {
				t.Fatalf("unexpected signal delivery: %s", output)
			}
			if _, ok := err.(*exec.ExitError); !ok {
				t.Fatalf("expected process exit, got %v: %s", err, output)
			}
			status := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if tc.signaled {
				if !status.Signaled() || status.Signal() != tc.sig {
					t.Fatalf("status = %v, want signal %v: %s", status, tc.sig, output)
				}
				if status.CoreDump() {
					t.Fatalf("status = %v: core dumped, want the process non-dumpable", status)
				}
				if bytes.Contains(output, []byte("goroutine ")) {
					t.Fatalf("Go runtime handled %v: %s", tc.sig, output)
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
