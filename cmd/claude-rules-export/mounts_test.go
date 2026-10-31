package main

import (
	"slices"
	"testing"
)

func TestUnescapeMount(t *testing.T) {
	for in, want := range map[string]string{`/a\040b`: "/a b", `/t\011x\012`: "/t\tx\n", `/s\134`: `/s\`, `/plain`: "/plain", `/bad\09`: `/bad\09`} {
		if got := unescapeMount(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// Подпапка файловой системы, показанная bind mount в другой точке, видна и под путём через корень той же системы.
func TestAliases(t *testing.T) {
	mounts := []mount{
		{dev: "0:1", root: "/", point: "/"},
		{dev: "0:2", root: "/home", point: "/home"},
		{dev: "0:2", root: "/home/u/.claude/hooks", point: "/mnt/h"},
		{dev: "0:3", root: "/", point: "/tmp"},
	}
	for _, c := range []struct {
		p    string
		want []string
	}{
		{"/mnt/h/x", []string{"/mnt/h/x", "/home/u/.claude/hooks/x"}},
		{"/home/u/.claude/hooks", []string{"/home/u/.claude/hooks", "/mnt/h"}},
		{"/home/u/work", []string{"/home/u/work"}},
		{"/tmp/o", []string{"/tmp/o"}},
	} {
		if got := aliases(c.p, mounts); !slices.Equal(got, c.want) {
			t.Errorf("%s: %q, want %q", c.p, got, c.want)
		}
	}
}
