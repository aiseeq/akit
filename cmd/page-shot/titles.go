package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/chromedp/chromedp"
)

// maxTitleSamples is how many tooltip elements the warning names.
const maxTitleSamples = 5

// titleTips are native tooltips on the page: a title attribute, or a <title>
// child of an SVG element. The browser draws them in system colours, page
// styles do not reach them, and a still frame never shows them.
type titleTips struct {
	Count   int        `json:"count"`
	Samples []titleTip `json:"samples"`
}

type titleTip struct {
	Selector string `json:"selector"`
	Text     string `json:"text"`
}

const titlesJS = `(() => {
  const describe = el => {
    const tag = el.tagName.toLowerCase();
    if (el.id) return tag + '#' + el.id;
    // SVG elements keep className as an object, so read the attribute.
    const cls = (el.getAttribute('class') || '').trim().split(/\s+/).filter(Boolean).slice(0, 2);
    return cls.length ? tag + '.' + cls.join('.') : tag;
  };
  const short = text => {
    const chars = [...text.trim()];
    return chars.length > 60 ? chars.slice(0, 60).join('') + '…' : chars.join('');
  };
  // One pass in document order. The <title> in <head> has no title attribute
  // and is not inside an <svg>, so it is not matched; an empty text shows no tooltip.
  const found = [];
  for (const el of document.querySelectorAll('[title], svg title')) {
    if (el.tagName.toLowerCase() === 'title' && el.closest('svg')) {
      const text = short(el.textContent);
      if (text && el.parentElement) found.push({selector: describe(el.parentElement) + ' > title', text});
      continue;
    }
    const text = short(el.getAttribute('title'));
    if (text) found.push({selector: describe(el), text});
  }
  return {count: found.length, samples: found};
})()`

// checkTitles returns the native tooltips of the page, nil when it has none.
func checkTitles(ctx context.Context) (*titleTips, error) {
	tips, err := chromedp.Run(ctx, chromedp.Evaluate[titleTips](titlesJS))
	if err != nil {
		return nil, fmt.Errorf("проверка подсказок title: %w", err)
	}
	if tips.Count == 0 {
		return nil, nil
	}
	tips.Samples = tips.Samples[:min(len(tips.Samples), maxTitleSamples)]
	return &tips, nil
}

// report is the stderr warning: how many tooltips and the first few of them.
func (t *titleTips) report() string {
	var b strings.Builder
	fmt.Fprintf(&b, "page-shot: предупреждение: подсказок title= на странице: %d — их рисует браузер цветами системы (стили страницы не действуют), кадр их не показывает; нужна своя подсказка в стиле страницы\n", t.Count)
	for _, s := range t.Samples {
		fmt.Fprintf(&b, "  %s «%s»\n", s.Selector, s.Text)
	}
	if t.Count > len(t.Samples) {
		fmt.Fprintf(&b, "  ...и ещё %d\n", t.Count-len(t.Samples))
	}
	return b.String()
}
