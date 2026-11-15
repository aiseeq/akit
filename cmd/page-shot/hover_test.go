package main

import (
	"bytes"
	"context"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

func TestGazetteDocumentPointeroverTooltipWaitsForDisplay(t *testing.T) {
	browser := newLayoutTestBrowser(t)
	page := `<!doctype html><style>body{background:#111;color:white}#tip{position:fixed;left:80px;top:80px;opacity:0;background:#fafafa;color:#111}</style><button id="gz" data-gz-tip="gazette text">gz</button><div id="tip"></div><script>let timer;document.addEventListener('pointerover',e=>{const el=e.target.closest('[data-gz-tip]');if(!el)return;clearTimeout(timer);timer=setTimeout(()=>{tip.textContent='gazette delayed bubble';tip.style.opacity=1},300)},true)</script>`
	path := writeLayoutTestPage(t, "gazette-delay", page)
	ctx, cancel := newLayoutTestTab(t, browser, path, 800, 600)
	defer cancel()
	warnings, err := checkHoverBubbles(ctx, "dark", 600, filepath.Join(t.TempDir(), "frame.png"))
	if err != nil {
		t.Fatal(err)
	}
	if warningWithText(warnings, "gazette delayed bubble") == nil {
		t.Fatalf("document pointerover Gazette bubble appearing after 300 ms must be caught: %#v", warnings)
	}
}

func TestDarkThemeLoadsCaptureStableDarkFrames(t *testing.T) {
	alloc := newTestAllocator(t)
	page := `<!doctype html><style>*{transition:background-color .4s,color .4s}:root{background:#fff;color:#111}@media(prefers-color-scheme:dark){:root{background:#111;color:#eee}}.low{height:1000px}@media(prefers-color-scheme:dark){.low{width:3000px}}</style><div class="low">below</div>`
	path := writeLayoutTestPage(t, "theme-transition", page)
	out := t.TempDir()
	got, err := shoot(alloc, "file://"+path, out, viewport{390, 844, 1}, []time.Duration{0})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.themes) != 2 || got.themes[1].overflow == nil {
		t.Fatalf("dark-only overflow below fold must be checked: %#v", got.themes)
	}
	light, err := os.ReadFile(filepath.Join(out, "390x844.png"))
	if err != nil {
		t.Fatal(err)
	}
	dark, err := os.ReadFile(filepath.Join(out, "390x844-dark.png"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(light, dark) {
		t.Fatal("dark frame must differ")
	}
	img, err := png.Decode(bytes.NewReader(dark))
	if err != nil {
		t.Fatal(err)
	}
	r, g, b, _ := img.At(1, 1).RGBA()
	if r>>8 > 30 || g>>8 > 30 || b>>8 > 30 {
		t.Fatalf("dark screenshot captured mid-transition: %d,%d,%d", r>>8, g>>8, b>>8)
	}
}

func TestDarkThemeSelectedByInlineScriptIsNotSkipped(t *testing.T) {
	alloc := newTestAllocator(t)
	page := `<!doctype html><style>.dark .wide{width:3000px}</style><body><div class="wide">wide</div><script>if(matchMedia('(prefers-color-scheme: dark)').matches)document.body.className='dark'</script>`
	path := writeLayoutTestPage(t, "js-dark", page)
	got, err := shoot(alloc, "file://"+path, t.TempDir(), viewport{390, 844, 1}, []time.Duration{0})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.themes) != 2 || got.themes[1].overflow == nil {
		t.Fatalf("JS-selected dark layout skipped: %#v", got.themes)
	}
}

func TestHoverCountsMixedFontRunsAsVisualRows(t *testing.T) {
	browser := newLayoutTestBrowser(t)
	page := `<!doctype html><style>body{background:#111;color:white}#b{width:50px;height:20px}#p{position:fixed;top:40px;left:100px;width:240px;background:#fff;color:#111;padding:4px}.g{display:grid;grid-template-columns:1fr auto;gap:2px 12px;font:13px/1.4 sans-serif}.n{font:12px monospace}</style><button id=b></button><div id=p style="opacity:0"><b>Title row</b><div class=g><span>name 1</span><span class=n>10%</span><span>name 2</span><span class=n>20%</span><span>name 3</span><span class=n>30%</span><span>name 4</span><span class=n>40%</span><span>name 5</span><span class=n>50%</span><span>name 6</span><span class=n>60%</span><span>name 7</span><span class=n>70%</span></div></div><script>b.onmouseenter=()=>p.style.opacity=1</script>`
	path := writeLayoutTestPage(t, "mixed-lines", page)
	ctx, cancel := newLayoutTestTab(t, browser, path, 800, 600)
	defer cancel()
	warnings, err := checkHoverBubbles(ctx, "light", 600, filepath.Join(t.TempDir(), "frame.png"))
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || warnings[0].Kind != "фон" || warnings[0].Bubble.Lines != 8 {
		t.Fatalf("mixed font baselines must merge into 8 visual rows without volume warning: %#v", warnings)
	}
}

func TestScrollRevealIsNotHoverBubble(t *testing.T) {
	browser := newLayoutTestBrowser(t)
	page := `<!doctype html><style>.rv{opacity:0;height:500px}.rv.in{opacity:1}</style><div style="height:1000px"></div><section class=rv>Ordinary revealed article content after scrolling.</section><button id=b>trigger</button><div id=p style="position:fixed;left:100px;top:10px;opacity:0;background:#fff"></div><script>new IntersectionObserver(es=>es.forEach(e=>{if(e.isIntersecting)e.target.classList.add('in')})).observe(document.querySelector('.rv'));b.onmouseenter=()=>{p.textContent='small tip';p.style.opacity=1}</script>`
	path := writeLayoutTestPage(t, "scroll-reveal", page)
	ctx, cancel := newLayoutTestTab(t, browser, path, 800, 600)
	defer cancel()
	warnings, err := checkHoverBubbles(ctx, "light", 600, filepath.Join(t.TempDir(), "frame.png"))
	if err != nil {
		t.Fatal(err)
	}
	if warningWithText(warnings, "Ordinary revealed") != nil {
		t.Fatalf("scroll reveal attributed to hover: %#v", warnings)
	}
}

func TestOpenShadowRootTooltipIsDetected(t *testing.T) {
	browser := newLayoutTestBrowser(t)
	page := `<!doctype html><style>body{background:#111;color:white}</style><x-tip></x-tip><script>customElements.define('x-tip',class extends HTMLElement{constructor(){super();let s=this.attachShadow({mode:'open'});s.innerHTML='<button id=t style="width:80px;height:30px">go</button><div id=p style="position:fixed;left:100px;top:20px;opacity:0;background:#fafafa;color:#111"></div>';s.getElementById('t').onmouseenter=()=>{s.getElementById('p').textContent='shadow tip';s.getElementById('p').style.opacity=1}}})</script>`
	path := writeLayoutTestPage(t, "shadow", page)
	ctx, cancel := newLayoutTestTab(t, browser, path, 800, 600)
	defer cancel()
	warnings, err := checkHoverBubbles(ctx, "dark", 600, filepath.Join(t.TempDir(), "frame.png"))
	if err != nil {
		t.Fatal(err)
	}
	if warningWithText(warnings, "shadow tip") == nil {
		t.Fatalf("open shadow tooltip not found: %#v", warnings)
	}
}

func TestRepeatedTooltipDoesNotHideLaterCandidate(t *testing.T) {
	browser := newLayoutTestBrowser(t)
	page := `<!doctype html><style>:root{background:#fff;color:#111;font:14px sans-serif}@media(prefers-color-scheme:dark){:root{background:#111;color:#eee}}</style><div id=p style="position:fixed;left:300px;top:20px;opacity:0;background:#fafafa;color:#111;padding:4px"></div><div id=q style="position:fixed;left:300px;top:80px;opacity:0;background:#1d2226;color:#eee;padding:4px"></div><button class=cp>copy 1</button><button class=cp>copy 2</button><button class=cp>copy 3</button><button class=cp>copy 4</button><button id=bad style="margin-left:60px">bad</button><script>document.querySelectorAll('.cp').forEach(b=>{b.addEventListener('mouseenter',()=>{q.textContent='Copy';q.style.opacity=1});b.addEventListener('mouseleave',()=>q.style.opacity=0)});bad.addEventListener('mouseenter',()=>{p.textContent='bad light tip';p.style.opacity=1});bad.addEventListener('mouseleave',()=>p.style.opacity=0)</script>`
	path := writeLayoutTestPage(t, "repeat-stop", page)
	ctx, cancel := newLayoutTestTab(t, browser, path, 800, 600)
	defer cancel()
	warnings, err := checkHoverBubbles(ctx, "dark", 600, filepath.Join(t.TempDir(), "frame.png"))
	if err != nil {
		t.Fatal(err)
	}
	if warningWithText(warnings, "bad light tip") == nil {
		t.Fatalf("fifth candidate must still be checked after four repeated tooltip targets: %#v", warnings)
	}
}

func TestSharedTooltipNodeIsCheckedOnEveryShow(t *testing.T) {
	browser := newLayoutTestBrowser(t)
	page := `<!doctype html><style>:root{background:#111;color:#eee;font:14px sans-serif}#t{position:fixed;left:300px;top:10px;opacity:0;background:#1d2226;color:#eee;padding:4px}</style><button id=a>short</button><button id=b style="margin-left:60px">long</button><div id=t></div><script>a.onmouseenter=()=>{t.textContent='short tip';t.style.opacity=1};b.onmouseenter=()=>{t.innerHTML='long tip'+'<br>row'.repeat(30);t.style.opacity=1};a.onmouseleave=b.onmouseleave=()=>t.style.opacity=0</script>`
	path := writeLayoutTestPage(t, "shared-node", page)
	ctx, cancel := newLayoutTestTab(t, browser, path, 800, 600)
	defer cancel()
	warnings, err := checkHoverBubbles(ctx, "dark", 600, filepath.Join(t.TempDir(), "frame.png"))
	if err != nil {
		t.Fatal(err)
	}
	if warningWithText(warnings, "long tip") == nil {
		t.Fatalf("one tooltip element shown by two triggers must be checked on the second, longer show: %#v", warnings)
	}
}

func TestShootChecksHoversInOnlyTheSelectedTheme(t *testing.T) {
	alloc := newTestAllocator(t)
	page := `<!doctype html><style>:root{background:#fff;color:#111}@media(prefers-color-scheme:dark){:root{background:#111;color:#eee}}</style><button id=b>show</button><div id=p style="position:fixed;left:100px;top:10px;opacity:0;background:#fff;color:#111"></div><script>b.onmouseenter=()=>{p.innerHTML=Array.from({length:13},()=>'<br>row').join('');p.style.opacity=1}</script>`
	path := writeLayoutTestPage(t, "single-hover-theme", page)
	out := t.TempDir()
	if _, err := shoot(alloc, "file://"+path, out, viewport{1440, 900, 1}, []time.Duration{0}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, "1440x900-dark-hover-01.png")); err != nil {
		t.Fatalf("тёмный tooltip должен проверяться в тёмном проходе: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "1440x900-hover-01.png")); !os.IsNotExist(err) {
		t.Fatalf("светлый hover-проход при наличии тёмной темы не нужен: stat err=%v", err)
	}
}

func TestLongAnimationStillCapturesFrames(t *testing.T) {
	alloc := newTestAllocator(t)
	page := `<!doctype html><style>@keyframes grow{from{width:10px}to{width:300px}}.bar{height:20px;background:#69c;animation:grow 5s ease-out forwards}</style><div class=bar></div>`
	path := writeLayoutTestPage(t, "anim-long", page)
	out := t.TempDir()
	if _, err := shoot(alloc, "file://"+path, out, viewport{390, 844, 1}, []time.Duration{0}); err != nil {
		t.Fatalf("long animation must not block requested capture: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "390x844.png")); err != nil {
		t.Fatalf("animation frame missing: %v", err)
	}
}

func TestIteratingAnimationStillCapturesFrames(t *testing.T) {
	alloc := newTestAllocator(t)
	page := `<!doctype html><style>@keyframes pulse{50%{opacity:.4}}.dot{width:20px;height:20px;background:#c63;animation:pulse 1s 10}</style><div class=dot></div>`
	path := writeLayoutTestPage(t, "anim-iter", page)
	out := t.TempDir()
	if _, err := shoot(alloc, "file://"+path, out, viewport{390, 844, 1}, []time.Duration{0}); err != nil {
		t.Fatalf("iterating animation must not block requested capture: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "390x844.png")); err != nil {
		t.Fatalf("animation frame missing: %v", err)
	}
}

func TestAtFramesCaptureDifferentAnimationMoments(t *testing.T) {
	alloc := newTestAllocator(t)
	page := `<!doctype html><style>@keyframes grow{from{width:10px}to{width:600px}}.bar{height:40px;background:#69c;animation:grow 2.5s linear forwards}</style><div class=bar></div>`
	path := writeLayoutTestPage(t, "anim-at", page)
	out := t.TempDir()
	if _, err := shoot(alloc, "file://"+path, out, viewport{390, 844, 1}, []time.Duration{500 * time.Millisecond, 1500 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(filepath.Join(out, "390x844-500ms.png"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(filepath.Join(out, "390x844-1.5s.png"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("--at frames must preserve their requested animation moments")
	}
}

func TestHoverWarningsCountRenderedLinesAndThemeBackground(t *testing.T) {
	browser := newLayoutTestBrowser(t)
	page := `<!doctype html><meta charset="utf-8"><style>
:root{background:#fff;color:#111;font:14px sans-serif;--ink:#111}
@media(prefers-color-scheme:dark){:root{background:#111;color:#eee;--ink:#eee}}
#trigger{width:80px;height:50px;background:#ccc}
#bubble{position:fixed;left:100px;top:80px;opacity:0;white-space:pre-line;background:var(--ink);color:#f00;pointer-events:none}
</style><div id="trigger"></div><div id="bubble"></div><script>
trigger.addEventListener('pointermove',()=>{bubble.textContent=Array.from({length:20},(_,i)=>'line '+i).join('\n');bubble.style.opacity=1});
</script>`
	path := writeLayoutTestPage(t, "twenty-lines", page)
	for _, theme := range []string{"light", "dark"} {
		ctx, cancel := newLayoutTestTab(t, browser, path, 800, 600)
		warnings, err := checkHoverBubbles(ctx, theme, 600, filepath.Join(t.TempDir(), "frame.png"))
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		volume := warningOfKind(warnings, "объём")
		if volume == nil || volume.Bubble.Lines != 20 {
			t.Fatalf("текст с 20 отрисованными строками: warnings=%#v", warnings)
		}
		background := warningOfKind(warnings, "фон")
		if theme == "dark" && background == nil {
			t.Fatalf("светлый var(--ink) bubble на тёмной странице должен предупреждать: %#v", warnings)
		}
		if theme == "light" && background != nil {
			t.Fatalf("тёмный bubble на светлой странице не должен предупреждать: %#v", warnings)
		}
	}
}

func TestHoverWarningsCountSoftWrappedText(t *testing.T) {
	browser := newLayoutTestBrowser(t)
	page := `<!doctype html><style>body{font:10px sans-serif;background:#111;color:white}#trigger{width:30px;height:30px}#bubble{position:fixed;left:80px;top:20px;width:55px;overflow-wrap:anywhere;opacity:0;background:#eee;color:#111}</style><div id="trigger"></div><div id="bubble"></div><script>trigger.onpointermove=()=>{bubble.textContent='abcdefghijklmnopqrstuvwxyz'.repeat(8);bubble.style.opacity=1}</script>`
	path := writeLayoutTestPage(t, "wrapped-lines", page)
	ctx, cancel := newLayoutTestTab(t, browser, path, 800, 900)
	defer cancel()
	warnings, err := checkHoverBubbles(ctx, "dark", 900, filepath.Join(t.TempDir(), "wrapped.png"))
	if err != nil {
		t.Fatal(err)
	}
	volume := warningOfKind(warnings, "объём")
	if volume == nil || volume.Bubble.Lines <= 12 {
		t.Fatalf("длинная строка должна считаться по визуальным переносам: %#v", warnings)
	}
	if strings.Contains(volume.Bubble.Text, " ") {
		t.Fatalf("первая отрисованная строка не должна включать весь текст: %q", volume.Bubble.Text)
	}
}

func TestHoverWarningsIgnoreShortThemeColoredBubble(t *testing.T) {
	browser := newLayoutTestBrowser(t)
	page := `<!doctype html><style>
:root{background:#fff;color:#111;--ink:#111} @media(prefers-color-scheme:dark){:root{background:#111;color:#eee;--ink:#eee}}
#trigger{width:80px;height:50px}#bubble{position:fixed;left:80px;top:80px;opacity:0;pointer-events:none;background:var(--page);color:var(--ink)}
@media(prefers-color-scheme:light){#bubble{--page:#fff}} @media(prefers-color-scheme:dark){#bubble{--page:#111}}
</style><div id="trigger"></div><div id="bubble"></div><script>trigger.onpointermove=()=>{bubble.innerHTML='one<br>two<br>three<br>four<br>five';bubble.style.opacity=1}</script>`
	path := writeLayoutTestPage(t, "short-theme-bubble", page)
	for _, theme := range []string{"light", "dark"} {
		ctx, cancel := newLayoutTestTab(t, browser, path, 800, 600)
		warnings, err := checkHoverBubbles(ctx, theme, 600, filepath.Join(t.TempDir(), "short.png"))
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if len(warnings) != 0 {
			t.Fatalf("тематический bubble в пять строк должен быть тихим (%s): %#v", theme, warnings)
		}
	}
}

func TestHoverCandidateMechanismsAndOffscreenScroll(t *testing.T) {
	browser := newLayoutTestBrowser(t)
	cases := []struct {
		name, html, want string
	}{
		{"delegated", `<!doctype html><style>:root{background:#111;color:white}#chart{display:flex}.bar{width:60px;height:60px;background:#69c}#tip{position:fixed;top:5px;left:200px;background:#fff;opacity:0}</style><div id="chart"><div class="bar">a</div><div class="bar">b</div></div><div id="tip"></div><script>chart.onmousemove=e=>{if(e.target.classList.contains('bar')){tip.textContent='delegated tip';tip.style.opacity=1}}</script>`, "delegated tip"},
		{"mouseenter", `<!doctype html><style>:root{background:#111;color:white}</style><div id="trigger" style="width:80px;height:40px"></div><div id="tip" style="position:fixed;top:10px;left:100px;background:#fff;opacity:0"></div><script>trigger.onmouseenter=()=>{tip.textContent='enter tip';tip.style.opacity=1}</script>`, "enter tip"},
		{"svg listener", `<!doctype html><style>:root{background:#111;color:white}</style><svg width="200" height="80"><circle class="point" cx="40" cy="40" r="20"/></svg><div id="tip" style="position:fixed;top:5px;left:100px;background:#fff;opacity:0"></div><script>document.querySelector('circle').onmouseover=()=>{tip.textContent='svg tip';tip.style.opacity=1}</script>`, "svg tip"},
		{"below viewport", `<!doctype html><style>:root{background:#111;color:white}</style><div style="height:1100px"></div><div id="trigger" style="width:80px;height:40px"></div><div id="tip" style="position:fixed;top:5px;left:100px;background:#fff;opacity:0"></div><script>trigger.onmouseenter=()=>{tip.textContent='below tip';tip.style.opacity=1}</script>`, "below tip"},
		{"css hover", `<!doctype html><style>:root{background:#111;color:white}.zone{width:80px;height:40px}.zone .tip{visibility:hidden;position:fixed;left:100px;top:5px;background:#fff}.zone:hover .tip{visibility:visible}</style><div class="zone">zone<span class="tip">css hover tip</span></div>`, "css hover tip"},
		{"handler ahead of links", crowdHoverPage(), "crowd handler tip"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeLayoutTestPage(t, tc.name, tc.html)
			ctx, cancel := newLayoutTestTab(t, browser, path, 800, 600)
			defer cancel()
			warnings, err := checkHoverBubbles(ctx, "light", 600, filepath.Join(t.TempDir(), "probe.png"))
			if err != nil {
				t.Fatal(err)
			}
			if warningWithText(warnings, tc.want) == nil {
				description, _ := chromedp.Run(ctx, chromedp.Evaluate[string](`JSON.stringify({c:window.__pageShotHover?.nodes.map(e=>({tag:e.tagName,cls:e.getAttribute('class'),r:e.getBoundingClientRect().toJSON(),scrollY,html:e.outerHTML.slice(0,80)}))||[],rules:[...document.styleSheets].flatMap(s=>{try{return [...s.cssRules].map(r=>r.selectorText||'group')}catch(e){return ['blocked']}}),tip:document.querySelector('#tip')?.outerHTML})`))
				t.Fatalf("не найден bubble %q: %#v; diagnostics=%s", tc.want, warnings, description)
			}
		})
	}
}

func TestHoverWarningsUseInnerSurfaceAndDeduplicateStickyBubble(t *testing.T) {
	browser := newLayoutTestBrowser(t)
	wrapper := `<!doctype html><style>:root{background:#111;color:white}button{margin:8px}</style><button id="trigger">show</button><button>other</button><div id="tip" style="position:fixed;top:20px;left:180px;opacity:0"><div id="surface" style="background:#fafafa;color:#111">Light bubble</div></div><script>trigger.onmouseenter=()=>tip.style.opacity=1</script>`
	path := writeLayoutTestPage(t, "wrapper", wrapper)
	ctx, cancel := newLayoutTestTab(t, browser, path, 800, 600)
	defer cancel()
	warnings, err := checkHoverBubbles(ctx, "dark", 600, filepath.Join(t.TempDir(), "wrapper.png"))
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || warnings[0].Kind != "фон" || warnings[0].Bubble.BG != "rgb(250, 250, 250)" {
		t.Fatalf("фон должен браться с цветного потомка и предупреждение быть одно: %#v", warnings)
	}

	sticky := `<!doctype html><style>:root{background:#111;color:white}button{margin:8px}</style><button id="trigger">first</button><button>second</button><button>third</button><div id="tip" style="position:fixed;top:20px;left:180px;opacity:0;background:#fafafa;color:#111">Sticky</div><script>trigger.onmouseenter=()=>tip.style.opacity=1</script>`
	path = writeLayoutTestPage(t, "sticky", sticky)
	ctx, cancel = newLayoutTestTab(t, browser, path, 800, 600)
	defer cancel()
	warnings, err = checkHoverBubbles(ctx, "dark", 600, filepath.Join(t.TempDir(), "sticky.png"))
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 {
		t.Fatalf("непрячущийся bubble повторно не предупреждается: %#v", warnings)
	}
}

func TestHoverCandidateScanIgnoresClosedDetailsAndTimedRerender(t *testing.T) {
	browser := newLayoutTestBrowser(t)
	page := `<!doctype html><button>button</button><details><summary>closed</summary><ul><li>hidden item</li></ul></details><ul id="rerender"></ul><script>setInterval(()=>rerender.innerHTML='<li>row one</li><li>row two</li>',50)</script>`
	path := writeLayoutTestPage(t, "details-rerender", page)
	ctx, cancel := newLayoutTestTab(t, browser, path, 800, 600)
	defer cancel()
	if err := chromedp.Do(ctx, chromedp.Sleep(120*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	warnings, err := checkHoverBubbles(ctx, "light", 600, filepath.Join(t.TempDir(), "none.png"))
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("закрытый details и таймерная перерисовка не являются hover-пузырями: %#v", warnings)
	}
}

func TestDarkThemeFrameAndLayoutFindingsArePerTheme(t *testing.T) {
	alloc := newTestAllocator(t)
	page := `<!doctype html><meta charset="utf-8"><style>
html{font:16px sans-serif}.wrap{position:relative;height:120px;width:100%}
.c{position:absolute;left:0;top:70px}.d{position:absolute;left:30px;top:74px}
@media(prefers-color-scheme:dark){.d{left:400px}html{width:1200px}}
</style><div class="wrap"><span class="c">Light only gamma</span><span class="d">Light only delta</span></div>`
	path := writeLayoutTestPage(t, "per-theme-layout", page)
	out := t.TempDir()
	result, err := shoot(alloc, "file://"+path, out, viewport{800, 600, 1}, []time.Duration{0})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.themes) != 2 || result.themes[0].theme != "light" || result.themes[1].theme != "dark" {
		t.Fatalf("ожидались отдельные находки обеих тем: %#v", result.themes)
	}
	if !overlapHasPair(result.themes[0].overlap, "Light only gamma", "Light only delta") {
		t.Fatalf("светлая пара потеряна: %#v", result.themes[0].overlap)
	}
	if result.themes[1].overlap != nil || result.themes[1].overflow == nil {
		t.Fatalf("тёмная тема должна убрать пару и дать скролл: %#v", result.themes[1])
	}
	for _, file := range []string{"800x600.png", "800x600-dark.png"} {
		if _, err := os.Stat(filepath.Join(out, file)); err != nil {
			t.Fatalf("нет кадра %s: %v", file, err)
		}
	}
	if _, err := os.Stat(filepath.Join(out, "800x600-dark-full.png")); !os.IsNotExist(err) {
		t.Fatalf("полный тёмный кадр не должен сниматься, stat err=%v", err)
	}
}

func TestDarkThemePassIsSkippedWhenNoThemeSignals(t *testing.T) {
	alloc := newTestAllocator(t)
	path := writeLayoutTestPage(t, "no-dark-media", `<!doctype html><p>plain page</p>`)
	out := t.TempDir()
	result, err := shoot(alloc, "file://"+path, out, viewport{800, 600, 1}, []time.Duration{0})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.themes) != 1 || result.themes[0].theme != "light" {
		t.Fatalf("идентичная тёмная тема должна пропускаться: %#v", result.themes)
	}
	if _, err := os.Stat(filepath.Join(out, "800x600-dark.png")); !os.IsNotExist(err) {
		t.Fatalf("идентичный тёмный кадр не должен записываться: %v", err)
	}
}

func crowdHoverPage() string {
	var links strings.Builder
	for i := range 40 {
		fmt.Fprintf(&links, `<a href="#%d">link %d</a> `, i, i)
	}
	return `<!doctype html><style>:root{background:#111;color:white}</style><div>` + links.String() + `</div><div id="trigger" style="width:90px;height:35px;background:#69c"></div><div id="tip" style="position:fixed;left:300px;top:20px;opacity:0;background:#fff">crowd handler tip</div><script>trigger.addEventListener('mousemove',()=>{tip.textContent='crowd handler tip';tip.style.opacity=1})</script>`
}

func warningOfKind(warnings []hoverWarning, kind string) *hoverWarning {
	for i := range warnings {
		if warnings[i].Kind == kind {
			return &warnings[i]
		}
	}
	return nil
}

func warningWithText(warnings []hoverWarning, text string) *hoverWarning {
	for i := range warnings {
		if strings.Contains(warnings[i].Bubble.Text, text) {
			return &warnings[i]
		}
	}
	return nil
}

func overlapHasPair(result *overlapResult, a, b string) bool {
	if result == nil {
		return false
	}
	for _, pair := range result.Pairs {
		if pair.A.Text == a && pair.B.Text == b || pair.A.Text == b && pair.B.Text == a {
			return true
		}
	}
	return false
}

func writeLayoutTestPage(t *testing.T, name, html string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name+".html")
	if err := os.WriteFile(path, []byte(html), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func newLayoutTestTab(t *testing.T, browserCtx context.Context, path string, width, height int64) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := chromedp.NewContext(browserCtx)
	if err := chromedp.Do(ctx, chromedp.EmulateViewport(width, height), chromedp.Navigate("file://"+path)); err != nil {
		cancel()
		t.Fatal(err)
	}
	return ctx, cancel
}
