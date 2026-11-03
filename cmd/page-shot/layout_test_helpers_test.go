package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

type layoutTestCase struct {
	name, html string
	pairs      int
	contains   string
}

func newLayoutTestBrowser(t *testing.T) context.Context {
	t.Helper()
	chrome := ""
	for _, name := range []string{"google-chrome", "chromium-browser", "/usr/bin/google-chrome"} {
		if path, err := exec.LookPath(name); err == nil {
			chrome = path
			break
		}
	}
	if chrome == "" {
		t.Skip("нет Chrome для headless-съёмки")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(chrome))...)
	t.Cleanup(cancelAlloc)
	return allocCtx
}

func TestCheckGazette(t *testing.T) {
	allocCtx := newLayoutTestBrowser(t)
	cases := []struct {
		name    string
		html    string
		missing bool
	}{
		{name: "missing", html: "<!doctype html><html><body></body></html>", missing: true},
		{name: "present", html: "<!doctype html><script>window.Gazette={}</script><body></body>", missing: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), c.name+".html")
			if err := os.WriteFile(path, []byte(c.html), 0o644); err != nil {
				t.Fatal(err)
			}
			tabCtx, cancelTab := chromedp.NewContext(allocCtx)
			defer cancelTab()
			if err := chromedp.Do(tabCtx, chromedp.EmulateViewport(800, 600), chromedp.Navigate("file://"+path)); err != nil {
				t.Fatal(err)
			}
			missing, err := checkGazette(tabCtx)
			if err != nil {
				t.Fatal(err)
			}
			if missing != c.missing {
				t.Fatalf("отсутствие Gazette: получили %t, ждали %t", missing, c.missing)
			}
		})
	}
}

func testLayoutCases(t *testing.T, allocCtx context.Context, cases []layoutTestCase, check func(context.Context) (int, string, error), failure string) {
	t.Helper()
	dir := t.TempDir()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(dir, c.name+".html")
			if err := os.WriteFile(path, []byte(c.html), 0o644); err != nil {
				t.Fatal(err)
			}
			tabCtx, cancelTab := chromedp.NewContext(allocCtx)
			defer cancelTab()
			if err := chromedp.Do(tabCtx, chromedp.EmulateViewport(800, 600), chromedp.Navigate("file://"+path)); err != nil {
				t.Fatal(err)
			}
			got, report, err := check(tabCtx)
			if err != nil {
				t.Fatal(err)
			}
			if report != "" {
				t.Log(report)
			}
			if c.contains != "" && !strings.Contains(report, c.contains) {
				t.Fatalf("отчёт не содержит %q: %s", c.contains, report)
			}
			if got != c.pairs {
				t.Fatalf("%s: %d пар, ждали %d", failure, got, c.pairs)
			}
		})
	}
}
