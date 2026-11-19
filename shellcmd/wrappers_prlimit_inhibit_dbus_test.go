package shellcmd

import (
	"errors"
	"slices"
	"testing"
)

func TestPrlimitInhibitDbusWrappersExposeProgram(t *testing.T) {
	tests := []struct {
		src  string
		name string
		via  []string
	}{
		{`prlimit --nofile=1024 systemd-run --user pkill -u aisee`, "pkill", []string{"prlimit", "systemd-run"}},
		{`prlimit -n1024 kill 1`, "kill", []string{"prlimit"}},
		{`prlimit -o RESOURCE kill 1`, "kill", []string{"prlimit"}},
		{`prlimit --output RESOURCE kill 1`, "kill", []string{"prlimit"}},
		{`systemd-inhibit --why x --mode=block kill 1`, "kill", []string{"systemd-inhibit"}},
		{`dbus-run-session --config-file /tmp/session.conf -- systemd-run --user kill 1`, "kill", []string{"dbus-run-session", "systemd-run"}},
		{`run0 -u root --unit x kill 1`, "kill", []string{"run0"}},
		{`pkexec --user root kill 1`, "kill", []string{"pkexec"}},
		{`chroot --userspec 1000:1000 / kill 1`, "kill", []string{"chroot"}},
		{`setarch x86_64 -R kill 1`, "kill", []string{"setarch"}},
		{`setarch i686 kill 1`, "kill", []string{"setarch"}},
		{`setarch athlon -R kill 1`, "kill", []string{"setarch"}},
		{`setarch -R kill 1`, "kill", []string{"setarch"}},
		{`nsenter -t 1 -m -p kill 1`, "kill", []string{"nsenter"}},
		{`nsenter -t 1 -m -S 0 kill 1`, "kill", []string{"nsenter"}},
		{`nsenter -t 1 -m/proc/1/ns/mnt kill 1`, "kill", []string{"nsenter"}},
		{`nsenter -t 1 -m -G 0 kill 1`, "kill", []string{"nsenter"}},
		{`nsenter -t 1 -m --setuid 0 kill 1`, "kill", []string{"nsenter"}},
		{`nsenter -t 1 -m --setgid 0 kill 1`, "kill", []string{"nsenter"}},
		{`nsenter -t 1 -m -W kill 1`, "kill", []string{"nsenter"}},
		{`nsenter -t 1 -m -wh kill 1`, "kill", []string{"nsenter"}},
		{`nsenter -t 1 -m -wV kill 1`, "kill", []string{"nsenter"}},
		{`nsenter -t 1 -rh -m kill 1`, "kill", []string{"nsenter"}},
		{`nsenter -t 1 -mh kill 1`, "kill", []string{"nsenter"}},
		{`nsenter --target=1 --mount=/proc/1/ns/mnt --wd=/ kill 1`, "kill", []string{"nsenter"}},
		{`nsenter -t 1 -N 3 kill 1`, "kill", []string{"nsenter"}},
		{`nsenter -t 1 --mou kill 1`, "kill", []string{"nsenter"}},
		{`prlimit --nof=10 kill 1`, "kill", []string{"prlimit"}},
		{`dbus-run-session -- systemd-run --user kill 1`, "kill", []string{"dbus-run-session", "systemd-run"}},
		{`pkexec --keep-cwd kill 1`, "kill", []string{"pkexec"}},
		{`linux32 systemd-run --user kill 1`, "kill", []string{"linux32", "systemd-run"}},
		{`linux64 kill 1`, "kill", []string{"linux64"}},
		{`x86_64 -R kill 1`, "kill", []string{"x86_64"}},
		{`i386 kill 1`, "kill", []string{"i386"}},
		{`uname26 kill 1`, "kill", []string{"uname26"}},
		{`ssh-agent -t 10 -a /tmp/s kill 1`, "kill", []string{"ssh-agent"}},
		{`ssh-agent -T -E sha256 -O no-restrict-websafe -P /usr/lib64/* kill 1`, "kill", []string{"ssh-agent"}},
		{`ssh-agent -- kill 1`, "kill", []string{"ssh-agent"}},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			calls, err := Calls(tc.src)
			if err != nil {
				t.Fatal(err)
			}
			for _, call := range calls {
				if call.Name != tc.name {
					continue
				}
				found := true
				for _, via := range tc.via {
					found = found && slices.Contains(call.Via, via)
				}
				if found {
					return
				}
			}
			t.Fatalf("Calls(%q) = %+v, want %s via %v", tc.src, calls, tc.name, tc.via)
		})
	}
}

