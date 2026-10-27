package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// elementViewport - окно снимков элементов: типичный ноутбук (см. viewports).
var elementViewport = viewports[1]

// element - найденный по селектору элемент: id и бокс в CSS px от начала документа.
type element struct {
	ID     string  `json:"id"`
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// elementsJS находит элементы по селектору (подставляется JSON-строкой) и меряет их боксы от начала документа.
const elementsJS = `[...document.querySelectorAll(%s)].map(el => {
  const r = el.getBoundingClientRect();
  return {id: el.id, x: r.left + scrollX, y: r.top + scrollY, width: r.width, height: r.height};
})`

// safeID - id, годный в имя файла как есть.
var safeID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// elementNames - имена png по порядку элементов: <id>.png, у элемента без id, с id не для имени файла или
// с повтором id - el-<n>.png, n - номер элемента среди найденных с 1.
func elementNames(els []element) []string {
	names := make([]string, len(els))
	used := map[string]bool{}
	for i, el := range els {
		names[i] = "el-" + strconv.Itoa(i+1) + ".png"
		if safeID.MatchString(el.ID) && !used[el.ID] {
			names[i] = el.ID + ".png"
		}
		used[el.ID] = true
	}
	return names
}

// clip - бокс элемента целыми CSS px наружу: снимок не срезает край дробного бокса.
func (el element) clip() *page.Viewport {
	x, y := math.Floor(el.X), math.Floor(el.Y)
	return &page.Viewport{X: x, Y: y, Width: math.Ceil(el.X+el.Width) - x, Height: math.Ceil(el.Y+el.Height) - y, Scale: 1}
}

// screenshot снимает бокс элемента PNG, часть ниже окна тоже.
func (el element) screenshot() chromedp.Action[[]byte] {
	return func(ctx context.Context, t *chromedp.Target) ([]byte, error) {
		yes := true
		r, err := cdp.Call(ctx, t, page.CaptureScreenshot, page.CaptureScreenshotParams{
			Clip: el.clip(), CaptureBeyondViewport: &yes, FromSurface: &yes,
		})
		if err != nil {
			return nil, err
		}
		return r.Data, nil
	}
}

// shootElements снимает каждый элемент по селектору целиком, даже выше окна, на одном окне в последний момент
// съёмки и пишет <id>.png в dir. Элемент нулевого размера (скрыт) пропускается с пометкой в stderr.
func shootElements(allocCtx context.Context, target, dir, selector string, moment time.Duration) error {
	tabCtx, cancel := chromedp.NewContext(allocCtx)
	defer cancel()
	vp := elementViewport
	emulate := chromedp.EmulateViewport(vp.width, vp.height, chromedp.EmulateScale(vp.scale))
	if err := chromedp.Do(tabCtx, emulate, chromedp.Navigate(target), chromedp.Sleep(moment)); err != nil {
		return fmt.Errorf("открыть страницу: %w", err)
	}
	js, err := json.Marshal(selector)
	if err != nil {
		return fmt.Errorf("--element: %w", err)
	}
	quoted := string(js)
	els, err := chromedp.Run(tabCtx, chromedp.Evaluate[[]element](fmt.Sprintf(elementsJS, quoted)))
	if err != nil {
		return fmt.Errorf("--element %s: %w", quoted, err)
	}
	if len(els) == 0 {
		return fmt.Errorf("--element %s: на странице нет таких элементов", quoted)
	}
	shot := 0
	for i, name := range elementNames(els) {
		if els[i].Width == 0 || els[i].Height == 0 {
			fmt.Fprintf(os.Stderr, "page-shot: %s: элемент нулевого размера, не снят\n", name)
			continue
		}
		buf, err := chromedp.Run(tabCtx, els[i].screenshot())
		if err != nil {
			return fmt.Errorf("снимок %s: %w", name, err)
		}
		if err := save(filepath.Join(dir, name), buf); err != nil {
			return err
		}
		shot++
	}
	if shot == 0 {
		return errors.New("--element: все найденные элементы нулевого размера")
	}
	return nil
}
