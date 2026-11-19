package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/chromedp/chromedp"
)

// pageFontFamilies is the set of permitted first-choice families.
const pageFontFamilies = `Golos Text|Russo One|JetBrains Mono`

type fontViolation struct {
	Selector string `json:"selector"`
	Text     string `json:"text"`
	Family   string `json:"family"`
}

type fontResult struct {
	Violations []fontViolation `json:"violations"`
}

const fontsJS = `(() => {
  ` + layoutCommonJS + `
  const allowed = new Set(("` + pageFontFamilies + `").toLowerCase().split('|'));
  const familyParts = value => {
    const parts = []; let part = '', quote = '';
    for (const ch of value) {
      if (quote) { part += ch; if (ch === quote) quote = ''; }
      else if (ch === '"' || ch === "'") { quote = ch; part += ch; }
      else if (ch === ',') { parts.push(part.trim()); part = ''; }
      else part += ch;
    }
    parts.push(part.trim());
    return parts.map(s => s.replace(/^['"]|['"]$/g, '').trim().toLowerCase());
  };
  const violations = [], seen = new WeakSet();
  const docWidth = Math.max(document.documentElement.scrollWidth, document.body?.scrollWidth || 0, innerWidth);
  const docHeight = Math.max(document.documentElement.scrollHeight, document.body?.scrollHeight || 0, innerHeight);
  const inDocument = r => {
    if (!r || r.width <= 0 || r.height <= 0 || r.width * r.height <= 1) return false;
    const left = r.left + scrollX, right = r.right + scrollX;
    const top = r.top + scrollY, bottom = r.bottom + scrollY;
    return right > 0 && left < docWidth && bottom > 0 && top < docHeight;
  };
  const emptyInset = (value, rect) => {
    const match = /^inset\(([^)]*)\)/.exec(value);
    if (!match) return false;
    const sides = match[1].split(/\s+/).map(v => parseFloat(v));
    if (!sides.length || sides.some(Number.isNaN)) return false;
    const [top, right = top, bottom = top, left = right] = sides;
    const horizontal = left + right, vertical = top + bottom;
    const percent = match[1].includes('%');
    return percent ? horizontal >= 100 || vertical >= 100 : horizontal >= rect.width || vertical >= rect.height;
  };
  const emptyLegacyClip = value => {
    const match = /^rect\(([^)]*)\)$/.exec(value);
    if (!match) return false;
    const edges = match[1].split(/[\s,]+/).map(v => parseFloat(v));
    return edges.length === 4 && edges.every(Number.isFinite) && (edges[2] <= edges[0] || edges[1] <= edges[3]);
  };
  const emptyShapeClip = (value, rect) => {
    const polygon = /^polygon\((.*)\)$/.exec(value);
    if (polygon) {
      const points = polygon[1].replace(/^\s*(nonzero|evenodd)\s*,/i, '').split(',').map(point => {
        const coordinates = point.trim().split(/\s+/);
        if (coordinates.length < 2) return [];
        return coordinates.slice(0, 2).map((coordinate, axis) => {
          const match = /^(-?(?:\d+\.?\d*|\.\d+))(px|%)?$/.exec(coordinate);
          if (!match) return Number.NaN;
          const value = Number(match[1]);
          if (match[2] === '%') return value * (axis === 0 ? rect.width : rect.height) / 100;
          return value;
        });
      });
      if (points.length < 3 || points.some(point => point.length !== 2 || point.some(Number.isNaN))) return false;
      let area = 0;
      for (let i = 0; i < points.length; i++) {
        const current = points[i], next = points[(i + 1) % points.length];
        area += current[0] * next[1] - next[0] * current[1];
      }
      return area === 0;
    }
    const match = /^(circle|ellipse)\((.*)\)$/.exec(value);
    if (!match) return false;
    const dimensions = match[2].split(/\s+at\s+/)[0].trim().split(/\s+/);
    if (match[1] === 'circle') return dimensions.length > 0 && parseFloat(dimensions[0]) === 0;
    return dimensions.length > 1 && (parseFloat(dimensions[0]) === 0 || parseFloat(dimensions[1]) === 0);
  };
  const establishesContainingBlock = (node, position) => {
    const style = getComputedStyle(node);
    if (position === 'absolute' && style.position !== 'static') return true;
    return style.transform !== 'none' || style.perspective !== 'none' ||
      style.filter !== 'none' || style.backdropFilter !== 'none' ||
      /(^|\s)(layout|paint|strict|content)(\s|$)/.test(style.contain) ||
      /(^|,\s*)(transform|perspective|filter|contain)(\s|,|$)/.test(style.willChange);
  };
  const overflowAppliesTo = display => {
    if (display === 'contents' || display.startsWith('ruby')) return false;
    const parts = display.split(/\s+/);
    const inlineOuter = parts[0] === 'inline';
    if (!inlineOuter) return true;
    const inner = parts[1] || 'flow';
    return inner === 'flow-root' || inner === 'flex' || inner === 'grid' || inner === 'table';
  };
  const escapesOverflow = (el, clipNode) => {
    let positioned = el;
    while (positioned) {
      let position = getComputedStyle(positioned).position;
      while (positioned && position !== 'absolute' && position !== 'fixed') {
        positioned = positioned.parentElement;
        if (positioned) position = getComputedStyle(positioned).position;
      }
      if (!positioned) return false;
      if (positioned === clipNode) return false;
      if (!clipNode.contains(positioned)) return false;
      let containing = null;
      for (let node = positioned.parentElement; node; node = node.parentElement) {
        if (establishesContainingBlock(node, position)) { containing = node; break; }
      }
      if (!containing || (containing !== clipNode && !clipNode.contains(containing))) return true;
      positioned = containing;
    }
    return false;
  };
  const fontRectVisible = (el, r) => {
    if (!r || r.width <= 0 || r.height <= 0) return false;
    let left = r.left, right = r.right, top = r.top, bottom = r.bottom;
    const htmlStyle = getComputedStyle(document.documentElement);
    const bodyStyle = getComputedStyle(document.body);
    const bodyScrolls = axis => {
      const value = axis === 'x' ? bodyStyle.overflowX : bodyStyle.overflowY;
      const body = document.body;
      return (value === 'auto' || value === 'scroll') && body &&
        (axis === 'x' ? body.scrollWidth > body.clientWidth && body.getBoundingClientRect().width <= innerWidth :
          body.scrollHeight > body.clientHeight && body.getBoundingClientRect().height <= innerHeight);
    };
    const escapesBody = document.body && escapesOverflow(el, document.body);
    const viewportOverflow = axis => {
      const rootValue = axis === 'x' ? htmlStyle.overflowX : htmlStyle.overflowY;
      const bodyValue = axis === 'x' ? bodyStyle.overflowX : bodyStyle.overflowY;
      if (escapesBody && rootValue !== 'visible') return rootValue;
      if (bodyScrolls(axis)) return bodyValue;
      return rootValue !== 'visible' ? rootValue : bodyValue;
    };
    let fixedToViewport = false;
    for (let item = el; item; item = item.parentElement) {
      if (getComputedStyle(item).position !== 'fixed') continue;
      let containing = null;
      for (let node = item.parentElement; node; node = node.parentElement) {
        if (establishesContainingBlock(node, 'fixed')) { containing = node; break; }
      }
      fixedToViewport = containing === null;
    }
    const hiddenViewportX = viewportOverflow('x') === 'hidden' || viewportOverflow('x') === 'clip';
    const hiddenViewportY = viewportOverflow('y') === 'hidden' || viewportOverflow('y') === 'clip';
    for (let node = el; node; node = node.parentElement) {
      const style = getComputedStyle(node);
      if (style.display === 'none' || style.contentVisibility === 'hidden' || Number(style.opacity) === 0 ||
          (node === el && style.visibility !== 'visible')) return false;
      if (node.tagName === 'DETAILS' && !node.open) {
        let item = el;
        while (item && item !== node && item.tagName !== 'SUMMARY') item = item.parentElement;
        if (!item || item === node) return false;
      }
      const rect = node.getBoundingClientRect();
      if (emptyInset(style.clipPath, rect) || emptyShapeClip(style.clipPath, rect) || emptyLegacyClip(style.clip)) return false;
      const viewportNode = node === document.body || node === document.documentElement;
      const escaped = escapesOverflow(el, node);
      const overflowApplies = overflowAppliesTo(style.display);
      const scrollsX = overflowApplies && (style.overflowX === 'auto' || style.overflowX === 'scroll') && node.scrollWidth > node.clientWidth;
      const scrollsY = overflowApplies && (style.overflowY === 'auto' || style.overflowY === 'scroll') && node.scrollHeight > node.clientHeight;
      const clipsX = overflowApplies && (style.overflowX === 'hidden' || style.overflowX === 'clip');
      const clipsY = overflowApplies && (style.overflowY === 'hidden' || style.overflowY === 'clip');
      const clipsUnscrollableX = overflowApplies && (style.overflowX === 'auto' || style.overflowX === 'scroll') && !scrollsX;
      const clipsUnscrollableY = overflowApplies && (style.overflowY === 'auto' || style.overflowY === 'scroll') && !scrollsY;
      const paintContainment = overflowApplies && /(^|\s)(paint|strict|content)(\s|$)/.test(style.contain);
      if (!viewportNode && !escaped) {
        if (scrollsX) {
          if (node.clientWidth <= 0) return false;
          const range = scrollRange(node, 'x');
          const scale = node.offsetWidth > 0 ? rect.width / node.offsetWidth : 1;
          const start = rect.left + node.clientLeft * scale - (range.current - range.min) * scale;
          if (right <= start || left >= start + (node.scrollWidth + range.extra) * scale) return false;
          left = rect.left + node.clientLeft * scale;
          right = left + node.clientWidth * scale;
        }
        else if (clipsX || clipsUnscrollableX) {
          const scale = node.offsetWidth > 0 ? rect.width / node.offsetWidth : 1;
          const clipLeft = rect.left + node.clientLeft * scale;
          left = Math.max(left, clipLeft); right = Math.min(right, clipLeft + node.clientWidth * scale);
        }
        if (scrollsY) {
          if (node.clientHeight <= 0) return false;
          const range = scrollRange(node, 'y');
          const scale = node.offsetHeight > 0 ? rect.height / node.offsetHeight : 1;
          const start = rect.top + node.clientTop * scale - (range.current - range.min) * scale;
          if (bottom <= start || top >= start + (node.scrollHeight + range.extra) * scale) return false;
          top = rect.top + node.clientTop * scale;
          bottom = top + node.clientHeight * scale;
        }
        else if (clipsY || clipsUnscrollableY) {
          const scale = node.offsetHeight > 0 ? rect.height / node.offsetHeight : 1;
          const clipTop = rect.top + node.clientTop * scale;
          top = Math.max(top, clipTop); bottom = Math.min(bottom, clipTop + node.clientHeight * scale);
        }
        if (paintContainment) {
          const scaleX = node.offsetWidth > 0 ? rect.width / node.offsetWidth : 1;
          const scaleY = node.offsetHeight > 0 ? rect.height / node.offsetHeight : 1;
          const clipLeft = rect.left + node.clientLeft * scaleX;
          const clipTop = rect.top + node.clientTop * scaleY;
          left = Math.max(left, clipLeft); right = Math.min(right, clipLeft + node.clientWidth * scaleX);
          top = Math.max(top, clipTop); bottom = Math.min(bottom, clipTop + node.clientHeight * scaleY);
        }
      }
      if (node === document.body && htmlStyle.overflowX !== 'visible' && !escaped && clipsX) {
        left = Math.max(left, rect.left); right = Math.min(right, rect.right);
      }
      if (node === document.body && htmlStyle.overflowY !== 'visible' && !escaped && clipsY) {
        top = Math.max(top, rect.top); bottom = Math.min(bottom, rect.bottom);
      }
      if (right - left <= 0 || bottom - top <= 0) return false;
    }
    if (fixedToViewport || hiddenViewportX) { left = Math.max(left, 0); right = Math.min(right, innerWidth); }
    if (fixedToViewport || hiddenViewportY) { top = Math.max(top, 0); bottom = Math.min(bottom, innerHeight); }
    if (right - left <= 0 || bottom - top <= 0) return false;
    const visibleRect = {left, right, top, bottom, width: right - left, height: bottom - top};
    if (inDocument(visibleRect)) return true;
    const documentRight = right + scrollX, documentBottom = bottom + scrollY;
    if (documentRight <= 0 || documentBottom <= 0) return false;
    const windowScrollsX = viewportOverflow('x') === 'auto' || viewportOverflow('x') === 'scroll';
    const windowScrollsY = viewportOverflow('y') === 'auto' || viewportOverflow('y') === 'scroll';
    const outsideX = documentRight > docWidth && left + scrollX >= 0;
    const outsideY = documentBottom > docHeight && top + scrollY >= 0;
    return Boolean((outsideX || outsideY) && (windowScrollsX || windowScrollsY));
  };
  const scrollRange = (node, axis) => {
    const property = axis === 'x' ? 'scrollLeft' : 'scrollTop';
    const current = node[property];
    const style = getComputedStyle(node);
    const vertical = style.writingMode.startsWith('vertical') || style.writingMode.startsWith('sideways');
    const inlineAxis = vertical ? 'y' : 'x';
    const inlineReverse = (style.direction === 'rtl') !== style.writingMode.endsWith('sideways-lr');
    const blockAxis = vertical ? 'x' : 'y';
    const blockReverse = style.writingMode.endsWith('-rl');
    let reverse = axis === inlineAxis ? inlineReverse : axis === blockAxis && blockReverse;
    if (style.display === 'flex' || style.display === 'inline-flex') {
      const row = style.flexDirection.startsWith('row');
      const mainAxis = row ? inlineAxis : blockAxis;
      if (axis === mainAxis) reverse = (row ? inlineReverse : blockReverse) !== style.flexDirection.endsWith('reverse');
      else if (style.flexWrap === 'wrap-reverse') reverse = !reverse;
    }
    const extent = Math.max(0, (axis === 'x' ? node.scrollWidth - node.clientWidth : node.scrollHeight - node.clientHeight));
    const legacyBox = style.display === '-webkit-box' || style.display === '-webkit-inline-box';
    const min = current < 0 || (current === 0 && (reverse || legacyBox)) ? -extent : 0;
    return {current, min, extra: legacyBox ? extent : 0};
  };
  const pseudoRect = (el, style, text) => {
    const host = el.getBoundingClientRect();
    const fontSize = parseFloat(style.fontSize) || 16;
    const canvas = document.createElement('canvas');
    const context = canvas.getContext('2d');
    context.font = style.font;
    const width = Math.max(context.measureText(text).width, fontSize);
    const height = parseFloat(style.lineHeight) || fontSize * 1.2;
    let left = host.left, top = host.top;
    if (style.position === 'absolute' || style.position === 'fixed') {
      const position = style.position;
      let containing = null;
      for (let node = el; node; node = node.parentElement) {
        if (establishesContainingBlock(node, position)) { containing = node; break; }
      }
      const base = position === 'fixed' && !containing ? {left: 0, top: 0} :
        (containing || el).getBoundingClientRect();
      const x = parseFloat(style.left), y = parseFloat(style.top);
      if (Number.isFinite(x)) left = base.left + x;
      if (Number.isFinite(y)) top = base.top + y;
    }
    return {left, top, right: left + width, bottom: top + height, width, height};
  };
  const textInputTypes = new Set(['text', 'search', 'email', 'url', 'tel', 'number', 'password', 'date']);
  const placeholderTypes = new Set(['text', 'search', 'email', 'url', 'tel', 'number', 'password']);
  const add = (el, text, family, suffix = '') => {
    if (!text || seen.has(el)) return;
    const short = text.trim().replace(/\s+/g, ' ').slice(0, 60);
    if (!short || allowed.has(familyParts(family)[0])) return;
    seen.add(el);
    violations.push({selector: describe(el) + suffix, text: short, family});
  };
  const roots = [document.body || document.documentElement], seenRoots = new Set(roots);
  for (let ri = 0; ri < roots.length; ri++) {
    const root = roots[ri];
    const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
    for (let n = walker.nextNode(); n; n = walker.nextNode()) {
      const text = n.nodeValue.trim(), el = n.parentElement;
      if (!text || !el) continue;
      const range = document.createRange(); range.selectNodeContents(n);
      if (!Array.from(range.getClientRects()).some(r => fontRectVisible(el, r))) continue;
      add(el, text, getComputedStyle(el).fontFamily);
    }
    for (const el of root.querySelectorAll('*')) {
      if (el.shadowRoot && !seenRoots.has(el.shadowRoot)) {
        roots.push(el.shadowRoot); seenRoots.add(el.shadowRoot);
      }
      const rect = el.getBoundingClientRect();
      if (el.matches('input,textarea') && el.value &&
          (el.matches('textarea') || textInputTypes.has(el.type) || el.type === 'submit' || el.type === 'button' || el.type === 'reset') &&
          fontRectVisible(el, rect)) {
        add(el, el.value, getComputedStyle(el).fontFamily);
      }
      if (el.matches('input,textarea') && el.placeholder && !el.value &&
          (el.matches('textarea') || placeholderTypes.has(el.type)) && fontRectVisible(el, rect)) {
        add(el, el.placeholder, getComputedStyle(el).fontFamily);
      }
      if (el.matches('select') && el.selectedOptions.length && fontRectVisible(el, rect)) {
        add(el, el.selectedOptions[0].text, getComputedStyle(el).fontFamily);
      }
      for (const pseudo of ['::before', '::after']) {
        const style = getComputedStyle(el, pseudo), content = style.content.trim();
        if (!content || content === 'none' || content === 'normal' || content.startsWith('url(') ||
            style.display === 'none' || style.visibility !== 'visible' || style.opacity === '0') continue;
        const text = content.replace(/^(['"])(.*)\1$/s, '$2').trim();
        if (!text) continue;
        const pseudoBox = rect.width > 0 && rect.height > 0 ? rect : pseudoRect(el, style, text);
        if (fontRectVisible(el, pseudoBox)) add(el, text, style.fontFamily, pseudo);
      }
    }
  }
  return {violations};
})()`

