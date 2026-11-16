package shellcmd

import "testing"

func TestReviewMutationFormsKeepKill(t *testing.T) {
	tests := []struct{ id, src, wrapper string }{
		{"1", "xargs --process-slot-var SLOT kill 1", "xargs"},
		{"3", "strace --summary-syscall-overhead 0 kill 1", "strace"},
		{"4", "strace --detach-on execve kill 1", "strace"},
		{"6", "strace --stack-trace-frame-limit 5 kill 1", "strace"},
		{"7", "strace --summary-sort-by calls kill 1", "strace"},
		{"8", "strace --interruptible 1 kill 1", "strace"},
		{"9", "strace --decode-pids comm kill 1", "strace"},
		{"12", "unshare --load-interp x kill 1", "unshare"},
		{"19", "echo 'kill 1' | env --block-signal bash", "env"},
		{"20", "echo 'kill 1' | env --default-signal bash", "env"},
		{"21", "echo 'kill 1' | env --ignore-signal bash", "env"},
		{"22", "echo 'kill 1' | env --list-signal-handling bash", "env"},
		{"23", "echo 'kill 1' | sudo -NA bash", "sudo"},
		{"24", "echo 'kill 1' | sudo --no-update bash", "sudo"},
		{"25", "echo 'kill 1' | timeout -f 5 bash", "timeout"},
		{"26", "echo 'kill 1' | timeout -pf 5 bash", "timeout"},
		{"34", "chrt -o --verbose 0 kill 1", "chrt"},
		{"35", "strace -Aa 10 kill 1", "strace"},
		{"36", "strace -Ca 10 kill 1", "strace"},
		{"39", "strace -ia 10 kill 1", "strace"},
		{"40", "strace -na 10 kill 1", "strace"},
		{"41", "strace -Na 10 kill 1", "strace"},
		{"42", "strace -wc kill 1", "strace"},
		{"47", "strace --always-show-pid kill 1", "strace"},
		{"48", "strace --summary kill 1", "strace"},
		{"50", "strace --instruction-pointer kill 1", "strace"},
		{"51", "strace --syscall-number kill 1", "strace"},
		{"52", "strace --arg-names kill 1", "strace"},
		{"53", "strace --relative-timestamps kill 1", "strace"},
		{"54", "strace --syscall-times kill 1", "strace"},
		{"55", "strace --no-abbrev kill 1", "strace"},
		{"56", "strace --strings-in-hex kill 1", "strace"},
		{"57", "strace --stack-trace kill 1", "strace"},
		{"62", "strace --tips kill 1", "strace"},
		{"68", "unshare -Tf kill 1", "unshare"},
		{"69", "unshare --mount-binfmt kill 1", "unshare"},
		{"70", "unshare --map-auto kill 1", "unshare"},
		{"71", "unshare --keep-caps kill 1", "unshare"},
		{"76", "systemd-run -Tr kill 1", "systemd-run"},
		{"77", "systemd-run --pty-late kill 1", "systemd-run"},
		{"79", "systemd-run --same-root-dir kill 1", "systemd-run"},
		{"80", "systemd-run --no-ask-password kill 1", "systemd-run"},
		{"81", "systemd-run --no-block kill 1", "systemd-run"},
		{"82", "systemd-run --no-pager kill 1", "systemd-run"},
		{"83", "systemd-run --ignore-failure kill 1", "systemd-run"},
		{"86", "systemd-run -vr kill 1", "systemd-run"},
		{"87", "systemd-run --verbose kill 1", "systemd-run"},
		{"95", "runuser -u root -Pg root kill 1", "runuser"},
		{"96", "runuser -u root -Tg root kill 1", "runuser"},
		{"97", "runuser -u root --no-pty kill 1", "runuser"},
		{"105", "systemd-run --b red kill 1", "systemd-run"},
		{"111", "echo 'kill 1' | nice --5 bash", "nice"},
		{"112", "echo 'kill 1' | nice -+5 bash", "nice"},
		{"113", "echo 'kill 1' | nice --5 bash", "nice"},
		{"122", "echo 'kill 1' | env -S bash", "env"},
		{"125", "env --split-string kill 1", "env"},
	}
	for _, tc := range tests {
		t.Run(tc.id, func(t *testing.T) {
			calls, err := Calls(tc.src)
			if err != nil {
				t.Fatalf("Calls: %v", err)
			}
			if !hasCallVia(calls, "kill", tc.wrapper) {
				t.Fatalf("calls %+v, want kill via %s", calls, tc.wrapper)
			}
		})
	}
}
