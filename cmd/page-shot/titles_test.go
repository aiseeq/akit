package main

import (
	"context"
	"strings"
	"testing"
)

func TestCheckTitles(t *testing.T) {
	browserCtx := newLayoutTestBrowser(t)
	many := strings.Repeat(`<span title="tip">x</span>`, 7)
	testLayoutCases(t, browserCtx, []layoutTestCase{
		{name: "none", html: `<!doctype html><title>Page</title><body><p>text</p><svg><rect/></svg></body>`},
		{name: "attribute", html: `<!doctype html><body><abbr id="a" title="  full name ">FN</abbr></body>`, pairs: 1, contains: "abbr#a «full name»"},
		{name: "svg", html: `<!doctype html><body><svg><rect class="bar"><title>42 runs</title></rect></svg></body>`, pairs: 1, contains: "rect.bar > title «42 runs»"},
		{name: "empty title ignored", html: `<!doctype html><body><span title="">x</span></body>`},
		{name: "first five and the rest", html: `<!doctype html><body>` + many + `</body>`, pairs: 7, contains: "...и ещё 2"},
	}, func(ctx context.Context) (int, string, error) {
		tips, err := checkTitles(ctx)
		if err != nil || tips == nil {
			return 0, "", err
		}
		return tips.Count, tips.report(), nil
	}, "подсказок title")
}
