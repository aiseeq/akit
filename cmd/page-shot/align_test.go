package main

import (
	"context"
	"testing"
)

func TestCheckAlign(t *testing.T) {
	allocCtx := newLayoutTestBrowser(t)
	cases := []layoutTestCase{
		{
			name:     "inline svg shifted down",
			html:     `<!doctype html><body style="font:16px/1.55 sans-serif"><p style="margin:0"><svg width="16" height="16" style="position:relative;top:4px;transform:translateY(4px)"><rect width="16" height="16"/></svg>Текст рядом со значком</p></body>`,
			pairs:    1,
			contains: "(вниз)",
		},
		{
			name:     "inline svg shifted up",
			html:     `<!doctype html><body style="font:16px/1.55 sans-serif"><p style="margin:0"><svg width="16" height="16" style="position:relative;top:-4px"><rect width="16" height="16"/></svg>Текст рядом со значком</p></body>`,
			pairs:    1,
			contains: "(вверх)",
		},
		{
			name:     "inline svg in centered flex shifted by transform",
			html:     `<!doctype html><body style="font:16px/1.55 sans-serif"><div style="display:flex;align-items:center;gap:8px"><svg width="16" height="16" style="transform:translateY(4px)"><rect width="16" height="16"/></svg><span>Текст рядом со значком</span></div></body>`,
			pairs:    1,
			contains: "(вниз)",
		},
		{
			name:     "inline svg shifted down with unit line height",
			html:     `<!doctype html><body style="font:16px/1 sans-serif"><p style="margin:0"><svg width="16" height="16" style="position:relative;top:4px;transform:translateY(4px)"><rect width="16" height="16"/></svg>Текст рядом со значком</p></body>`,
			pairs:    1,
			contains: "+5.5 px (вниз)",
		},
		{
			name:  "inline svg aligned",
			html:  `<!doctype html><body style="font:16px/1.55 sans-serif"><p style="margin:0"><svg width="1em" height="1em" style="vertical-align:-0.125em"><rect width="16" height="16"/></svg>Текст рядом со значком</p></body>`,
			pairs: 0,
		},
		{
			name:     "svg inside button is checked",
			html:     `<!doctype html><body style="font:16px/1.55 sans-serif"><div style="display:flex;align-items:center;gap:8px"><button style="display:flex;align-items:center;padding:0;border:0"><svg width="16" height="16" style="transform:translateY(4px)"><rect width="16" height="16"/></svg></button><span>Текст рядом со значком</span></div></body>`,
			pairs:    1,
			contains: "(вниз)",
		},
		{
			name:  "flex arrow and dot shifted",
			html:  `<!doctype html><body style="font:16px/1.55 sans-serif"><div style="display:flex;align-items:flex-start;gap:8px"><span style="font-size:14px;line-height:1">→</span><span style="width:10px;height:10px;background:#000;margin-top:0"></span><p style="margin:0">Текст в абзаце рядом с символом</p></div></body>`,
			pairs: 2,
		},
		{
			name:     "grid icon with nested text",
			html:     `<!doctype html><body style="font:16px/1.55 sans-serif"><p style="display:grid;grid-template-columns:22px 1fr;margin:0"><span class="ic" style="display:block;font-size:12px;line-height:1"><svg width="15" height="15"><path d="M0 0h15v15"/></svg></span><span>Начало <span>вложенного текста</span> и продолжение</span></p></body>`,
			pairs:    1,
			contains: "(вверх)",
		},
		{
			name:     "letter icon found by second class word",
			html:     `<!doctype html><body style="font:16px/1.55 sans-serif"><p style="margin:0"><span class="small icon" style="position:relative;top:-6px;font-size:12px">i</span>Текст рядом с буквенным значком</p></body>`,
			pairs:    1,
			contains: "(вверх)",
		},
		{
			name:  "hidden svg in visible icon container",
			html:  `<!doctype html><body style="font:16px/1.55 sans-serif"><p style="margin:0"><span class="ic"><svg width="16" height="16" style="visibility:hidden;position:relative;top:-8px"><rect width="16" height="16"/></svg></span>Текст рядом со скрытым значком</p></body>`,
			pairs: 0,
		},
		{
			name:     "svg next to a decorated label",
			html:     `<!doctype html><body style="font:16px/1 sans-serif"><div style="display:flex;align-items:center;gap:6px"><svg width="16" height="16" style="transform:translateY(6px)"><rect width="16" height="16"/></svg><span style="background:#ddd;min-width:80px;padding:2px 6px">Подпись</span></div></body>`,
			pairs:    1,
			contains: "(вниз)",
		},
		{
			name:  "icons in far table cells are not on the label line",
			html:  `<!doctype html><body style="font:16px/1.4 sans-serif"><div style="display:grid;grid-template-columns:120px repeat(3,60px);border-top:1px solid #ccc"><div style="border-bottom:1px solid #ccc;padding:6px">sc2demu</div><div></div><div style="display:flex;justify-content:center;align-items:center;height:48px"><svg width="14" height="14"><rect width="14" height="14"/></svg></div><div style="display:flex;justify-content:center;align-items:center;height:48px"><svg width="14" height="14"><rect width="14" height="14"/></svg></div></div></body>`,
			pairs: 0,
		},
		{
			name:  "wide empty strip is not a small icon",
			html:  `<!doctype html><body style="font:16px/1.55 sans-serif"><div style="display:grid;grid-template-columns:auto 1fr;gap:8px"><span style="width:40px;height:5px;background:#888"></span><span>Подпись рядом с полосой</span></div></body>`,
			pairs: 0,
		},
		{
			name:  "treemap tiles are not icons or row text",
			html:  `<!doctype html><body style="font:16px/1.55 sans-serif"><div style="display:grid;grid-template-columns:160px 1fr;gap:4px"><div style="height:72px;background:repeating-linear-gradient(135deg,#ddd 0 5px,#bbb 5px 10px)"></div><div style="height:72px;background:#eee;padding:6px"><span>sc2go</span><br><span>6.3%</span></div></div></body>`,
			pairs: 0,
		},
		{
			name:  "centered flex svg",
			html:  `<!doctype html><body style="font:16px/1.55 sans-serif"><div style="display:flex;align-items:center;gap:8px"><svg width="16" height="16"><rect width="16" height="16"/></svg><p style="margin:0">Текст рядом со значком</p></div></body>`,
			pairs: 0,
		},
		{
			name:  "inline svg baseline",
			html:  `<!doctype html><body style="font:16px/1.55 sans-serif"><p style="margin:0"><svg width="1em" height="1em" style="vertical-align:-0.125em"><rect width="16" height="16"/></svg>Текст рядом со значком</p></body>`,
			pairs: 0,
		},
		{
			name:  "centered dot",
			html:  `<!doctype html><body style="font:16px/1.55 sans-serif"><div style="display:flex;align-items:flex-start;gap:8px"><span style="width:10px;height:10px;background:#000;margin-top:calc((1.55em - 10px) / 2)"></span><p style="margin:0">Текст рядом со значком</p></div></body>`,
			pairs: 0,
		},
		{
			name:  "large image and hidden symbol",
			html:  `<!doctype html><body style="font:16px/1.55 sans-serif"><p><img width="120" height="120">Текст рядом с картинкой</p><div style="display:flex"><span style="visibility:hidden">→</span><span>Скрытый значок</span></div></body>`,
			pairs: 0,
		},
	}

	testLayoutCases(t, allocCtx, cases, func(ctx context.Context) (int, string, error) {
		res, err := checkAlign(ctx)
		if err != nil || res == nil {
			return 0, "", err
		}
		return len(res.Pairs), res.report("800x600"), nil
	}, "пар выравнивания")
}