func TestWrappersReadCommandsFromStdin(t *testing.T) {
	for _, src := range []string{
		`chroot / <<<'kill 1'`,
		`echo 'kill 1' | chroot /`,
		"chroot / bash <<'X'\nkill 1\nX",
		`setarch x86_64 <<<'kill 1'`,
		`nsenter -t 1 -m <<<'kill 1'`,
		`run0 <<<'kill 1'`,
		`pkexec <<<'kill 1'`,
		`linux64 <<<'kill 1'`,
		`echo 'kill 1' | linux64`,
		"linux32 bash <<'X'\nkill 1\nX",
	} {
		t.Run(src, func(t *testing.T) {
			calls, err := Calls(src)
			if err != nil {
				t.Fatal(err)
			}
			if !containsCallName(calls, "kill") {
				t.Fatalf("Calls(%q) = %+v, want kill from wrapper stdin", src, calls)
			}
		})
	}
}

func TestNewWrapperOptionAndRouteDetails(t *testing.T) {
	for _, name := range []string{"chroot", "ssh-agent", "linux32", "linux64", "x86_64", "i386", "uname26"} {
		if !wrappers[name] {
			t.Errorf("wrappers missing %q", name)
		}
	}
	for _, tc := range []struct {
		name, arg string
	}{
		{"prlimit", "--nof=10"},
		{"nsenter", "--mou"},
	} {
		if !wrapperOptionsKnown(tc.name, []string{tc.arg, "kill"}) {
			t.Errorf("%s option abbreviation %q not recognized", tc.name, tc.arg)
		}
	}
	if got := optionNext("nsenter", []string{"-S", "0", "kill"}, 0); got != 2 {
		t.Errorf("optionNext(nsenter, -S) = %d, want 2", got)
	}
	if got := optionNext("nsenter", []string{"-W", "kill"}, 0); got != 1 {
		t.Errorf("optionNext(nsenter, -W) = %d, want 1", got)
	}
	if shortGroupExitsWithoutCommand("nsenter", "-wh") || shortGroupExitsWithoutCommand("nsenter", "-wV") || shortGroupExitsWithoutCommand("nsenter", "-rh") || shortGroupExitsWithoutCommand("nsenter", "-mh") {
		t.Fatal("short flags following optional-value option must be consumed as its value")
	}
	if target, via, ok := unwrapMain("dbus-run-session", []string{"--", "kill", "1"}); !ok || target[0] != "kill" || len(via) != 1 || via[0] != "dbus-run-session" {
		t.Fatalf("unwrapMain dbus-run-session = %v, %v, %v", target, via, ok)
	}
}

func TestSSHAgentModesDoNotRunCommand(t *testing.T) {
	for _, mode := range []string{"-c", "-s", "-D", "-d", "-k"} {
		src := "ssh-agent " + mode + " kill 1"
		calls, err := Calls(src)
		if err != nil {
			t.Fatal(err)
		}
		if containsCallName(calls, "kill") {
			t.Errorf("Calls(%q) = %+v, ssh-agent mode must not run a child", src, calls)
		}
	}
}

func TestWrappersWithoutCommandDoNotRunCommand(t *testing.T) {
	for _, src := range []string{`prlimit --pid 1 kill 1`, `prlimit -p 1 kill 1`, `systemd-inhibit --list kill 1`, `setarch --list kill 1`, `setarch --show=linux32 kill 1`} {
		calls, err := Calls(src)
		if err != nil {
			t.Fatal(err)
		}
		if containsCallName(calls, "kill") {
			t.Fatalf("Calls(%q) = %+v, --pid mode must not unwrap a command", src, calls)
		}
	}
}

func TestNewWrappersRejectUnknownOptions(t *testing.T) {
	for _, src := range []string{
		`prlimit --zzz kill 1`,
		`systemd-inhibit --zzz kill 1`,
		`run0 --zzz kill 1`,
		`pkexec --zzz kill 1`,
		`nsenter --zzz -t 1 -m kill 1`,
	} {
		_, err := Calls(src)
		if !errors.Is(err, errUnknownWrapperSignal) {
			t.Errorf("Calls(%q) error = %v, want errUnknownWrapperSignal", src, err)
		}
	}
}
