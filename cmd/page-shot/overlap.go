package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/chromedp/chromedp"
)

// overlapPair - две подписи, чьи рамки текста пересеклись: одна закрывает другую.
type overlapPair struct {
	A     overlapText `json:"a"`
	B     overlapText `json:"b"`
	Width float64     `json:"w"`
	High  float64     `json:"h"`
	// Y - верх пересечения от начала документа, CSS px: где искать на кадре полной высоты.
	Y float64 `json:"y"`
}

// overlapText - текст подписи и элемент, в котором он стоит.
type overlapText struct {
	Text     string `json:"text"`
	Selector string `json:"selector"`
}

// overlapResult - все найденные пересечения на окне.
type overlapResult struct {
	Pairs []overlapPair `json:"pairs"`
}

// overlapMinPx - пересечение меньше этого по ширине или высоте не считается:
// соседние строки и подписи касаются краями на субпиксельном округлении.
const (
	overlapMinPx   = 2
	overlapMinPxJS = "2"
)

// overlapJS собирает рамки видимого текста и ищет пересекающиеся пары.
// HTML: каждый текстовый узел, по рамке строки (Range.getClientRects), рамка
// обрезана предками с overflow не visible. SVG: элемент text целиком (tspan
// внутри - его часть). Рамка строки выше кегля (ascent+descent): по вертикали
// она сжимается до кегля, иначе соседние строки с плотным line-height давали бы
// ложное пересечение. Пары внутри одного текстового узла или одного SVG text не
// сравниваются. Невидимое (visibility, opacity 0 у предка, нулевая рамка) - мимо.
const overlapJS = `(() => {
  const min = ` + overlapMinPxJS + `;
  ` + layoutCommonJS + `
  const items = [];
  const add = (owner, el, text, r, em) => {
    const box = textBox(el, r, em);
    if (!box) return;
    items.push({owner, el, text, ...box});
  };
  // checkVisibility: закрытый details (content-visibility: hidden), display none,
  // visibility и opacity 0; без него - свой обход предков.
  const walker = document.createTreeWalker(document.body || document.documentElement, NodeFilter.SHOW_TEXT);
  let owner = 0;
  for (let n = walker.nextNode(); n; n = walker.nextNode()) {
    const text = n.nodeValue.trim();
    const el = n.parentElement;
    if (!text || !el || (el.closest('svg') && !el.closest('foreignObject'))) continue;
    if (!visible(el)) continue;
    const em = parseFloat(getComputedStyle(el).fontSize) || 0;
    const range = document.createRange();
    range.selectNodeContents(n);
    owner++;
    for (const r of range.getClientRects()) add(owner, el, text, r, em);
  }
  for (const el of document.querySelectorAll('svg text')) {
    if (el.parentElement && el.parentElement.closest('text')) continue;
    const text = el.textContent.trim();
    if (!text || !visible(el)) continue;
    const m = el.getScreenCTM();
    const scale = m ? Math.hypot(m.a, m.b) : 1;
    const em = (parseFloat(getComputedStyle(el).fontSize) || 0) * scale;
    owner++;
    add(owner, el, text, el.getBoundingClientRect(), em);
  }
  items.sort((a, b) => a.t - b.t);
  const pairs = [];
  const seen = new Set();
  for (let i = 0; i < items.length; i++) {
    const a = items[i];
    for (let j = i + 1; j < items.length && items[j].t < a.b; j++) {
      const b = items[j];
      if (a.owner === b.owner) continue;
      const w = Math.min(a.r, b.r) - Math.max(a.l, b.l);
      const h = Math.min(a.b, b.b) - Math.max(a.t, b.t);
      if (w <= min || h <= min) continue;
      const key = Math.min(a.owner, b.owner) + ':' + Math.max(a.owner, b.owner);
      if (seen.has(key)) continue;
      seen.add(key);
      const t = s => s.length > 40 ? s.slice(0, 40) + '…' : s;
      pairs.push({a: {text: t(a.text), selector: describe(a.el)}, b: {text: t(b.text), selector: describe(b.el)},
                  w: Math.round(w * 10) / 10, h: Math.round(h * 10) / 10, y: Math.round(Math.max(a.t, b.t) + window.scrollY)});
    }
  }
  return {pairs};
})()`

// checkOverlap возвращает пересечения подписей или nil, если их нет.
func checkOverlap(ctx context.Context) (*overlapResult, error) {
	res, err := chromedp.Run(ctx, chromedp.Evaluate[overlapResult](overlapJS))
	if err != nil {
		return nil, fmt.Errorf("проверка наложения подписей: %w", err)
	}
	if len(res.Pairs) == 0 {
		return nil, nil
	}
	return &res, nil
}

// report - текст для stderr: окно, число пар, до maxCulprits пар с текстом.
func (o *overlapResult) report(name string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "page-shot: %s: наложение подписей, пар: %d (пересечение больше %d px)\n", name, len(o.Pairs), overlapMinPx)
	for i, p := range o.Pairs {
		if i == maxCulprits {
			fmt.Fprintf(&b, "  ...и ещё %d\n", len(o.Pairs)-maxCulprits)
			break
		}
		fmt.Fprintf(&b, "  %q (%s) и %q (%s): %gx%g px, y=%g\n", p.A.Text, p.A.Selector, p.B.Text, p.B.Selector, p.Width, p.High, p.Y)
	}
	return b.String()
}
