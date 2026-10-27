package main

import (
	"context"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

func TestElementNames(t *testing.T) {
	got := elementNames([]element{{ID: "n-swap"}, {}, {ID: "n-swap"}, {ID: "../x"}, {ID: "kira.idea_2"}})
	want := []string{"n-swap.png", "el-2.png", "el-3.png", "el-4.png", "kira.idea_2.png"}
	if !slices.Equal(got, want) {
		t.Errorf("имена: %q, ждали %q", got, want)
	}
}

// Карточки: вторая выше окна 860 и ниже первого экрана, у третьей нет id, скрытая не снимается.
const cards = `<!doctype html><body style="margin:0;font:16px sans-serif">
<div style="height:1200px">шапка</div>
<div id="list">
<article class="note" id="first" style="width:300px;height:100px">первая</article>
<article class="note" id="tall" style="width:300.4px;height:1500px">высокая</article>
<article class="note" style="width:200px;height:50px">без id</article>
<article class="note" id="hidden" style="display:none">скрытая</article>
<article class="item" id="other" style="height:40px">не та</article>
</div></body>`

func TestShootElements(t *testing.T) {
	if _, err := exec.LookPath("google-chrome"); err != nil {
		if _, err := exec.LookPath("chromium-browser"); err != nil {
			t.Skip("нет Chrome для headless-съёмки")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, chromedp.DefaultExecAllocatorOptions[:]...)
	defer cancelAlloc()
	dir := t.TempDir()
	page := filepath.Join(dir, "cards.html")
	if err := os.WriteFile(page, []byte(cards), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	if err := os.Mkdir(out, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := shootElements(allocCtx, "file://"+page, out, "#list article.note", 0); err != nil {
		t.Fatal(err)
	}
	// Размер png - бокс в CSS px, округлённый наружу, на масштаб 1.25.
	for name, size := range map[string][2]int{"first.png": {375, 125}, "tall.png": {376, 1875}, "el-3.png": {250, 63}} {
		f, err := os.Open(filepath.Join(out, name))
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := png.DecodeConfig(f)
		_ = f.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got := [2]int{cfg.Width, cfg.Height}; got != size {
			t.Errorf("%s: %v, ждали %v", name, got, size)
		}
	}
	if _, err := os.Stat(filepath.Join(out, "hidden.png")); err == nil {
		t.Error("скрытый элемент снят")
	}
	err := shootElements(allocCtx, "file://"+page, out, "article.missing", 0)
	if err == nil || !strings.Contains(err.Error(), "нет таких элементов") {
		t.Errorf("пустой селектор: %v", err)
	}
}
