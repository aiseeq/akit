package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/chromedp/chromedp"
)

func TestFinishThemeInspectionsOrder(t *testing.T) {
	var calls []string
	fontsCheck := func(context.Context) (*fontResult, error) {
		calls = append(calls, "fonts")
		return &fontResult{}, nil
	}
	titlesCheck := func(context.Context) (*titleTips, error) {
		calls = append(calls, "titles")
		return &titleTips{}, nil
	}
	screenshot := func(context.Context) ([]byte, error) {
		calls = append(calls, "screenshot")
		return []byte("frame"), nil
	}

	fonts, titles, frame, err := finishThemeInspections(context.Background(), "light", fontsCheck, titlesCheck, screenshot)
	if err != nil {
		t.Fatal(err)
	}
	if fonts == nil || titles == nil || string(frame) != "frame" {
		t.Fatalf("unexpected inspection results: fonts=%v titles=%v frame=%q", fonts, titles, frame)
	}
	if want := []string{"fonts", "titles", "screenshot"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("inspection order %v, want %v", calls, want)
	}
}

func TestFinishThemeChecksFontsBeforeFullScreenshot(t *testing.T) {
	browserCtx := newLayoutTestBrowser(t)
	page := `<!doctype html><html><head><style>
body{font-family:"Golos Text",sans-serif}#late{display:none;font-family:Georgia}
</style><script>window.Gazette={};window.addEventListener('resize',()=>{window.__resized=true;document.querySelector('#late').style.display='block'})</script></head><body><p id=late>revealed on resize</p></body></html>`
	path := filepath.Join(t.TempDir(), "finish-theme-order.html")
	if err := os.WriteFile(path, []byte(page), 0o644); err != nil {
		t.Fatal(err)
	}
	tabCtx, cancelTab := chromedp.NewContext(browserCtx)
	defer cancelTab()
	if err := chromedp.Do(tabCtx, chromedp.EmulateViewport(800, 600), chromedp.Navigate("file://"+path)); err != nil {
		t.Fatal(err)
	}
	lay, _, err := finishTheme(tabCtx, t.TempDir(), viewport{width: 800, height: 600, scale: 1}, "light")
	if err != nil {
		t.Fatal(err)
	}
	resized, err := chromedp.Run(tabCtx, chromedp.Evaluate[bool](`window.__resized === true`))
	if err != nil {
		t.Fatal(err)
	}
	if !resized {
		t.Fatal("FullScreenshot did not trigger the fixture's resize handler")
	}
	if lay.fonts != nil && len(lay.fonts.Violations) != 0 {
		t.Fatalf("finishTheme checked fonts after the resize handler: %#v", lay.fonts)
	}
}
