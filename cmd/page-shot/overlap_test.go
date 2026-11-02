package main

import (
	"context"
	"testing"
)

// Страницы-примеры: в первых двух подпись закрыта другой, в третьей тексты
// стоят плотно, но не налезают (плотный line-height, скрытый текст, обрезка).
const (
	svgOverlap = `<!doctype html><body><svg width="400" height="200" viewBox="0 0 200 100">
<text x="10" y="50" font-size="10">Резерв 6</text>
<text x="20" y="52" font-size="10">$328 выводы</text>
<text x="10" y="90" font-size="10">отдельно</text>
</svg></body>`
	htmlOverlap = `<!doctype html><body style="font:16px sans-serif">
<div style="position:relative;height:60px">
<span style="position:absolute;left:0;top:10px">Подпись события один</span>
<span style="position:absolute;left:40px;top:14px">Подпись события два</span>
</div></body>`
	clean = `<!doctype html><body style="font:16px sans-serif;width:300px">
<details><summary>Свёрнуто</summary><span style="position:absolute;left:0;top:0">Под свёрнутым</span></details>
<p style="line-height:1">Длинный абзац с <b>жирным</b> словом и <i>курсивом</i>, который переносится
на несколько строк при плотном межстрочном интервале и не должен считаться наложением.</p>
<div style="position:relative;height:40px">
<span style="position:absolute;left:0;top:0">Видимая</span>
<span style="position:absolute;left:0;top:0;visibility:hidden">Скрытая подпись</span>
<span style="position:absolute;left:0;top:0;opacity:0">Прозрачная</span>
</div>
<div style="display:flex;gap:0">
<div style="width:60px;overflow:hidden;white-space:nowrap">обрезанный длинный текст</div>
<div>сосед</div>
</div>
<svg width="300" height="60"><text x="0" y="20" font-size="12">слева</text><text x="150" y="20" font-size="12">справа</text></svg>
</body>`
)

func TestCheckOverlap(t *testing.T) {
	allocCtx := newLayoutTestBrowser(t)
	cases := []layoutTestCase{
		{name: "svg", html: svgOverlap, pairs: 1},
		{name: "html", html: htmlOverlap, pairs: 1},
		{name: "clean", html: clean, pairs: 0},
	}
	testLayoutCases(t, allocCtx, cases, func(ctx context.Context) (int, string, error) {
		res, err := checkOverlap(ctx)
		if err != nil || res == nil {
			return 0, "", err
		}
		return len(res.Pairs), res.report("800x600"), nil
	}, "пар наложения")
}
