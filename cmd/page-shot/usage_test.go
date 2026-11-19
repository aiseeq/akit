package main

import (
	"bytes"
	"flag"
	"strings"
	"testing"
)

func usageText() string {
	var b bytes.Buffer
	fs := flag.NewFlagSet("page-shot", flag.ContinueOnError)
	fs.SetOutput(&b)
	printUsage(fs)
	return b.String()
}

func TestUsageListsExactlyTheShotWindowsInOrder(t *testing.T) {
	var line string
	for l := range strings.SplitSeq(usageText(), "\n") {
		if strings.HasPrefix(l, "Окна ") {
			line = l
		}
	}
	_, list, ok := strings.Cut(line, "): ")
	if ok {
		list, _, ok = strings.Cut(list, ";")
	}
	if !ok {
		t.Fatalf("usage has no window list line: %q", usageText())
	}
	listed := strings.Split(list, ", ")
	if len(listed) != len(viewports) {
		t.Fatalf("usage lists %d windows %q, code shoots %d", len(listed), listed, len(viewports))
	}
	for i, vp := range viewports {
		if listed[i] != vp.name() {
			t.Errorf("window %d: usage says %q, code shoots %q", i, listed[i], vp.name())
		}
	}
}

func TestUsageNamesTheElementWindow(t *testing.T) {
	want := "--element: только окно " + elementViewport.name() + ","
	if !strings.Contains(usageText(), want) {
		t.Fatalf("usage lacks %q: %q", want, usageText())
	}
}

func TestUsageDescribesFontLayoutExit(t *testing.T) {
	if !strings.Contains(usageText(), "неподходящий шрифт") {
		t.Fatalf("usage does not mention font findings in exit code 2: %q", usageText())
	}
}
