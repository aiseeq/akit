package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

var (
	sharedTestAllocator context.Context
	sharedTestBrowser   context.Context
	sharedTestCancel    context.CancelFunc
	sharedAllocCancel   context.CancelFunc
	sharedRootCancel    context.CancelFunc
	sharedChromeErr     error
	sharedTestOnce      sync.Once
)

type layoutTestCase struct {
	name, html string
	pairs      int
	contains   string
}

func TestMain(m *testing.M) {
	code := m.Run()
	if sharedTestCancel != nil {
		sharedTestCancel()
		sharedAllocCancel()
		sharedRootCancel()
	}
	os.Exit(code)
}

func startSharedTestBrowser() {
	chrome := ""
	for _, name := range []string{"google-chrome", "chromium-browser", "/usr/bin/google-chrome"} {
		if path, err := exec.LookPath(name); err == nil {
			chrome = path
			break
		}
	}
	if chrome == "" {
		sharedChromeErr = fmt.Errorf("нет Chrome для headless-съёмки")
	} else {
		base, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		sharedRootCancel = cancel
		sharedTestAllocator, sharedAllocCancel = chromedp.NewExecAllocator(base, append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(chrome), chromedp.NoSandbox)...)
		sharedTestBrowser, sharedTestCancel = chromedp.NewContext(sharedTestAllocator)
		if err := chromedp.Do(sharedTestBrowser); err != nil {
			sharedChromeErr = fmt.Errorf("запустить Chrome: %w", err)
			sharedTestCancel()
			sharedAllocCancel()
			sharedRootCancel()
			sharedTestBrowser, sharedTestAllocator = nil, nil
		}
	}
}

// All browser tests share one Chrome process; each scenario opens an isolated tab.
func newTestAllocator(t *testing.T) context.Context {
	t.Helper()
	sharedTestOnce.Do(startSharedTestBrowser)
	if sharedChromeErr != nil {
		t.Fatalf("Chrome test setup: %v", sharedChromeErr)
	}
	return sharedTestAllocator
}

func newLayoutTestBrowser(t *testing.T) context.Context {
	t.Helper()
	newTestAllocator(t)
	return sharedTestBrowser
}

func TestCheckGazette(t *testing.T) {
	browserCtx := newLayoutTestBrowser(t)
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
			tabCtx, cancelTab := chromedp.NewContext(browserCtx)
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

func testLayoutCases(t *testing.T, browserCtx context.Context, cases []layoutTestCase, check func(context.Context) (int, string, error), failure string) {
	t.Helper()
	dir := t.TempDir()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(dir, c.name+".html")
			if err := os.WriteFile(path, []byte(c.html), 0o644); err != nil {
				t.Fatal(err)
			}
			tabCtx, cancelTab := chromedp.NewContext(browserCtx)
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

func testSingleLayoutPage(t *testing.T, browserCtx context.Context, page string, check func(context.Context) (int, string, error)) (int, string, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "page.html")
	if err := os.WriteFile(path, []byte(page), 0o644); err != nil {
		t.Fatal(err)
	}
	tabCtx, cancelTab := chromedp.NewContext(browserCtx)
	defer cancelTab()
	if err := chromedp.Do(tabCtx, chromedp.EmulateViewport(800, 600), chromedp.Navigate("file://"+path)); err != nil {
		t.Fatal(err)
	}
	return check(tabCtx)
}
