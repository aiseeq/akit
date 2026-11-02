package main

// layoutCommonJS shares visibility, clipping, element descriptions and text
// line metrics between the layout checks.
const layoutCommonJS = `const describe = el => {
    const tag = el.tagName.toLowerCase();
    if (el.id) return tag + '#' + el.id;
    const cn = typeof el.className === 'string' ? el.className : (el.className && el.className.baseVal) || '';
    const cls = cn.trim().split(/\s+/).filter(Boolean).slice(0, 2);
    return cls.length ? tag + '.' + cls.join('.') : tag;
  };
  const hiddenCache = new Map();
  const hidden = el => {
    if (!el || el === document.documentElement) return false;
    if (hiddenCache.has(el)) return hiddenCache.get(el);
    const cs = getComputedStyle(el);
    const h = cs.opacity === '0' || hidden(el.parentElement);
    hiddenCache.set(el, h);
    return h;
  };
  const visible = el => el.checkVisibility
    ? el.checkVisibility({checkOpacity: true, checkVisibilityCSS: true, opacityProperty: true, visibilityProperty: true})
    : getComputedStyle(el).visibility === 'visible' && !hidden(el);
  const clipCache = new Map();
  const clipOf = el => {
    if (!el || el === document.body || el === document.documentElement) return null;
    if (clipCache.has(el)) return clipCache.get(el);
    let c = clipOf(el.parentElement);
    const cs = getComputedStyle(el);
    if (cs.overflowX !== 'visible' || cs.overflowY !== 'visible') {
      const r = el.getBoundingClientRect();
      c = c ? {l: Math.max(c.l, r.left), t: Math.max(c.t, r.top), r: Math.min(c.r, r.right), b: Math.min(c.b, r.bottom)}
            : {l: r.left, t: r.top, r: r.right, b: r.bottom};
    }
    clipCache.set(el, c);
    return c;
  };
  const clippedBox = (el, r) => {
    const c = clipOf(el);
    const box = {l: r.left ?? r.l, t: r.top ?? r.t, r: r.right ?? r.r, b: r.bottom ?? r.b};
    if (c) { box.l = Math.max(box.l, c.l); box.t = Math.max(box.t, c.t); box.r = Math.min(box.r, c.r); box.b = Math.min(box.b, c.b); }
    return box.r - box.l >= 1 && box.b - box.t >= 1 ? box : null;
  };
  const textBox = (el, r, em) => {
    const box = {l: r.left, t: r.top, r: r.right, b: r.bottom};
    const inset = Math.max(0, (box.b - box.t - em) / 2);
    box.t += inset; box.b -= inset;
    return clippedBox(el, box);
  };`