func checkFonts(ctx context.Context) (*fontResult, error) {
	res, err := chromedp.Run(ctx, chromedp.Evaluate[fontResult](fontsJS))
	if err != nil {
		return nil, fmt.Errorf("проверка шрифтов: %w", err)
	}
	if len(res.Violations) == 0 {
		return nil, nil
	}
	return &res, nil
}

func (f *fontResult) report(window string) string {
	type grouped struct {
		fontViolation
		count int
	}
	groups := make([]grouped, 0, len(f.Violations))
	indexes := make(map[string]int)
	for _, v := range f.Violations {
		key := v.Selector + "\x00" + v.Text + "\x00" + v.Family
		if i, ok := indexes[key]; ok {
			groups[i].count++
		} else {
			indexes[key] = len(groups)
			groups = append(groups, grouped{fontViolation: v, count: 1})
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "page-shot: %s: шрифт вне набора, элементов: %d\n", window, len(f.Violations))
	for i, g := range groups {
		if i == maxCulprits {
			fmt.Fprintf(&b, "  ...и ещё %d\n", len(groups)-maxCulprits)
			break
		}
		fmt.Fprintf(&b, "  %s %q: %q", g.Selector, g.Text, g.Family)
		if g.count > 1 {
			fmt.Fprintf(&b, " (×%d)", g.count)
		}
		b.WriteByte('\n')
	}
	return b.String()
}
