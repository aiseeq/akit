package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/chromedp/chromedp"
)

type alignPair struct {
	IconText string  `json:"iconText"`
	Selector string  `json:"selector"`
	Neighbor string  `json:"neighbor"`
	Shift    float64 `json:"shift"`
	Y        float64 `json:"y"`
}

type alignResult struct {
	Pairs []alignPair `json:"pairs"`
}

const alignThreshold = 2

const alignJS = `(() => {
  ` + layoutCommonJS + `
  const symbols = s => {
    const clean = s.trim().replace(/[\uFE0E\uFE0F\u200D\u{1F3FB}-\u{1F3FF}]/gu, '');
    return clean.length > 0 && Array.from(clean).length <= 2 && !/[\p{L}\p{N}]/u.test(clean);
  };
  const className = el => typeof el.className === 'string' ? el.className : (el.className && el.className.baseVal) || '';
  const decorated = el => {
    const cs = getComputedStyle(el);
    const border = ['Top', 'Right', 'Bottom', 'Left'].some(side => parseFloat(cs['border' + side + 'Width']) > 0 && cs['border' + side + 'Style'] !== 'none');
    return border || cs.backgroundImage !== 'none' || (cs.backgroundColor !== 'rgba(0, 0, 0, 0)' && cs.backgroundColor !== 'transparent');
  };
  const isIcon = el => {
    const tag = el.tagName.toLowerCase();
    if (tag === 'svg' || tag === 'img') return true;
    if (/(^|[\s_-])(?:ic|icon)(?:$|[\s_-])/i.test(className(el))) return true;
    const text = el.textContent.trim();
    if (text && symbols(text)) return true;
    return !text && !el.children.length && decorated(el);
  };
  // lineOf — первая строка текста el по рамкам его фрагментов, сжатая по вертикали до кегля.
  const lineOf = (el, text, rects) => {
    rects = rects.filter(r => r.width > 0 && r.height > 0).sort((a, b) => a.top - b.top || a.left - b.left);
    if (!rects.length) return null;
    const top = rects[0].top;
    const line = rects.filter(r => Math.abs(r.top - top) <= 1);
    const rect = {left: Math.min(...line.map(r => r.left)), top,
                  right: Math.max(...line.map(r => r.right)), bottom: Math.max(...line.map(r => r.bottom))};
    const style = getComputedStyle(el);
    const em = parseFloat(style.fontSize) || 0;
    const lineHeight = style.lineHeight === 'normal' ? em * 1.2 : parseFloat(style.lineHeight);
    const box = em ? textBox(el, rect, em) : null;
    return box ? {el, text, em, lineHeight, box} : null;
  };
  const firstLine = el => {
    const text = el.textContent.trim().replace(/\s+/g, ' ');
    if (!text || symbols(text) || !visible(el)) return null;
    const runs = [];
    const walker = document.createTreeWalker(el, NodeFilter.SHOW_TEXT);
    for (let node = walker.nextNode(); node; node = walker.nextNode()) {
      if (!node.textContent.trim() || !visible(node.parentElement)) continue;
      const range = document.createRange(); range.selectNodeContents(node);
      const rects = Array.from(range.getClientRects()).filter(r => r.width > 0 && r.height > 0);
      if (!rects.length) continue;
      const runEl = node.parentElement;
      runs.push({el: runEl, rects, em: parseFloat(getComputedStyle(runEl).fontSize) || 0});
    }
    if (!runs.length) return null;
    const firstTop = Math.min(...runs.flatMap(run => run.rects.map(rect => rect.top)));
    const line = runs.map(run => ({...run, rects: run.rects.filter(rect => Math.abs(rect.top - firstTop) <= 1)}))
      .filter(run => run.rects.length);
    const em = Math.max(...line.map(run => run.em));
    const main = line.filter(run => Math.abs(run.em - em) < 0.1);
    return lineOf(main[0].el, text, main.flatMap(run => run.rects));
  };
  const directTextLine = el => {
    const nodes = Array.from(el.childNodes).filter(node => node.nodeType === Node.TEXT_NODE && node.textContent.trim());
    const text = nodes.map(node => node.textContent.trim()).join(' ').replace(/\s+/g, ' ');
    if (!text || symbols(text) || !visible(el)) return null;
    const rects = [];
    for (const node of nodes) {
      const range = document.createRange(); range.selectNodeContents(node);
      rects.push(...range.getClientRects());
    }
    return lineOf(el, text, rects);
  };
  const decoratedBlock = (el, em) => {
    const rect = el.getBoundingClientRect();
    return decorated(el) && (rect.width > em * 2 || rect.height > em * 2);
  };
  const visualBox = icon => {
    const media = icon.matches('svg, img') ? icon : icon.querySelector('svg, img');
    if (media) return clippedBox(media, media.getBoundingClientRect());
    const text = icon.textContent.trim();
    if (text && symbols(text)) {
      const range = document.createRange(); range.selectNodeContents(icon);
      const rect = Array.from(range.getClientRects()).find(r => r.width > 0 && r.height > 0);
      if (rect) return clippedBox(icon, rect);
    }
    return clippedBox(icon, icon.getBoundingClientRect());
  };
  const iconLabel = icon => {
    const text = icon.textContent.trim();
    if (text) return text;
    const media = icon.matches('svg, img') ? icon : icon.querySelector('svg, img');
    return media ? media.getAttribute('aria-label') || media.getAttribute('alt') || media.tagName.toLowerCase() : '';
  };
  const branchUnder = (el, row) => {
    let branch = el;
    while (branch.parentElement && branch.parentElement !== row) branch = branch.parentElement;
    return branch.parentElement === row ? branch : null;
  };
  const rowCache = new Map();
  const rowChildren = row => {
    if (!rowCache.has(row)) {
      const items = Array.from(row.children).map(el => ({el, text: firstLine(el)})).filter(item => item.text);
      const directText = directTextLine(row);
      if (directText) items.push({el: row, text: directText});
      rowCache.set(row, items);
    }
    return rowCache.get(row);
  };
  const pairs = [];
  const all = Array.from(document.querySelectorAll('body *')).filter(el => isIcon(el) && visible(el));
  const icons = all.filter(el => !all.some(parent => parent !== el && parent.contains(el)));
  for (const icon of icons) {
    const media = icon.matches('svg, img') ? icon : icon.querySelector('svg, img');
    // Видимость контейнера не делает видимым вложенный значок: мерить его рамку, только если виден он сам.
    if (media && !visible(media)) continue;
    const box = visualBox(icon);
    if (!box) continue;
    let best = null;
    for (let row = icon.parentElement; row && !best; row = row.parentElement) {
      const cs = getComputedStyle(row);
      const directRow = row === icon.parentElement;
      if (!directRow && cs.display !== 'grid' && !(cs.display === 'flex' && cs.flexDirection === 'row')) continue;
      const branch = branchUnder(icon, row);
      if (!branch) continue;
      let rowBest = null, bestDistance = Infinity;
      for (const item of rowChildren(row)) {
        if (item.el === branch) continue;
        const text = item.text;
        if (Math.min(box.b, text.box.b) <= Math.max(box.t, text.box.t)) continue;
        if (box.b - box.t > text.em * 3) continue;
        if (decoratedBlock(item.el, text.em)) continue;
        const distance = box.r < text.box.l ? text.box.l - box.r : text.box.r < box.l ? box.l - text.box.r : 0;
        if (distance < bestDistance) { rowBest = text; bestDistance = distance; }
      }
      best = rowBest;
    }
    if (!best) continue;
    const blankDecoration = !media && !icon.textContent.trim() && decorated(icon);
    if (blankDecoration && (box.r - box.l > best.em * 2 || box.b - box.t > best.em * 2)) continue;
    const iconCenter = (box.t + box.b) / 2;
    const textCenter = (best.box.t + best.box.b) / 2;
    const shift = iconCenter - textCenter;
    if (Math.abs(shift) <= 2) continue;
    const shorten = s => s.length > 60 ? s.slice(0, 60) + '…' : s;
    pairs.push({iconText: shorten(iconLabel(icon)), selector: describe(icon), neighbor: shorten(best.text),
                shift: Math.round(shift * 10) / 10, y: Math.round(Math.min(box.t, best.box.t) + window.scrollY)});
  }
  return {pairs};
})()`

func checkAlign(ctx context.Context) (*alignResult, error) {
	res, err := chromedp.Run(ctx, chromedp.Evaluate[alignResult](alignJS))
	if err != nil {
		return nil, fmt.Errorf("проверка выравнивания значков: %w", err)
	}
	if len(res.Pairs) == 0 {
		return nil, nil
	}
	return &res, nil
}

func (a *alignResult) report(name string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "page-shot: %s: значки не выровнены с текстом, пар: %d (сдвиг больше %d px)\n", name, len(a.Pairs), alignThreshold)
	for i, p := range a.Pairs {
		if i == maxCulprits {
			fmt.Fprintf(&b, "  ...и ещё %d\n", len(a.Pairs)-maxCulprits)
			break
		}
		direction := "вверх"
		if p.Shift > 0 {
			direction = "вниз"
		}
		fmt.Fprintf(&b, "  %s %q и текст %q: %+.1f px (%s), y=%g\n", p.Selector, p.IconText, p.Neighbor, p.Shift, direction, p.Y)
	}
	return b.String()
}
