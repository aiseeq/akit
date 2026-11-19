package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

func TestCheckFonts(t *testing.T) {
	browserCtx := newLayoutTestBrowser(t)
	cases := []struct {
		name, html string
		count      int
		contains   string
	}{
		{"serif-heading", `<body><h2 style='font-family: Georgia'>Heading <span style='font-family:"Golos Text"'>x</span></h2></body>`, 1, `Georgia`},
		{"heading-child-family", `<body><h2>Title <span style='font-family:Georgia'>serif part</span></h2></body>`, 1, `serif part`},
		{"heading-svg-text-family", `<body><h2>Chart <svg width=120 height=30><text x=0 y=20 font-family='Georgia'>svg in h2</text></svg></h2></body>`, 1, `svg in h2`},
		{"one-element-multiple-sources", `<style>p::after{content:'Badge';font-family:Georgia}</style><body><p style='font-family:Georgia'>Label</p></body>`, 1, `Label`},
		{"approved-with-fallbacks", `<body><p style='font-family: "Golos Text", sans-serif, monospace'>Text</p><h1 style='font-family:"Russo One", serif'>Title</h1></body>`, 0, ""},
		{"serif-generic-before-approved", `<body><p style='font-family: serif, "Golos Text"'>Text</p></body>`, 1, `serif`},
		{"pseudo-before", `<style>p::before{content:'Generated';font-family:Georgia}</style><body><p>Visible</p></body>`, 1, `::before`},
		{"open-shadow-root", `<body><div id='host'></div><script>document.querySelector('#host').attachShadow({mode:'open'}).innerHTML='<p style="font-family:Georgia">Shadow</p>'</script></body>`, 1, `Georgia`},
		{"input-value", `<body><input value='Serif value' style='font-family:Georgia'></body>`, 1, `Serif value`},
		{"textarea-value", `<body><textarea style='font-family:Georgia'>Serif value</textarea></body>`, 1, `Serif value`},
		{"text-input-types", `<body><input type=text value=x style='font-family:Georgia'><input type=search value=x style='font-family:Georgia'><input type=email value=x style='font-family:Georgia'><input type=url value=x style='font-family:Georgia'><input type=tel value=x style='font-family:Georgia'><input type=number value=1 style='font-family:Georgia'><input type=password value=x style='font-family:Georgia'><input value=x style='font-family:Georgia'></body>`, 8, `Georgia`},
		{"button-input-types", `<body><input type=submit value=Send style='font-family:Georgia'><input type=button value=Click style='font-family:Georgia'></body>`, 2, `Georgia`},
		{"reset-input-button", `<body><input type=reset value=Reset style='font-family:Georgia'></body>`, 1, `Reset`},
		{"date-input-value", `<body><input type=date value='2026-11-16' style='font-family:Georgia'></body>`, 1, `2026`},
		{"non-text-input-values", `<body><input type=checkbox value='x' style='font-family:Georgia'><input type=radio value='x' style='font-family:Georgia'><input type=range value=2 style='font-family:Georgia'><input type=color value='#fff' style='font-family:Georgia'><input type=file value='x' style='font-family:Georgia'><input type=hidden value='x' style='font-family:Georgia'></body>`, 0, ""},
		{"placeholder", `<body><input placeholder='Type here' style='font-family:Georgia'></body>`, 1, `Type here`},
		{"select-option", `<body><select style='font-family:Georgia'><option>Choice</option></select></body>`, 1, `Choice`},
		{"decorative-pseudo-content", `<style>.a::before{content:'';font-family:Georgia}.b::after{content:'  ';font-family:Georgia}.c::after{content:'\00a0';font-family:Georgia}.d::before{content:url(x.png);font-family:Georgia}</style><body><p class=a>a</p><p class=b>b</p><p class=c>c</p><p class=d>d</p></body>`, 0, ""},
		{"pseudo-counter", `<style>ol{counter-reset:n}li{counter-increment:n}li::before{content:counter(n) '. ';font-family:Georgia}</style><body><ol><li>one</li></ol></body>`, 1, `::before`},
		{"collapsed-max-height", `<style>.acc{max-height:0;overflow:hidden}.acc p{font-family:Georgia}</style><body><div class=acc><p>collapsed</p></div></body>`, 0, ""},
		{"collapsed-height", `<style>.acc{height:0;overflow:hidden}.acc p{font-family:Georgia}</style><body><div class=acc><p>collapsed</p></div></body>`, 0, ""},
		{"clip-path", `<style>.sr{position:absolute;clip-path:inset(50%);font-family:Georgia}</style><body><span class=sr>clipped</span></body>`, 0, ""},
		{"clip-path-circle-zero", `<style>.clip{clip-path:circle(0)}.clip p{font-family:Georgia}</style><body><div class=clip><p>circle zero</p></div></body>`, 0, ""},
		{"clip-path-ellipse-zero", `<style>.clip{clip-path:ellipse(0 0)}.clip p{font-family:Georgia}</style><body><div class=clip><p>ellipse zero</p></div></body>`, 0, ""},
		{"clip-rect", `<style>.sr{position:absolute;clip:rect(0 0 0 0);font-family:Georgia}</style><body><span class=sr>clipped</span></body>`, 0, ""},
		{"display-contents", `<style>.dc{display:contents;font-family:Georgia}</style><body><div class=dc>contents text</div></body>`, 1, `contents text`},
		{"zero-size-overflow-visible", `<style>.lab{position:absolute;left:100px;top:100px;width:0;height:0;overflow:visible;white-space:nowrap;font-family:Georgia}</style><body><div class=lab>Chart label</div></body>`, 1, `Chart label`},
		{"tiny-text-area", `<body><span style='display:inline-block;transform:scale(.01);transform-origin:top left;font-family:Georgia'>tiny</span></body>`, 0, ""},
		{"hidden", `<body><p style='visibility:hidden;font-family:Georgia'>Hidden</p></body>`, 0, ""},
		{"sr-only", `<style>.sr{position:absolute;width:1px;height:1px;overflow:hidden;clip:rect(0 0 0 0);white-space:nowrap;font-family:Georgia}</style><body><p>Visible</p><span class=sr>screen reader</span></body>`, 0, ""},
		{"offscreen", `<body><p>Visible</p><p style='position:absolute;left:-9999px;font-family:Georgia'>offscreen</p></body>`, 0, ""},
		{"offscreen-left-with-scrollable-window-y", `<style>html{overflow-y:scroll}.sr{position:absolute;left:-9999px;font-family:Georgia}</style><body><p>Top</p><a class=sr>skip to content</a></body>`, 0, ""},
		{"offscreen-top-with-auto-window", `<style>html{overflow:auto}.sr{position:absolute;top:-9999px;font-family:Georgia}</style><body><p>Top</p><span class=sr>offscreen above</span></body>`, 0, ""},
		{"offscreen-left-with-propagated-body-auto", `<style>body{overflow:auto}.sr{position:absolute;left:-9999px;font-family:Georgia}</style><body><p>Top</p><span class=sr>offscreen left</span></body>`, 0, ""},
		{"offscreen-left-outside-hidden-body-with-auto-window", `<style>html{height:100%;overflow:auto}body{height:100%;overflow:hidden;margin:0}.sr{position:absolute;left:-9999px;top:40px;font-family:Georgia}</style><body><p>Top</p><span class=sr>offscreen left in hidden body</span></body>`, 0, ""},
		{"abs-below-reachable-by-window-y-scroll", `<style>html{overflow-y:scroll}.x{position:absolute;top:3000px;left:20px;font-family:Georgia}</style><body><p>Top</p><span class=x>abs below, window scrolls</span></body>`, 1, `abs below`},
		{"abs-right-not-reachable-by-window-y-scroll", `<style>html{overflow-y:scroll;overflow-x:hidden}.x{position:absolute;left:3000px;top:40px;font-family:Georgia}</style><body><p>Top</p><span class=x>right, x hidden</span></body>`, 0, ""},
		{"fixed-escapes-overflow", `<style>.box{overflow:hidden;height:20px}.fixed{position:fixed;top:200px;left:20px;font-family:Georgia}</style><body><div class=box><span class=fixed>Fixed toast</span></div></body>`, 1, `Fixed toast`},
		{"fixed-clipped-by-transformed-containing-block", `<style>.box{transform:translateZ(0);overflow:hidden;height:20px}.fixed{position:fixed;top:200px;left:20px;font-family:Georgia}</style><body><div class=box><p>inside</p><span class=fixed>Clipped fixed</span></div></body>`, 0, ""},
		{"window-fixed-below-viewport-on-long-page", `<style>.pad{height:3000px}.fx{position:fixed;bottom:-200px;left:20px;font-family:Georgia}</style><body><div class=pad>Top</div><span class=fx>Hidden fixed toast</span></body>`, 0, ""},
		{"window-fixed-below-hidden-viewport", `<style>html,body{overflow:hidden}.pad{height:3000px}.fx{position:fixed;bottom:-200px;left:20px;font-family:Georgia}</style><body><div class=pad>Top</div><span class=fx>Hidden fixed toast</span></body>`, 0, ""},
		{"fixed-transform-below-hidden-viewport", `<style>html,body{overflow:hidden}.t{transform:translateZ(0);margin-top:3000px}.fx{position:fixed;top:0;left:20px;font-family:Georgia}</style><body><p>Top</p><div class=t><span class=fx>Unreachable transformed fixed</span></div></body>`, 0, ""},
		{"window-fixed-panel-child-below-viewport", `<style>.fx{position:fixed;top:0;left:0;width:300px}.bad{margin-top:2000px;font-family:Georgia}</style><body><p>Top</p><div class=fx><p>panel</p><p class=bad>Below fixed panel</p></div></body>`, 0, ""},
		{"fixed-panel-transform-fixed-child-below", `<style>.fx{position:fixed;top:0;left:0;width:300px;height:200px}.t{transform:translateZ(0)}.in{position:fixed;top:2000px;left:0;font-family:Georgia}.pad{height:4000px}</style><body><div class=pad><p>Top</p></div><div class=fx><div class=t><span class=in>fixed in transform in fixed panel</span></div></div></body>`, 0, ""},
		{"fixed-in-filter-scrolls-with-page", `<style>.f{filter:blur(0);margin-top:2000px;height:100px}.fx{position:fixed;top:10px;left:20px;font-family:Georgia}</style><body><p>Top</p><div class=f><span class=fx>fixed in filter, scrolls with page</span></div><div style='height:2000px'></div></body>`, 1, `fixed in filter`},
		{"fixed-in-paint-containment-scrolls-with-page", `<style>.c{contain:paint;margin-top:2000px;height:100px}.fx{position:fixed;top:10px;left:20px;font-family:Georgia}</style><body><p>Top</p><div class=c><span class=fx>fixed in contain paint</span></div><div style='height:2000px'></div></body>`, 1, `fixed in contain paint`},
		{"absolute-in-fixed-panel-below-viewport", `<style>.fx{position:fixed;top:0;left:0;width:300px;height:100px}.ab{position:absolute;top:2000px;left:0;font-family:Georgia}.pad{height:4000px}</style><body><div class=pad><p>Top</p></div><div class=fx><span class=ab>abs inside fixed panel below</span></div></body>`, 0, ""},
		{"absolute-escapes-static-overflow", `<style>.box{overflow:hidden;height:20px}.absolute{position:absolute;top:200px;left:20px;font-family:Georgia}</style><body><div class=box><span class=absolute>Absolute label</span></div></body>`, 1, `Absolute label`},
		{"absolute-overflow-clips-own-content", `<style>.clip{position:absolute;top:20px;left:20px;width:200px;height:20px;overflow:hidden}.bad{margin-top:100px;font-family:Georgia}</style><body><div class=clip><p>inside</p><p class=bad>Clipped by absolute overflow</p></div></body>`, 0, ""},
		{"overflow-inside-absolute-clips-static-text", `<style>.outer{position:absolute;top:20px;left:20px;width:200px;height:60px}.clip{height:20px;overflow:hidden}.bad{margin-top:100px;font-family:Georgia}</style><body><div class=outer><div class=clip><p>inside</p><p class=bad>Clipped in absolute</p></div></div></body>`, 0, ""},
		{"absolute-containing-block-outside-overflow", `<style>.rel{position:relative}.o{overflow:hidden;height:20px}.ab{position:absolute;top:200px;font-family:Georgia}</style><body><div class=rel><div class=o><span class=ab>Containing block outside</span></div></div></body>`, 1, `Containing block outside`},
		{"absolute-clipped-above-relative-containing-block", `<style>.outer{overflow:hidden;height:20px}.cb{position:relative}.absolute{position:absolute;top:200px;font-family:Georgia}</style><body><div class=outer><div class=cb><p>inside</p><span class=absolute>clipped absolute</span></div></div></body>`, 0, ""},
		{"absolute-clipped-above-sticky-containing-block", `<style>.outer{overflow:hidden;height:20px}.cb{position:sticky;top:0}.absolute{position:absolute;top:200px;font-family:Georgia}</style><body><div class=outer><div class=cb><p>inside</p><span class=absolute>clipped absolute</span></div></div></body>`, 0, ""},
		{"absolute-in-fixed-chain-escapes-overflow", `<style>.o{overflow:hidden;height:20px}.fx{position:fixed;top:200px;left:20px;width:300px;height:60px}.ab{position:absolute;top:10px;left:10px;font-family:Georgia}</style><body><div class=o><p>inside</p><div class=fx><span class=ab>absolute in fixed</span></div></div></body>`, 1, `absolute in fixed`},
		{"absolute-in-absolute-chain-escapes-static-overflow", `<style>.o{overflow:hidden;height:20px}.outer{position:absolute;top:200px;left:20px;width:300px;height:60px}.ab{position:absolute;top:10px;left:10px;font-family:Georgia}</style><body><div class=o><p>inside</p><div class=outer><span class=ab>absolute in absolute</span></div></div></body>`, 1, `absolute in absolute`},
		{"body-overflow-below-fold", `<style>html,body{height:100%}body{overflow-x:hidden}.below{margin-top:3000px;font-family:Georgia}</style><body><p>Top</p><p class=below>Far below</p></body>`, 1, `Far below`},
		{"hidden-html-body-overflow-below-fold", `<style>html,body{height:100%;overflow:hidden}.below{margin-top:3000px;font-family:Georgia}</style><body><p>Top</p><p class=below>Unreachable</p></body>`, 0, ""},
		{"body-only-overflow-hidden-below-fold", `<style>body{overflow:hidden}.below{margin-top:3000px;font-family:Georgia}</style><body><p>Top</p><p class=below>Body hidden</p></body>`, 0, ""},
		{"html-only-overflow-hidden-below-fold", `<style>html{overflow:hidden}.below{margin-top:3000px;font-family:Georgia}</style><body><p>Top</p><p class=below>HTML hidden</p></body>`, 0, ""},
		{"body-scroll-container-below-fold", `<style>html{height:100%;overflow:hidden}body{height:100%;overflow:auto}.below{margin-top:3000px;font-family:Georgia}</style><body><p>Top</p><p class=below>Scrollable body text</p></body>`, 1, `Scrollable body text`},
		{"body-auto-no-scroll-with-hidden-window", `<style>html{height:100%;overflow:hidden}body{overflow:auto}.below{margin-top:3000px;font-family:Georgia}</style><body><p>Top</p><p class=below>Body grows without scrolling</p></body>`, 0, ""},
		{"auto-container-that-grows-does-not-scroll", `<style>html{height:100%;overflow:hidden}.scroll{overflow:auto}.bad{margin-top:3000px;font-family:Georgia}</style><body><div class=scroll><p>Top</p><p class=bad>Auto container grows with content</p></div></body>`, 0, ""},
		{"body-scroll-overflow-reaches-content", `<style>html{height:100%;overflow:hidden}body{height:100%;overflow:scroll}.below{margin-top:3000px;font-family:Georgia}</style><body><p>Top</p><p class=below>Body scrolls vertically</p></body>`, 1, `Body scrolls vertically`},
		{"body-horizontal-scroll-reaches-content", `<style>html,body{height:100%;margin:0}body{overflow-x:scroll}.outside{position:absolute;left:3000px;top:20px;font-family:Georgia}</style><body><p>Top</p><span class=outside>Body scrolls horizontally</span></body>`, 1, `Body scrolls horizontally`},
		{"body-own-x-scroller-reachable-with-hidden-html", `<style>html{overflow:hidden}body{margin:0;overflow-x:auto}.wide{width:3000px;white-space:nowrap}.g{margin-left:2000px;font-family:Georgia}</style><body><p>Top</p><div class=wide><span class=g>reach by body x scroll</span></div></body>`, 1, `reach by body x scroll`},
		{"body-auto-not-scrolling-with-hidden-html", `<style>html{overflow:hidden}body{margin:0;height:200px;overflow-y:auto}.ab{position:absolute;top:3000px;left:20px;font-family:Georgia}</style><body><p>Top</p><span class=ab>unreachable absolute</span></body>`, 0, ""},
		{"body-hidden-with-scrolling-window", `<style>html{height:100%;overflow:auto}body{height:100%;overflow:hidden}.below{margin-top:3000px;font-family:Georgia}</style><body><p>Top</p><p class=below>Body clips</p></body>`, 0, ""},
		{"nested-scroller-text-reachable", `<style>.scroll{height:20px;overflow:auto}.bad{font-family:Georgia}</style><body><div class=scroll><p>Top</p><p class=bad>Reachable below</p></div></body>`, 1, `Reachable below`},
		{"horizontal-scroller-text-reachable", `<style>.scroll{width:60px;overflow-x:auto;white-space:nowrap}.bad{font-family:Georgia}</style><body><div class=scroll><span class=bad>Reachable to the right by scrolling</span></div></body>`, 1, `Reachable to the right`},
		{"horizontal-scrollport-through-hidden-parent", `<style>.outer{width:100px;overflow:hidden}.scroll{width:50px;overflow-x:auto;white-space:nowrap}.bad{display:block;margin-left:150px;font-family:Georgia}</style><body><div class=outer><div class=scroll><span class=bad>Reachable past parent edge</span></div></div></body>`, 1, `Reachable past parent edge`},
		{"hidden-box-inside-x-scroller", `<style>.sc{overflow-x:auto;width:300px}.wide{width:600px}.h{overflow:hidden;width:100px;white-space:nowrap}.g{display:inline-block;margin-left:200px;font-family:Georgia}</style><body><p>Top</p><div class=sc><div class=wide><div class=h><span class=g>clipped by hidden box</span></div></div></div></body>`, 0, ""},
		{"clip-overflow-cuts-off-y", `<style>.clip{height:20px;overflow:clip}.bad{margin-top:100px;font-family:Georgia}</style><body><div class=clip><p>Top</p><p class=bad>Clipped vertically</p></div></body>`, 0, ""},
		{"clip-overflow-cuts-off-x", `<style>.clip{width:100px;height:40px;overflow-x:clip;white-space:nowrap}.bad{display:block;position:relative;left:150px;font-family:Georgia}</style><body><div class=clip><span>Visible</span><span class=bad>Clipped horizontally</span></div></body>`, 0, ""},
		{"app-shell-scroller-inside-hidden-window", `<style>html,body{height:100%;overflow:hidden;margin:0}main{height:100%;overflow:auto}.bad{font-family:Georgia}</style><body><main><p>Top</p><p style='margin-top:2000px' class=bad>Reachable by main scroll</p></main></body>`, 1, `Reachable by main scroll`},
		{"scroller-area-clipped-by-hidden-parent", `<style>.outer{height:100px;overflow:hidden}.inner{height:50px;overflow:auto}.bad{font-family:Georgia}</style><body><div class=outer><div class=inner><p>a</p><p>b</p><p>c</p><p class=bad>Reachable by inner scroll</p></div></div></body>`, 1, `Reachable by inner scroll`},
		{"collapsed-auto-scroller", `<style>.acc{max-height:0;overflow:auto}.acc p{font-family:Georgia}</style><body><p>Top</p><div class=acc><p>Collapsed</p></div></body>`, 0, ""},
		{"zero-height-scroll-scroller", `<style>.acc{height:0;overflow:scroll}.acc p{font-family:Georgia}</style><body><p>Top</p><div class=acc><p>Collapsed</p></div></body>`, 0, ""},
		{"body-overflow-hidden-fixed-footer", `<style>body{overflow:hidden}.fx{position:fixed;bottom:10px;left:10px;font-family:Georgia}</style><body><p>Short</p><span class=fx>Fixed footer</span></body>`, 1, `Fixed footer`},
		{"body-overflow-hidden-absolute-first-screen", `<style>body{overflow:hidden}.ab{position:absolute;top:400px;left:20px;font-family:Georgia}</style><body><p>Short</p><span class=ab>Absolute in first screen</span></body>`, 1, `Absolute in first screen`},
		{"body-overflow-y-hidden-horizontal-content", `<style>body{overflow-y:hidden}.outside{position:absolute;left:3000px;font-family:Georgia}</style><body><p>Top</p><span class=outside>Horizontal scroll remains</span></body>`, 1, `Horizontal scroll remains`},
		{"window-fixed-right-outside-viewport-wide-document", `<style>.pad{height:100px;width:4000px}.fx{position:fixed;top:40px;left:2000px;font-family:Georgia}</style><body><div class=pad>Top</div><span class=fx>fixed right of window</span></body>`, 0, ""},
		{"narrow-body-own-x-scroller", `<style>html{overflow:hidden}body{margin:0;width:300px;overflow-x:auto}.wide{width:1000px;white-space:nowrap}.g{display:inline-block;margin-left:500px;font-family:Georgia}</style><body><p>Top</p><div class=wide><span class=g>reach by narrow body scroll</span></div></body>`, 1, `narrow body scroll`},
		{"absolute-escapes-x-hidden-body", `<style>html{overflow:auto}body{margin:0;width:300px;overflow-x:hidden}.ab{position:absolute;left:310px;top:40px;font-family:Georgia}</style><body><p>Top</p><span class=ab>escapes body x clip</span></body>`, 1, `escapes body x clip`},
		{"auto-x-container-grows-under-hidden-window", `<style>html{overflow-x:hidden}.sc{overflow-x:auto;width:max-content}.g{display:inline-block;margin-left:2000px;white-space:nowrap;font-family:Georgia}</style><body><p>Top</p><div class=sc><span class=g>auto x container grows</span></div></body>`, 0, ""},
		{"body-hidden-clips-horizontal-when-html-scrolls", `<style>html{overflow-x:auto}body{width:100px;overflow-x:hidden}.bad{display:block;position:relative;left:200px;font-family:Georgia}</style><body><span>content</span><span class=bad>Clipped by body</span></body>`, 0, ""},
		{"body-hidden-x-propagates-to-visible-window", `<style>body{width:100px;height:100px;overflow-x:hidden}.bad{display:block;position:relative;left:300px;font-family:Georgia}</style><body><span>body box is not clip</span><span class=bad>Visible outside body box</span></body>`, 1, `Visible outside body box`},
		{"body-hidden-y-propagates-to-visible-window", `<style>body{width:400px;height:40px;overflow-y:hidden}.bad{display:block;position:relative;top:200px;font-family:Georgia}</style><body><span>body box is not clip</span><span class=bad>Visible below body box</span></body>`, 1, `Visible below body box`},
		{"absolute-escapes-hidden-body-with-auto-window", `<style>html{height:100%;overflow:auto}body{height:100%;overflow:hidden;margin:0}.bad{position:absolute;top:3000px;left:20px;font-family:Georgia}</style><body><p>Top</p><span class=bad>Absolute outside body</span></body>`, 1, `Absolute outside body`},
		{"polygon-mixed-units", `<style>.o{clip-path:polygon(0 0,50% 50%,50px 50px);height:200px}.o p{margin:0;font-family:Georgia}</style><body><div class=o><p>Mixed polygon units</p></div></body>`, 1, `Mixed polygon units`},
		{"polygon-evenodd-collinear", `<style>.o{clip-path:polygon(evenodd,0 0,50% 50%,100% 100%)}.o p{font-family:Georgia}</style><body><div class=o><p>Collinear polygon</p></div></body>`, 0, ""},
		{"horizontal-window-overflow-hidden", `<style>html{overflow-x:hidden}.outside{position:absolute;left:3000px;font-family:Georgia}</style><body><p>Top</p><p class=outside>Outside viewport</p></body>`, 0, ""},
		{"horizontal-body-overflow-hidden", `<style>html,body{width:4000px}body{overflow-x:hidden}.outside{position:absolute;left:3000px;font-family:Georgia}</style><body><p>Top</p><p class=outside>Outside body viewport</p></body>`, 0, ""},
		{"horizontal-window-overflow-clip", `<style>html{overflow-x:clip}.outside{position:absolute;left:3000px;font-family:Georgia}</style><body><p>Top</p><p class=outside>Clipped viewport</p></body>`, 0, ""},
		{"html-overflow-y-clip", `<style>html{overflow-y:clip}.outside{margin-top:3000px;font-family:Georgia}</style><body><p>Top</p><p class=outside>Clipped below fold</p></body>`, 0, ""},
		{"ellipse-one-zero-radius", `<style>.clip{clip-path:ellipse(0 20px)}.clip p{font-family:Georgia}</style><body><div class=clip><p>Empty ellipse</p></div></body>`, 0, ""},
		{"polygon-zero-area", `<style>.clip{clip-path:polygon(0 0,0 0,0 0)}.clip p{font-family:Georgia}</style><body><div class=clip><p>Empty polygon</p></div></body>`, 0, ""},
		{"opacity-zero", `<body><p style='opacity:0;font-family:Georgia'>Transparent</p></body>`, 0, ""},
		{"visible-child-overrides-hidden", `<style>.parent{visibility:hidden}.child{visibility:visible;font-family:Georgia}</style><body><div class=parent>hidden <span class=child>shown child</span></div></body>`, 1, `shown child`},
		{"pseudo-zero-size-host", `<style>.z{position:relative;display:block;width:0;height:0}.z::after{content:'Badge';position:absolute;left:0;top:0;white-space:nowrap;font-family:Georgia}</style><body><div class=z></div></body>`, 1, `Badge`},
		{"pseudo-zero-host-shifted-left-and-measured-width", `<style>.z{position:relative;display:block;width:0;height:0;left:-1000px;top:40px}.z::after{content:'A long generated label';position:absolute;left:970px;top:10px;white-space:nowrap;font-family:Georgia}</style><body><div class=z></div></body>`, 1, `generated label`},
		{"pseudo-zero-host-shifted-top", `<style>.z{position:relative;display:block;width:0;height:0;left:40px;top:-1000px}.z::after{content:'A long generated label';position:absolute;left:10px;top:1020px;white-space:nowrap;font-family:Georgia}</style><body><div class=z></div></body>`, 1, `generated label`},
		{"contain-paint-clips", `<style>.paint{contain:paint;height:20px}.paint p.bad{font-family:Georgia}</style><body><div class=paint><p>inside</p><p class=bad>below paint containment</p></div></body>`, 0, ""},
		{"contain-content-clips", `<style>.paint{contain:content;height:20px}.paint p.bad{font-family:Georgia}</style><body><div class=paint><p>inside</p><p class=bad>below content containment</p></div></body>`, 0, ""},
		{"contain-strict-clips", `<style>.paint{contain:strict;height:20px}.paint p.bad{font-family:Georgia}</style><body><div class=paint><p>inside</p><p class=bad>below strict containment</p></div></body>`, 0, ""},
		{"closed-details-content", `<body><details><summary>Summary</summary><p style='font-family:Georgia'>Closed content</p></details></body>`, 0, ""},
		{"body-own-x-scroll-with-margin", `<!doctype html><style>html{overflow:hidden}body{margin:0 0 0 300px;overflow-x:auto}.wide{width:3000px;height:40px;position:relative}.g{position:absolute;right:0;top:0;font-family:Georgia}</style><body><div class=wide><span class=g>body scroll right end</span></div></body>`, 1, `body scroll right end`},
		{"body-own-y-scroll-with-margin", `<!doctype html><style>html{overflow:hidden}body{margin:300px 0 0;height:200px;overflow-y:auto}.tall{height:3000px;position:relative}.g{position:absolute;bottom:0;left:20px;font-family:Georgia}</style><body><div class=tall><span class=g>body scroll bottom end</span></div></body>`, 1, `body scroll bottom end`},
		{"fixed-footer-with-short-scrolling-body", `<!doctype html><style>html{overflow:hidden}body{margin:0;height:300px;overflow-y:auto}.c{height:400px}footer{position:fixed;left:0;right:0;bottom:0;height:40px;font-family:Georgia}</style><body><div class=c>content</div><footer>fixed footer on screen</footer></body>`, 1, `fixed footer on screen`},
		{"body-auto-below-hidden-window-does-not-scroll", `<!doctype html><style>html{overflow:hidden}body{margin:2000px 0 0;height:100px;overflow-y:auto}.g{font-family:Georgia}</style><body><p class=g>body box below the window</p></body>`, 0, ""},
		{"body-scroll-origin-x-in-document", `<!doctype html><style>html{overflow:hidden}body{margin:0 0 0 300px;width:400px;overflow-x:auto}.wide{width:600px;height:40px}.ab{position:absolute;left:650px;top:60px;font-family:Georgia}</style><body><p>Top</p><div class=wide></div><span class=ab>abs past scroll width</span></body>`, 1, `abs past scroll width`},
		{"body-scroll-origin-y-in-document", `<!doctype html><style>html{overflow:hidden}body{margin:300px 0 0;height:200px;overflow-y:auto}.tall{height:300px}.ab{position:absolute;top:350px;left:20px;font-family:Georgia}</style><body><div class=tall>Top</div><span class=ab>abs past scroll height</span></body>`, 1, `abs past scroll height`},
		{"quirks-window-scroll-x-only-absolute-right", `<style>body{font-family:"Golos Text",sans-serif}</style><script>window.Gazette={}</script><style>html{height:100%;overflow-x:auto;overflow-y:hidden}body{height:100%;overflow:hidden;margin:0}.bad{position:absolute;left:3000px;top:20px;font-family:Georgia}</style><body><p>Top</p><span class=bad>Absolute right, window x only</span></body>`, 1, `Absolute right, window x only`},
		{"quirks-window-scroll-y-only-absolute-below", `<style>body{font-family:"Golos Text",sans-serif}</style><script>window.Gazette={}</script><style>html{height:100%;overflow-x:hidden;overflow-y:auto}body{height:100%;overflow:hidden;margin:0}.bad{position:absolute;top:3000px;left:20px;font-family:Georgia}</style><body><p>Top</p><span class=bad>Absolute below, window y only</span></body>`, 1, `Absolute below, window y only`},
		{"one-pixel-screenreader-text-remains-hidden", `<!doctype html><style>html{overflow-y:scroll}.sr{position:absolute;width:1px;height:1px;overflow:hidden;white-space:nowrap;font-family:Georgia}</style><body><p>Top</p><span class=sr>one pixel visually hidden</span></body>`, 0, ""},
		{"fixed-header-body-scroll-last-line", `<!doctype html><style>html{height:100%;overflow:hidden}header{position:fixed;top:0;left:0;right:0;height:60px}body{margin:60px 0 0;height:calc(100% - 60px);overflow:auto}.c{height:2000px}.last{font-family:Georgia;margin:0}</style><body><header>Head</header><div class=c>content</div><p class=last>body scroll last line</p></body>`, 1, `body scroll last line`},
		{"absolute-escapes-scrolling-body-x-html-hidden", `<!doctype html><style>html{height:100%;overflow:hidden}body{margin:0;height:100%;overflow:auto}.wide{width:3000px;height:40px}.ab{position:absolute;left:2000px;top:60px;font-family:Georgia}</style><body><div class=wide></div><span class=ab>absolute x beyond window</span></body>`, 0, ""},
		{"absolute-escapes-scrolling-body-y-html-hidden", `<!doctype html><style>html{height:100%;overflow:hidden}body{margin:0;height:100%;overflow:auto}.tall{height:3000px}.ab{position:absolute;top:2000px;left:20px;font-family:Georgia}</style><body><div class=tall></div><span class=ab>absolute y beyond window</span></body>`, 0, ""},
		{"quirks-absolute-right-body-overflow-auto", `<style>body{font-family:"Golos Text",sans-serif}</style><script>window.Gazette={}</script><style>html{height:100%;overflow:auto}body{height:100%;overflow:hidden;margin:0}.bad{position:absolute;left:3000px;top:20px;font-family:Georgia}</style><body><p>Top</p><span class=bad>quirks absolute right</span></body>`, 1, `quirks absolute right`},
		{"quirks-absolute-left-and-below", `<style>body{font-family:"Golos Text",sans-serif}</style><script>window.Gazette={}</script><style>html{height:100%;overflow:auto}body{height:100%;overflow:hidden;margin:0}.bad{position:absolute;left:-9999px;top:3000px;font-family:Georgia}</style><body><p>Top</p><span class=bad>quirks left and below</span></body>`, 0, ""},
		{"quirks-absolute-above-and-right", `<style>body{font-family:"Golos Text",sans-serif}</style><script>window.Gazette={}</script><style>html{height:100%;overflow:auto}body{height:100%;overflow:hidden;margin:0}.bad{position:absolute;left:3000px;top:-9999px;font-family:Georgia}</style><body><p>Top</p><span class=bad>quirks above and right</span></body>`, 0, ""},
		{"plain-doctype-absolute-below-document", `<!doctype html><style>.bad{position:absolute;top:3000px;left:20px;font-family:Georgia}</style><body><p>Top</p><span class=bad>plain absolute below</span></body>`, 1, `plain absolute below`},
		{"plain-doctype-absolute-right-of-document", `<!doctype html><style>.bad{position:absolute;left:3000px;top:40px;font-family:Georgia}</style><body><p>Top</p><span class=bad>plain absolute right</span></body>`, 1, `plain absolute right`},
		{"plain-quirks-absolute-below-document", `<style>.bad{position:absolute;top:3000px;left:20px;font-family:Georgia}</style><body><p>Top</p><span class=bad>quirks absolute below</span></body>`, 1, `quirks absolute below`},
		{"plain-quirks-absolute-right-of-document", `<style>.bad{position:absolute;left:3000px;top:40px;font-family:Georgia}</style><body><p>Top</p><span class=bad>quirks absolute right</span></body>`, 1, `quirks absolute right`},
		{"x-scroller-cannot-reach-before-origin", `<style>.sc{position:relative;overflow-x:auto;width:300px}.wide{width:600px;height:40px}.bad{position:absolute;left:-9999px;top:0;font-family:Georgia}</style><body><p>Top</p><div class=sc><div class=wide></div><span class=bad>before x scroll origin</span></div></body>`, 0, ""},
		{"y-scroller-cannot-reach-before-origin", `<!doctype html><style>.sc{position:relative;overflow-y:auto;height:300px}.tall{height:1000px}.bad{position:absolute;top:-40px;left:0;font-family:Georgia}</style><body><p>Top</p><div class=sc><span class=bad>skip above origin</span><div class=tall>content</div></div></body>`, 0, ""},
		{"quirks-y-scroller-cannot-reach-before-origin", `<html><head><style>.sc{position:relative;overflow-y:auto;height:300px}.tall{height:1000px}.bad{position:absolute;top:-40px;left:0;font-family:Georgia}</style></head><body><p>Top</p><div class=sc><span class=bad>quirks skip above origin</span><div class=tall>content</div></div></body>`, 0, ""},
		{"transform-before-origin-in-x-scroller", `<style>.sc{overflow-x:auto;width:300px}.wide{width:600px;height:40px}.bad{transform:translateX(-2000px);font-family:Georgia;white-space:nowrap}</style><body><p>Top</p><div class=sc><div class=wide></div><div class=bad>translated before origin</div></div></body>`, 0, ""},
		{"reverse-column-scroller-reaches-old-content", `<style>.sc{display:flex;flex-direction:column-reverse;overflow-y:auto;height:200px}.m{flex:none;height:60px}.bad{font-family:Georgia}</style><body><p>Top</p><div class=sc><div class=m>new</div><div class=m>2</div><div class=m>3</div><div class=m>4</div><div class=m>5</div><div class='m bad'>old reachable</div></div></body>`, 1, `old reachable`},
		{"content-visibility-hidden-text", `<style>.cv{content-visibility:hidden;height:100px}.bad{font-family:Georgia}</style><body><p>Top</p><div class=cv><p class=bad>not rendered</p></div></body>`, 0, ""},
		{"scrolled-y-scroller-first-message-reachable", `<!doctype html><style>.sc{overflow-y:auto;height:200px}.m{height:60px;margin:0}.bad{font-family:Georgia}</style><body><p>Top</p><div class=sc id=s><p class='m bad'>first message</p><p class=m>m</p><p class=m>m</p><p class=m>m</p><p class=m>m</p><p class=m>m</p><p class=m>m</p><p class=m>m</p><p class=m>m</p></div><script>s.scrollTop=s.scrollHeight</script></body>`, 1, `first message`},
		{"scrolled-x-scroller-first-slide-reachable", `<!doctype html><style>.sc{overflow-x:auto;width:300px;white-space:nowrap}.sl{display:inline-block;width:280px;height:40px}.bad{font-family:Georgia}</style><body><p>Top</p><div class=sc id=s><span class='sl bad'>first slide</span><span class=sl>s</span><span class=sl>s</span><span class=sl>s</span><span class=sl>s</span><span class=sl>s</span></div><script>s.scrollLeft=1200</script></body>`, 1, `first slide`},
		{"rtl-scroller-left-end-reachable", `<!doctype html><style>.sc{direction:rtl;overflow-x:auto;width:300px;white-space:nowrap}.sl{display:inline-block;width:280px;height:40px}.bad{font-family:Georgia}</style><body><p>Top</p><div class=sc><span class=sl>s</span><span class=sl>s</span><span class=sl>s</span><span class=sl>s</span><span class=sl>s</span><span class='sl bad'>last slide far left</span></div></body>`, 1, `last slide far left`},
		{"row-reverse-scroller-left-end-reachable", `<!doctype html><style>.sc{display:flex;flex-direction:row-reverse;overflow-x:auto;width:300px}.it{flex:none;width:280px;height:40px}.bad{font-family:Georgia}</style><body><p>Top</p><div class=sc><div class=it>i</div><div class=it>i</div><div class=it>i</div><div class=it>i</div><div class=it>i</div><div class='it bad'>last item left</div></div></body>`, 1, `last item left`},
		{"inline-flex-reverse-scroller-reaches-oldest", `<!doctype html><style>.sc{display:inline-flex;flex-direction:column-reverse;overflow-y:auto;height:200px;width:300px}.m{flex:none;height:60px}.bad{font-family:Georgia}</style><body><p>Top</p><div class=sc><div class=m>newest</div><div class=m>m</div><div class=m>m</div><div class=m>m</div><div class=m>m</div><div class='m bad'>oldest message</div></div></body>`, 1, `oldest message`},
		{"reverse-scroller-scrolled-reaches-newest", `<!doctype html><style>.sc{display:flex;flex-direction:column-reverse;overflow-y:auto;height:200px}.m{flex:none;height:60px}.bad{font-family:Georgia}</style><body><p>Top</p><div class=sc id=s><div class='m bad'>newest message</div><div class=m>m</div><div class=m>m</div><div class=m>m</div><div class=m>m</div><div class=m>m</div><div class=m>m</div></div><script>s.scrollTop=-s.scrollHeight</script></body>`, 1, `newest message`},
		{"smooth-scroller-keeps-current-position-in-range", `<!doctype html><style>.sc{overflow-y:auto;scroll-behavior:smooth;height:100px}.m{height:60px}.bad{font-family:Georgia}</style><body><div class=sc id=s><p class=bad>first message</p><p class=m>m</p><p class=m>m</p><p class=m>m</p></div><script>s.scrollTo({top:1e6,behavior:'instant'})</script></body>`, 1, `first message`},
		{"ltr-scroller-negative-scrollleft-does-not-move-page", `<!doctype html><html><head><meta charset="utf-8"><style>body{font-family:"Golos Text",sans-serif;font-size:16px}.sc{overflow-x:auto;width:300px;white-space:nowrap}.sl{display:inline-block;width:280px;height:40px}.g{font-family:Georgia}</style><script>window.Gazette={}</script></head><body><p>Top</p><div class=sc id=s data-sc><span class="sl g">first slide</span><span class=sl>s</span><span class=sl>s</span><span class=sl>s</span><span class=sl>s</span><span class=sl>s</span><span class=sl>s</span></div><script>s.scrollLeft=-500</script></body></html>`, 1, `first slide`},
		{"smooth-rtl-scroller-has-leftward-range", `<!doctype html><style>.sc{direction:rtl;overflow-x:auto;scroll-behavior:smooth;width:200px;white-space:nowrap}.item{display:inline-block;width:120px}.bad{font-family:Georgia}</style><body><div class=sc><span class=item>a</span><span class=item>b</span><span class='item bad'>last item</span></div></body>`, 1, `last item`},
		{"smooth-row-reverse-scroller-has-leftward-range", `<!doctype html><style>.sc{display:flex;flex-direction:row-reverse;overflow-x:auto;scroll-behavior:smooth;width:200px}.item{flex:none;width:120px}.bad{font-family:Georgia}</style><body><div class=sc><span class=item>a</span><span class=item>b</span><span class='item bad'>last item</span></div></body>`, 1, `last item`},
		{"smooth-column-reverse-scroller-has-upward-range", `<!doctype html><style>.sc{display:flex;flex-direction:column-reverse;overflow-y:auto;scroll-behavior:smooth;height:100px}.item{flex:none;height:60px}.bad{font-family:Georgia}</style><body><div class=sc><span class=item>a</span><span class=item>b</span><span class='item bad'>oldest message</span></div></body>`, 1, `oldest message`},
		{"rtl-row-flex-scroller-reaches-last-item", `<!doctype html><html><head><meta charset="utf-8"><style>body{font-family:"Golos Text",sans-serif;font-size:16px}.sc{display:flex;overflow-x:auto;width:300px;height:60px;direction:rtl}.it{flex:none;width:280px;height:40px}.g{font-family:Georgia}</style><script>window.Gazette={}</script></head><body><p>Top</p><div class=sc data-sc><div class=it>i</div><div class=it>i</div><div class=it>i</div><div class=it>i</div><div class=it>i</div><div class="it g">last item</div></div></body></html>`, 1, `last item`},
		{"row-wrap-reverse-y-scroller-reaches-last-line", `<!doctype html><html><head><meta charset="utf-8"><style>body{font-family:"Golos Text",sans-serif;font-size:16px}.sc{display:flex;flex-wrap:wrap-reverse;overflow-y:auto;height:200px;width:300px}.it{flex:none;width:280px;height:60px}.g{font-family:Georgia}</style><script>window.Gazette={}</script></head><body><p>Top</p><div class=sc data-sc><div class=it>i</div><div class=it>i</div><div class=it>i</div><div class=it>i</div><div class=it>i</div><div class=it>i</div><div class="it g">last line</div></div></body></html>`, 1, `last line`},
		{"column-wrap-reverse-x-scroller-reaches-last-column", `<!doctype html><html><head><meta charset="utf-8"><style>body{font-family:"Golos Text",sans-serif;font-size:16px}.sc{display:flex;flex-direction:column;flex-wrap:wrap-reverse;overflow-x:auto;width:300px;height:120px}.it{flex:none;width:200px;height:100px}.g{font-family:Georgia}</style><script>window.Gazette={}</script></head><body><p>Top</p><div class=sc data-sc><div class=it>i</div><div class=it>i</div><div class=it>i</div><div class=it>i</div><div class=it>i</div><div class="it g">last column</div></div></body></html>`, 1, `last column`},
		{"sideways-lr-y-scroller-reaches-inline-end", `<!doctype html><html><head><meta charset="utf-8"><style>body{font-family:"Golos Text",sans-serif;font-size:16px}.sc{writing-mode:sideways-lr;overflow-y:auto;height:200px;width:300px}.ln{display:block;white-space:nowrap}.g{font-family:Georgia}</style><script>window.Gazette={}</script></head><body><p>Top</p><div class=sc data-sc><span class=ln>word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word <span class=g>inline end</span></span></div></body></html>`, 1, `inline end`},
		{"vertical-rl-y-scroller-follows-top-to-bottom-inline-flow", `<!doctype html><style>.sc{writing-mode:vertical-rl;overflow-y:auto;height:200px;width:300px}.ln{white-space:nowrap}.bad{font-family:Georgia}</style><body><div class=sc><span class=ln>word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word <span class=bad>inline end</span></span></div></body>`, 1, `inline end`},
		{"sideways-rl-x-scroller-reaches-left-columns", `<!doctype html><style>.sc{writing-mode:sideways-rl;overflow-x:auto;width:300px;height:200px}.col{display:block;width:80px}.bad{font-family:Georgia}</style><body><p>Top</p><div class=sc><div class=col>c</div><div class=col>c</div><div class=col>c</div><div class=col>c</div><div class=col>c</div><div class="col bad">far left column</div></div></body>`, 1, `far left column`},
		{"row-reverse-does-not-reverse-cross-axis", `<!doctype html><style>.sc{display:flex;flex-direction:row-reverse;flex-wrap:wrap;overflow-y:auto;height:100px;width:300px}.it{flex:none;width:280px;height:60px}.bad{font-family:Georgia}</style><body><div class=sc><div class=it>a</div><div class=it>b</div><div class=it>c</div><div class=it>d</div><div class="it bad">last row</div></div></body>`, 1, `last row`},
		{"column-reverse-flex-end-unreachable-first-item", `<!doctype html><html><head><meta charset="utf-8"><style>body{font-family:"Golos Text",sans-serif;font-size:16px}.sc{display:flex;flex-direction:column-reverse;overflow-y:auto;height:200px;width:300px;justify-content:flex-end}.it{flex:none;height:60px}.g{font-family:Georgia}</style><script>window.Gazette={}</script></head><body><p>Top</p><div class=sc data-sc><div class="it g">first item</div><div class=it>i</div><div class=it>i</div><div class=it>i</div><div class=it>i</div><div class=it>i</div><div class=it>i</div></div></body></html>`, 0, ""},
		{"column-reverse-safe-flex-end-unreachable-first-item", `<!doctype html><html><head><meta charset="utf-8"><style>body{font-family:"Golos Text",sans-serif;font-size:16px}.sc{display:flex;flex-direction:column-reverse;overflow-y:auto;height:200px;width:300px;justify-content:safe flex-end}.it{flex:none;height:60px}.g{font-family:Georgia}</style><script>window.Gazette={}</script></head><body><p>Top</p><div class=sc data-sc><div class="it g">first item</div><div class=it>i</div><div class=it>i</div><div class=it>i</div><div class=it>i</div><div class=it>i</div><div class=it>i</div></div></body></html>`, 0, ""},
		{"row-reverse-flex-end-unreachable-first-item", `<!doctype html><style>.sc{display:flex;flex-direction:row-reverse;overflow-x:auto;width:200px;height:100px;justify-content:flex-end}.it{flex:none;width:60px;height:60px}.bad{font-family:Georgia}</style><body><p>Top</p><div class=sc><div class="it bad">first item</div><div class=it>i</div><div class=it>i</div><div class=it>i</div><div class=it>i</div><div class=it>i</div><div class=it>i</div></div></body>`, 0, ""},
		{"y-scroller-abs-bottom-100-unreachable", `<!doctype html><html><head><meta charset="utf-8"><style>body{font-family:"Golos Text",sans-serif;font-size:16px}.sc{position:relative;overflow-y:auto;height:200px;width:300px}.tall{height:1000px}.g{position:absolute;bottom:100%;left:0;font-family:Georgia}</style><script>window.Gazette={}</script></head><body><p style="margin:0;height:40px;line-height:20px">Top</p><div class=sc data-sc><div class=tall>content</div><span class=g>just above y origin</span></div></body></html>`, 0, ""},
		{"smooth-column-reverse-chat-at-top-origin", `<!doctype html><html><head><meta charset="utf-8"><style>body{font-family:"Golos Text",sans-serif;font-size:16px}.sc{display:flex;flex-direction:column-reverse;overflow-y:auto;height:200px;scroll-behavior:smooth}.m{flex:none;height:60px}.g{font-family:Georgia}</style><script>window.Gazette={}</script></head><body><p>Top</p><div class=sc data-sc><div class=m>newest</div><div class=m>m</div><div class=m>m</div><div class=m>m</div><div class=m>m</div><div class="m g">oldest message</div></div></body></html>`, 1, `oldest message`},
		{"scaled-bordered-x-scrollport-rejects-border-text", `<!doctype html><style>.z{transform:scale(2);transform-origin:0 0}.sc{position:relative;overflow-x:auto;width:300px;padding:0 40px;border-left:30px solid}.wide{width:600px;height:40px}.bad{position:absolute;left:-60px;top:0;width:50px;overflow:hidden;white-space:nowrap;font-family:Georgia}</style><body><div class=z><div class=sc><div class=wide></div><span class=bad>under left border</span></div></div></body>`, 0, ""},
		{"scaled-border-scroll-offset-is-converted-to-screen-pixels", `<!doctype html><style>.z{transform:scale(2);transform-origin:0 0}.sc{overflow-x:auto;width:300px;white-space:nowrap}.item{display:inline-block;width:120px}.bad{font-family:Georgia}</style><body><div class=z><div class=sc id=s><span class='item bad'>first slide</span><span class=item>a</span><span class=item>b</span><span class=item>c</span></div></div><script>s.scrollLeft=200</script></body>`, 1, `first slide`},
		{"scaled-x-scroller-scrolled-first-slide", `<!doctype html><html><head><meta charset="utf-8"><style>body{font-family:"Golos Text",sans-serif;font-size:16px}.z{transform:scale(2);transform-origin:0 0}.sc{overflow-x:auto;width:300px;white-space:nowrap}.sl{display:inline-block;width:280px;height:40px}.g{font-family:Georgia}</style><script>window.Gazette={}</script></head><body><p>Top</p><div class=z><div class=sc id=s data-sc><span class="sl g">first slide</span><span class=sl>s</span><span class=sl>s</span><span class=sl>s</span><span class=sl>s</span><span class=sl>s</span><span class=sl>s</span></div></div><script>s.scrollLeft=900</script></body></html>`, 1, `first slide`},
		{"scaled-y-scroller-scrolled-first-message", `<!doctype html><html><head><meta charset="utf-8"><style>body{font-family:"Golos Text",sans-serif;font-size:16px}.z{transform:scale(2);transform-origin:0 0}.sc{overflow-y:auto;height:200px;width:300px}.m{height:60px;margin:0}.g{font-family:Georgia}</style><script>window.Gazette={}</script></head><body><p>Top</p><div class=z><div class=sc id=s data-sc><p class="m g">first message</p><p class=m>m</p><p class=m>m</p><p class=m>m</p><p class=m>m</p><p class=m>m</p><p class=m>m</p><p class=m>m</p><p class=m>m</p><p class=m>m</p><p class=m>m</p><p class=m>m</p><p class=m>m</p><p class=m>m</p><p class=m>m</p><p class=m>m</p><p class=m>m</p><p class=m>m</p><p class=m>m</p><p class=m>m</p></div></div><script>s.scrollTop=1e6</script></body></html>`, 1, `first message`},
		{"scaled-y-scroller-last-message", `<!doctype html><html><head><meta charset="utf-8"><style>body{font-family:"Golos Text",sans-serif;font-size:16px}.z{transform:scale(2);transform-origin:0 0}.sc{overflow-y:auto;height:200px;width:300px}.m{height:60px;margin:0}.g{font-family:Georgia}</style><script>window.Gazette={}</script></head><body><p>Top</p><div class=z><div class=sc data-sc><p class=m>m</p><p class=m>m</p><p class=m>m</p><p class=m>m</p><p class=m>m</p><p class=m>m</p><p class=m>m</p><p class=m>m</p><p class=m>m</p><p class=m>m</p><p class=m>m</p><p class=m>m</p><p class=m>m</p><p class=m>m</p><p class=m>m</p><p class=m>m</p><p class=m>m</p><p class=m>m</p><p class=m>m</p><p class="m g">last message</p></div></div></body></html>`, 1, `last message`},
		{"scaled-y-border-scrollport-straddles-origin", `<!doctype html><html><head><meta charset="utf-8"><style>body{font-family:"Golos Text",sans-serif;font-size:16px}.z{transform:scale(2);transform-origin:0 0}.sc{position:relative;overflow-y:auto;height:200px;width:300px;border-top:30px solid #ccc}.tall{height:1000px}.sk{position:absolute;top:-20px;left:0;line-height:20px;font-family:Georgia}</style><script>window.Gazette={}</script></head><body><p>Top</p><div class=z><div class=sc data-sc><a class=sk data-t href=#m>skip to main</a><div class=tall>content</div></div></div></body></html>`, 0, ""},
		{"rtl-absolute-before-scroll-origin-stays-hidden", `<!doctype html><style>.sc{direction:rtl;position:relative;overflow-x:auto;width:300px}.wide{width:600px;height:40px}.bad{position:absolute;left:100%;top:0;white-space:nowrap;font-family:Georgia}</style><body><div class=sc><div class=wide></div><span class=bad>outside rtl origin</span></div></body>`, 0, ""},
		{"reverse-scrollport-outside-end-stays-hidden", `<!doctype html><style>.sc{position:relative;display:flex;flex-direction:column-reverse;overflow-y:auto;height:200px}.m{flex:none;height:60px}.bad{position:absolute;top:100%;left:0;font-family:Georgia}</style><body><p>Top</p><div class=sc data-sc><div class=m>newest</div><div class=m>m</div><div class=m>m</div><div class=m>m</div><div class=m>oldest</div><span class=bad>just below reverse end</span></div></body>`, 0, ""},
		{"transformed-scroller-near-end-reachable", `<!doctype html><style>.z{transform:scale(2);transform-origin:0 0}.sc{overflow-x:auto;width:300px;white-space:nowrap}.sl{display:inline-block;width:200px;height:40px}.bad{font-family:Georgia}</style><body><p>Top</p><div class=z><div class=sc><span class=sl>s</span><span class=sl>s</span><span class=sl>s</span><span class=sl>s</span><span class=sl>s</span><span class='sl bad'>near end</span></div></div></body>`, 1, `near end`},
		{"zoomed-scroller-near-end-reachable", `<!doctype html><style>.z{zoom:2}.sc{overflow-x:auto;width:300px;white-space:nowrap}.sl{display:inline-block;width:200px;height:40px}.bad{font-family:Georgia}</style><body><p>Top</p><div class=z><div class=sc><span class=sl>s</span><span class=sl>s</span><span class=sl>s</span><span class=sl>s</span><span class=sl>s</span><span class='sl bad'>near end</span></div></div></body>`, 1, `near end`},
		{"transformed-y-scroller-last-item-reachable", `<!doctype html><style>.z{transform:scale(2);transform-origin:0 0}.sc{overflow-y:auto;height:100px}.it{height:80px}.bad{font-family:Georgia}</style><body><p>Top</p><div class=z><div class=sc><div class=it>a</div><div class=it>a</div><div class=it>a</div><div class=it>a</div><div class='it bad'>last item</div></div></div></body>`, 1, `last item`},
		{"vertical-rl-x-scroller-left-end-reachable", `<!doctype html><style>.sc{writing-mode:vertical-rl;overflow-x:auto;width:300px;height:200px}.col{display:block;width:80px}.bad{font-family:Georgia}</style><body><p>Top</p><div class=sc><div class=col>c</div><div class=col>c</div><div class=col>c</div><div class=col>c</div><div class=col>c</div><div class=col>c</div><div class=col>c</div><div class=col>c</div><div class='col bad'>far left column</div></div></body>`, 1, `far left column`},
		{"skip-link-in-scrolled-box-stays-unreachable", `<!doctype html><style>.sc{position:relative;overflow-y:auto;height:200px}.tall{height:1000px}.bad{position:absolute;top:-40px;left:0;font-family:Georgia}</style><body><p>Top</p><div class=sc id=s><a class=bad>skip above origin</a><div class=tall>content</div></div><script>s.scrollTop=500</script></body>`, 0, ""},
		{"rtl-absolute-beyond-scroll-origin-stays-hidden", `<!doctype html><style>.sc{direction:rtl;position:relative;overflow-x:auto;width:300px}.wide{width:600px;height:40px}.bad{position:absolute;right:-9999px;top:0;font-family:Georgia}</style><body><p>Top</p><div class=sc><div class=wide></div><span class=bad>right of rtl origin</span></div></body>`, 0, ""},
		{"padded-x-scrollport-honors-border-origin", `<!doctype html><style>.sc{position:relative;overflow-x:auto;width:300px;padding:0 40px;border-left:30px solid}.wide{width:600px;height:40px}.bad{position:absolute;left:-60px;top:0;width:50px;overflow:hidden;white-space:nowrap;font-family:Georgia}</style><body><p>Top</p><div class=sc><div class=wide></div><span class=bad>under left border</span></div></body>`, 0, ""},
		{"bordered-y-scrollport-honors-border-origin", `<!doctype html><style>.sc{position:relative;overflow-y:auto;height:200px;border-top:30px solid}.tall{height:1000px}.bad{position:absolute;top:-40px;left:0;line-height:20px;font-family:Georgia}</style><body><p>Top</p><div class=sc><a class=bad>skip to main</a><div class=tall>content</div></div></body>`, 0, ""},
		{"absolute-right-edge-at-scroll-origin-hidden", `<!doctype html><html><head><style>body{font-family:"Golos Text",sans-serif;font-size:16px}.sc{position:relative;overflow-x:auto;width:300px}.wide{width:600px;height:40px}.g{position:absolute;right:100%;top:0;white-space:nowrap;font-family:Georgia}</style><script>window.Gazette={}</script></head><body><p>Top</p><div class=sc><div class=wide></div><span class=g>just left of origin</span></div></body></html>`, 0, ""},
		{"rtl-absolute-beyond-scroll-origin-hidden", `<!doctype html><html><head><style>body{font-family:"Golos Text",sans-serif;font-size:16px}.sc{direction:rtl;position:relative;overflow-x:auto;width:300px}.wide{width:600px;height:40px}.g{position:absolute;right:-9999px;top:0;white-space:nowrap;font-family:Georgia}</style><script>window.Gazette={}</script></head><body><p>Top</p><div class=sc><div class=wide></div><span class=g>right of rtl origin</span></div></body></html>`, 0, ""},
		{"reverse-absolute-after-scroll-end-hidden", `<!doctype html><style>.sc{position:relative;display:flex;flex-direction:column-reverse;overflow-y:auto;height:200px}.m{flex:none;height:60px}.bad{position:absolute;bottom:-100px;left:0;font-family:Georgia}</style><body><p>Top</p><div class=sc><div class=m>newest</div><div class=m>m</div><div class=m>m</div><div class=m>m</div><div class=m>oldest</div><span class=bad>below reverse end</span></div></body>`, 0, ""},
		{"zero-width-scrollport-border-does-not-show-content", `<!doctype html><style>.sc{overflow-x:auto;width:0;border:1px solid;white-space:nowrap}.bad{font-family:Georgia}</style><body><p>Top</p><div class=sc><span class=bad>no horizontal scrollport</span></div></body>`, 0, ""},
		{"zero-height-scrollport-border-does-not-show-content", `<!doctype html><style>.sc{overflow-y:auto;height:0;border:1px solid}.bad{font-family:Georgia}</style><body><p>Top</p><div class=sc><p class=bad>no vertical scrollport</p></div></body>`, 0, ""},
		{"content-visibility-auto-keeps-offscreen-text", `<!doctype html><style>.cv{content-visibility:auto;contain-intrinsic-size:auto 100px;margin-top:4000px}.bad{font-family:Georgia}</style><body><p>Top</p><div class=cv><p class=bad>auto offscreen</p></div></body>`, 1, `auto offscreen`},
		{"content-visibility-auto-keeps-onscreen-text", `<!doctype html><style>.cv{content-visibility:auto;contain-intrinsic-size:auto 100px}.bad{font-family:Georgia}</style><body><p>Top</p><div class=cv><p class=bad>auto on screen</p></div></body>`, 1, `auto on screen`},
		{"content-visibility-hidden-own-text", `<!doctype html><style>.cv{content-visibility:hidden;height:100px;font-family:Georgia}</style><body><p>Top</p><div class=cv>own text under hidden</div></body>`, 0, ""},
		{"inline-overflow-auto-does-not-clip-absolute-child", `<!doctype html><style>.sp{position:relative;overflow:auto}.g{position:absolute;left:300px;top:0;white-space:nowrap;font-family:Georgia}</style><body><p>Top <span class=sp>inline<span class=g>abs child of inline</span></span></p></body>`, 1, `abs child of inline`},
		{"contents-overflow-auto-does-not-clip", `<!doctype html><style>.dc{display:contents;overflow:auto}.g{font-family:Georgia}</style><body><p>Top</p><div class=dc><p class=g>inside display contents</p></div></body>`, 1, `inside display contents`},
		{"contents-overflow-hidden-does-not-clip", `<!doctype html><style>.dc{display:contents;overflow:hidden}.g{font-family:Georgia}</style><body><p>Top</p><div class=dc><p class=g>inside display contents hidden</p></div></body>`, 1, `inside display contents hidden`},
		{"unscrolled-auto-clips-under-border", `<!doctype html><style>.sc{position:relative;overflow:auto;width:300px;height:60px;border-left:30px solid #ccc}.g{position:absolute;left:-25px;top:0;width:20px;overflow:hidden;white-space:nowrap;font-family:Georgia}</style><body><p>Top</p><div class=sc><span class=g>under border</span></div></body>`, 0, ""},
		{"inline-auto-wrapper-does-not-clip-inline-block", `<!doctype html><style>.ib{display:inline-block;width:200px}.g{font-family:Georgia}.w{overflow:auto}</style><body><p>Top</p><p><span class=w><span class=ib>first line<br>second line <span class=g>of the card</span><br>third line</span></span></p></body>`, 1, `of the card`},
		{"inline-hidden-wrapper-does-not-clip-inline-block", `<!doctype html><style>.ib{display:inline-block;width:200px}.g{font-family:Georgia}.w{overflow:hidden}</style><body><p>Top</p><p><span class=w><span class=ib>first line<br>second line <span class=g>of the card</span><br>third line</span></span></p></body>`, 1, `of the card`},
		{"inline-scroll-wrapper-does-not-clip-inline-block", `<!doctype html><style>.ib{display:inline-block;width:200px}.g{font-family:Georgia}.w{overflow:scroll}</style><body><p>Top</p><p><span class=w><span class=ib>first line<br>second line <span class=g>of the card</span><br>third line</span></span></p></body>`, 1, `of the card`},
		{"webkit-box-reverse-reaches-last-item-ltr", `<!doctype html><style>.sc{display:-webkit-box;overflow-x:auto;width:300px;height:60px}.it{width:120px;height:40px;flex:none}.g{font-family:Georgia}.sc{-webkit-box-orient:horizontal;-webkit-box-direction:reverse}</style><body><p>Top</p><div class=sc><div class=it>1</div><div class=it>2</div><div class=it>3</div><div class=it>4</div><div class=it>5</div><div class=it>6</div><div class=it>7</div><div class="it g">last item</div></div></body>`, 1, `last item`},
		{"webkit-box-reverse-reaches-last-item-rtl", `<!doctype html><style>.sc{display:-webkit-box;overflow-x:auto;width:300px;height:60px}.it{width:120px;height:40px;flex:none}.g{font-family:Georgia}.sc{direction:rtl;-webkit-box-orient:horizontal;-webkit-box-direction:reverse}</style><body><p>Top</p><div class=sc><div class=it>1</div><div class=it>2</div><div class=it>3</div><div class=it>4</div><div class=it>5</div><div class=it>6</div><div class=it>7</div><div class="it g">last item</div></div></body>`, 1, `last item`},
		{"webkit-box-reverse-keeps-scrolled-item", `<!doctype html><style>.sc{display:-webkit-box;overflow-x:auto;width:300px;height:60px}.it{width:120px;height:40px;flex:none}.g{font-family:Georgia}.sc{-webkit-box-orient:horizontal;-webkit-box-direction:reverse}</style><body><p>Top</p><div class=sc id=s><div class=it>1</div><div class=it>2</div><div class=it>3</div><div class="it g">item 4</div><div class=it>5</div><div class=it>6</div><div class=it>7</div><div class=it>last</div></div><script>s.scrollLeft=-300</script></body>`, 1, `item 4`},
		{"rtl-column-wrap-reverse-direction", `<!doctype html><style>.sc{direction:rtl;display:flex;flex-direction:column;flex-wrap:wrap-reverse;overflow-x:auto;width:300px;height:100px}.it{flex:none;width:140px;height:40px}.g{font-family:Georgia}</style><body><p>Top</p><div class=sc><div class=it>1</div><div class=it>2</div><div class=it>3</div><div class=it>4</div><div class=it>5</div><div class=it>6</div><div class=it>7</div><div class=it>8</div><div class=it>9</div><div class=it>10</div><div class=it>11</div><div class="it g">last column</div></div></body>`, 1, `last column`},
		{"column-reverse-scroll-unreachable-flex-end", `<!doctype html><style>.sc{display:flex;flex-direction:column-reverse;justify-content:flex-end;overflow-y:scroll;width:300px;height:100px}.it{flex:none;height:40px}.g{font-family:Georgia}</style><body><p>Top</p><div class=sc><div class="it g">first item</div><div class=it>1</div><div class=it>2</div><div class=it>3</div><div class=it>4</div><div class=it>5</div></div></body>`, 0, ""},
		{"column-reverse-flex-end-clips-y-while-x-scrolls", `<!doctype html><style>.sc{display:flex;flex-direction:column-reverse;justify-content:flex-end;overflow:auto;width:300px;height:200px}.it{flex:none;height:60px;width:600px}.g{font-family:Georgia}</style><body><p>Top</p><div class=sc><div class="it g">first item</div><div class=it>1</div><div class=it>2</div><div class=it>3</div><div class=it>4</div><div class=it>5</div><div class=it>6</div></div></body>`, 0, ""},
		{"row-flex-end-scroll-unreachable-y", `<!doctype html><style>.sc{margin-left:600px;display:flex;flex-wrap:nowrap;justify-content:flex-end;overflow:auto;width:300px;height:60px}.it{flex:none;width:120px;height:40px}.tall{flex:none;width:120px;height:300px}.g{font-family:Georgia}</style><body><p>Top</p><div class=sc><div class="it g">first item</div><div class=it>1</div><div class=it>2</div><div class=it>3</div><div class=it>4</div><div class=tall>tall</div></div></body>`, 0, ""},
		{"row-flex-end-unscrollable-x", `<!doctype html><style>.sc{margin-left:600px;display:flex;justify-content:flex-end;overflow-x:scroll;width:300px;height:60px}.it{flex:none;width:120px;height:40px}.g{font-family:Georgia}</style><body><p>Top</p><div class=sc><div class="it g">first item</div><div class=it>1</div><div class=it>2</div><div class=it>3</div><div class=it>4</div><div class=it>5</div></div></body>`, 0, ""},
		{"y-scroll-glyph-bottom-at-origin", `<!doctype html><style>p.top{margin:0;height:40px;line-height:20px}.sc{position:relative;overflow-y:auto;height:200px;width:300px}.tall{height:1000px}.g{position:absolute;bottom:calc(100% - 1px);left:0;font-family:Georgia}</style><body><p class=top>Top</p><div class=sc><div class=tall>content</div><span class=g>glyph bottom at origin</span></div></body>`, 0, ""},
		{"text-under-right-border-is-clipped", `<!doctype html><style>.g{font-family:Georgia}.sc{position:relative;overflow:hidden;width:300px;height:60px;border-right:30px solid #ccc}.u{position:absolute;left:305px;top:0;width:20px;overflow:hidden;white-space:nowrap}</style><body><div class=sc><span class="u g">under right border</span></div></body>`, 0, ""},
		{"text-under-bottom-border-is-clipped", `<!doctype html><style>.g{font-family:Georgia}.sc{position:relative;overflow:hidden;width:300px;height:60px;border-bottom:30px solid #ccc}.u{position:absolute;left:0;top:65px;height:20px;overflow:hidden;white-space:nowrap}</style><body><div class=sc><span class="u g">under bottom border</span></div></body>`, 0, ""},
		{"x-scrollport-under-parent-border-stays-clipped", `<!doctype html><style>.g{font-family:Georgia}.p{overflow:hidden;width:30px;height:80px;margin-left:400px}.sc{overflow-x:auto;overflow-y:hidden;width:300px;height:60px;border-right:30px solid #ccc;margin-left:-300px}.w{width:900px;white-space:nowrap}</style><body><div class=p><div class=sc><div class=w>start <span class=g>scrolled text</span> end</div></div></div></body>`, 0, ""},
		{"y-scrollport-under-parent-border-stays-clipped", `<!doctype html><style>.g{font-family:Georgia}.p{overflow:hidden;width:300px;height:30px;margin-top:300px}.sc{overflow-y:auto;overflow-x:hidden;width:300px;height:200px;border-bottom:30px solid #ccc;margin-top:-200px}.t{height:900px}</style><body><div class=p><div class=sc><div class=t><p class=g>scrolled text</p></div></div></div></body>`, 0, ""},
		{"text-under-top-border-is-clipped", `<!doctype html><style>.g{font-family:Georgia}.sc{position:relative;overflow:hidden;width:300px;height:60px;border-top:30px solid #ccc}.u{position:absolute;left:0;top:-25px;height:20px;overflow:hidden;white-space:nowrap}</style><body><div class=sc><span class="u g">under top border</span></div></body>`, 0, ""},
		{"paint-containment-clips-under-border", `<!doctype html><style>.g{font-family:Georgia}.sc{position:relative;contain:paint;width:300px;height:60px;border-left:30px solid #ccc}.u{position:absolute;left:-25px;top:0;width:20px;overflow:hidden;white-space:nowrap}</style><body><div class=sc><span class="u g">under paint border</span></div></body>`, 0, ""},
		{"webkit-inline-box-reverse-reaches-last-item", `<!doctype html><style>.g{font-family:Georgia}.sc{display:-webkit-inline-box;-webkit-box-direction:reverse;overflow-x:auto;width:300px;height:60px;margin-left:700px}.it{width:120px;height:40px}</style><body><div class=sc><div class=it>1</div><div class=it>2</div><div class=it>3</div><div class=it>4</div><div class=it>5</div><div class=it>6</div><div class="it g">last item</div></div></body>`, 1, `last item`},
		{"webkit-box-reverse-absolute-beyond-scroll-range", `<!doctype html><style>.g{font-family:Georgia}.sc{position:relative;display:-webkit-box;overflow-x:auto;width:300px;height:60px}.it{width:120px;height:40px;flex:none}.sc{-webkit-box-orient:horizontal;-webkit-box-direction:reverse}.g.abs{position:absolute;left:1110px;top:5px;white-space:nowrap}</style><body><div class=sc><div class=it>1</div><div class=it>2</div><div class=it>3</div><div class=it>4</div><div class=it>5</div><div class=it>6</div><div class=it>7</div><div class=it>last</div><span class="g abs">beyond the right edge</span></div></body>`, 0, ""},
		{"webkit-vertical-box-reverse-range-reaches-last-item", `<!doctype html><style>.g{font-family:Georgia}.sc{display:-webkit-box;-webkit-box-orient:vertical;overflow-y:auto;width:300px;height:100px}.it{height:40px}</style><body><div class=sc><div class=it>1</div><div class=it>2</div><div class=it>3</div><div class=it>4</div><div class=it>5</div><div class=it>6</div><div class=it>7</div><div class="it g">last item</div></div></body>`, 1, `last item`},
		{"parent-clips-scrollport-right-border", `<!doctype html><style>.g{font-family:Georgia}.p{overflow:hidden;width:30px;height:80px;margin-left:400px}.sc{overflow-x:auto;overflow-y:hidden;width:300px;height:60px;border-right:30px solid #ccc;margin-left:-300px}.w{width:900px;white-space:nowrap}</style><body><div class=p><div class=sc><div class=w>start <span class=g>scrolled text</span> end</div></div></div></body>`, 0, ""},
		{"parent-clips-scrollport-bottom-border", `<!doctype html><style>.g{font-family:Georgia}.p{overflow:hidden;width:300px;height:30px;margin-top:300px}.sc{overflow-y:auto;overflow-x:hidden;width:300px;height:200px;border-bottom:30px solid #ccc;margin-top:-200px}.t{height:900px}</style><body><div class=p><div class=sc><div class=t><p class=g>scrolled text</p></div></div></div></body>`, 0, ""},
		{"parent-clips-overflow-x-at-client-edge", `<!doctype html><style>.g{font-family:Georgia}.p{overflow:hidden;width:30px;height:80px;margin-left:400px}.sc{overflow-x:auto;overflow-y:hidden;width:300px;height:60px;border-right:30px solid #ccc;margin-left:-300px}.w{width:900px;white-space:nowrap}</style><body><div class=p><div class=sc><div class=w>start <span class=g>scrolled text</span> end</div></div></div></body>`, 0, ""},
		{"parent-clips-overflow-y-at-client-edge", `<!doctype html><style>.g{font-family:Georgia}.p{overflow:hidden;width:300px;height:30px;margin-top:300px}.sc{overflow-y:auto;overflow-x:hidden;width:300px;height:200px;border-bottom:30px solid #ccc;margin-top:-200px}.t{height:900px}</style><body><div class=p><div class=sc><div class=t><p class=g>scrolled text</p></div></div></div></body>`, 0, ""},
		{"paint-containment-clips-at-client-edge", `<!doctype html><style>.g{font-family:Georgia}.p{overflow:hidden;width:30px;height:80px;margin-left:400px}.sc{contain:paint;width:300px;height:60px;border-right:30px solid #ccc;margin-left:-300px}.w{width:900px;white-space:nowrap}</style><body><div class=p><div class=sc><div class=w>start <span class=g>scrolled text</span> end</div></div></div></body>`, 0, ""},
		{"vertical-lr-rtl-y-scroll-reaches-inline-start", `<!doctype html><style>.sc{writing-mode:vertical-lr;direction:rtl;overflow-y:auto;width:100px;height:100px;white-space:nowrap}.it{display:inline-block;width:80px;height:80px}.bad{font-family:Georgia}</style><body><p>Top</p><div class=sc><span class='it bad'>first inline item</span><span class=it>2</span><span class=it>3</span><span class=it>4</span><span class=it>5</span></div></body>`, 1, `first inline item`},
		{"vertical-lr-rtl-y-scroll-reaches-inline-start-absolute", `<!doctype html><style>.sc{position:relative;writing-mode:vertical-lr;direction:rtl;overflow-y:auto;width:100px;height:100px}.tall{height:500px}.bad{position:absolute;top:-150px;left:0;font-family:Georgia}</style><body><p>Top</p><div class=sc><div class=tall>content</div><span class=bad>reachable at inline start</span></div></body>`, 1, `reachable at inline start`},
		{"inline-contain-paint-keeps-text", `<!doctype html><style>.g{font-family:Georgia}</style><body><p>Top</p><p>before <span style="contain:paint" class=g>inline paint text</span> after</p></body>`, 1, `inline paint text`},
		{"contents-contain-paint-keeps-text", `<!doctype html><style>.g{font-family:Georgia}</style><body><p>Top</p><div style="display:contents;contain:paint"><p class=g>inside contents paint</p></div></body>`, 1, `inside contents paint`},
		{"inline-contain-content-keeps-text", `<!doctype html><style>.g{font-family:Georgia}</style><body><p>Top</p><p>before <span style="contain:content" class=g>inline content text</span> after</p></body>`, 1, `inline content text`},
		{"block-contain-content-clips-overflow", `<!doctype html><style>.box{contain:content;height:20px}.g{font-family:Georgia}</style><body><p>Top</p><div class=box><p>inside</p><p class=g>below content containment</p></div></body>`, 0, ""},
		{"ruby-overflow-hidden-keeps-annotation", `<!doctype html><style>.g{font-family:Georgia}</style><body><p>Top</p><p><ruby style="overflow:hidden">base<rt class=g>annotation</rt></ruby> after</p></body>`, 1, `annotation`},
		{"ruby-text-overflow-hidden-keeps-annotation", `<!doctype html><style>rt{overflow:hidden}.g{font-family:Georgia}</style><body><p>Top</p><p><ruby>base<rt class=g>annotation over the base text</rt></ruby> after</p></body>`, 1, `annotation over the base text`},
		{"ruby-text-contain-paint-keeps-annotation", `<!doctype html><style>rt{contain:paint}.g{font-family:Georgia}</style><body><p>Top</p><p><ruby>base<rt class=g>annotation paint</rt></ruby> after</p></body>`, 1, `annotation paint`},
		{"inline-list-item-overflow-keeps-text", `<!doctype html><style>.c{display:inline list-item;overflow:hidden}.g{font-family:Georgia}</style><body><p>Top</p><p>x <span class="c g">inline list item text</span></p></body>`, 1, `inline list item text`},
		{"inline-block-overflow-clips-absolute-tail", `<!doctype html><style>.box{position:relative;display:inline-block;width:40px;height:20px;overflow:hidden}.bad{position:absolute;left:100px;top:0;font-family:Georgia;white-space:nowrap}</style><body><p>Top</p><p><span class=box><span class=bad>clipped inline-block tail</span></span></p></body>`, 0, ""},
		{"inline-flex-overflow-clips-absolute-tail", `<!doctype html><style>.box{position:relative;display:inline-flex;width:40px;height:20px;overflow:hidden}.bad{position:absolute;left:100px;top:0;font-family:Georgia;white-space:nowrap}</style><body><p>Top</p><p><span id=box class=box><span class=bad>clipped inline flex tail</span></span></p><script>const nativeStyle=window.getComputedStyle.bind(window);window.getComputedStyle=(el,pseudo)=>{const style=nativeStyle(el,pseudo);return el.id==='box'?new Proxy(style,{get:(target,key)=>key==='display'?'inline flex':Reflect.get(target,key,target)}):style}</script></body>`, 0, ""},
		{"inline-grid-overflow-clips-absolute-tail", `<!doctype html><style>.box{position:relative;display:inline-grid;width:40px;height:20px;overflow:hidden}.bad{position:absolute;left:100px;top:0;font-family:Georgia;white-space:nowrap}</style><body><p>Top</p><p><span id=box class=box><span class=bad>clipped inline grid tail</span></span></p><script>const nativeStyle=window.getComputedStyle.bind(window);window.getComputedStyle=(el,pseudo)=>{const style=nativeStyle(el,pseudo);return el.id==='box'?new Proxy(style,{get:(target,key)=>key==='display'?'inline grid':Reflect.get(target,key,target)}):style}</script></body>`, 0, ""},
		{"inline-table-overflow-clips-absolute-tail", `<!doctype html><style>.box{position:relative;display:inline-table;width:40px;height:20px;overflow:hidden}.bad{position:absolute;left:100px;top:0;font-family:Georgia;white-space:nowrap}</style><body><p>Top</p><p><span id=box class=box><span class=bad>clipped inline table tail</span></span></p><script>const nativeStyle=window.getComputedStyle.bind(window);window.getComputedStyle=(el,pseudo)=>{const style=nativeStyle(el,pseudo);return el.id==='box'?new Proxy(style,{get:(target,key)=>key==='display'?'inline table':Reflect.get(target,key,target)}):style}</script></body>`, 0, ""},
		{"inline-flow-root-list-item-overflow-clips-absolute-tail", `<!doctype html><style>.box{position:relative;display:inline flow-root list-item;width:40px;height:20px;overflow:hidden}.bad{position:absolute;left:100px;top:0;font-family:Georgia;white-space:nowrap}</style><body><p>Top</p><p><span class=box><span class=bad>clipped inline flow-root list-item tail</span></span></p></body>`, 0, ""},
		{"scaled-paint-clips-under-left-border", `<!doctype html><style>.wrap{transform:scale(2);transform-origin:0 0;margin-bottom:120px}.sc{position:relative;contain:paint;width:300px;height:60px;border-left:20px solid}.u{position:absolute;left:-15px;top:0;width:12px;overflow:hidden;white-space:nowrap}.g{font-family:Georgia}</style><body><p>Top</p><div class=wrap><div class=sc><span class="u g">under border</span></div></div></body>`, 0, ""},
		{"paint-clips-under-right-border", `<!doctype html><style>.sc{position:relative;contain:paint;width:300px;height:60px;border-right:30px solid}.u{position:absolute;left:305px;top:0;width:20px;overflow:hidden;white-space:nowrap}.g{font-family:Georgia}</style><body><p>Top</p><div class=sc><span class="u g">under border</span></div></body>`, 0, ""},
		{"paint-clips-under-top-border", `<!doctype html><style>.sc{position:relative;contain:paint;width:300px;height:60px;border-top:30px solid}.u{position:absolute;left:0;top:-25px;height:20px;overflow:hidden;white-space:nowrap}.g{font-family:Georgia}</style><body><p>Top</p><div class=sc><span class="u g">under border</span></div></body>`, 0, ""},
		{"scaled-paint-clips-under-top-border", `<!doctype html><style>.wrap{transform:scale(2);transform-origin:0 0;margin-bottom:160px}.sc{position:relative;contain:paint;width:300px;height:60px;border-top:20px solid}.u{position:absolute;left:0;top:-15px;height:12px;overflow:hidden;white-space:nowrap;font-size:10px}.g{font-family:Georgia}</style><body><p>Top</p><div class=wrap><div class=sc><span class="u g">under border</span></div></div></body>`, 0, ""},
		{"paint-clips-under-bottom-border", `<!doctype html><style>.sc{position:relative;contain:paint;width:300px;height:60px;border-bottom:30px solid}.u{position:absolute;left:0;top:65px;height:20px;overflow:hidden;white-space:nowrap}.g{font-family:Georgia}</style><body><p>Top</p><div class=sc><span class="u g">under border</span></div></body>`, 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, report, err := browserCheck(t, browserCtx, tc.html)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.count || (tc.contains != "" && !strings.Contains(report, tc.contains)) {
				t.Fatalf("got %d, report %q; want %d containing %q", got, report, tc.count, tc.contains)
			}
		})
	}
}

func TestCheckFontsDoesNotChangeScrollOrClasses(t *testing.T) {
	browserCtx := newLayoutTestBrowser(t)
	page := `<!doctype html><html><head><style>
body{font-family:"Golos Text",sans-serif}.sc{height:100px;overflow-y:auto}.item{height:60px}.bad{font-family:Georgia}
</style></head><body><div class=sc id=s><p class="item bad">first item</p><p class=item>second</p><p class=item>third</p><p class=item>fourth</p></div>
<script>s.scrollTop=120;s.addEventListener('scroll',()=>document.body.classList.add('scrolled'))</script></body></html>`
	count, _, err := testSingleLayoutPage(t, browserCtx, page, func(ctx context.Context) (int, string, error) {
		if _, err := chromedp.Run(ctx, chromedp.Sleep(50*time.Millisecond)); err != nil {
			return 0, "", err
		}
		before, err := chromedp.Run(ctx, chromedp.Evaluate[string](`JSON.stringify([s.scrollTop,document.body.className,innerWidth,innerHeight])`))
		if err != nil {
			return 0, "", err
		}
		res, err := checkFonts(ctx)
		if err != nil {
			return 0, "", err
		}
		if _, err := chromedp.Run(ctx, chromedp.Sleep(50*time.Millisecond)); err != nil {
			return 0, "", err
		}
		after, err := chromedp.Run(ctx, chromedp.Evaluate[string](`JSON.stringify([s.scrollTop,document.body.className,innerWidth,innerHeight])`))
		if err != nil {
			return 0, "", err
		}
		if before != after || !strings.HasPrefix(before, `[120,`) {
			return 0, after, errors.New("checkFonts changed the scroll position or page classes")
		}
		return len(res.Violations), res.report("800x600"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("checkFonts found %d violations, want the scrolled first item", count)
	}
}

func TestBodyScrollerDocumentWidthIncludesViewport(t *testing.T) {
	browserCtx := newLayoutTestBrowser(t)
	path := filepath.Join(t.TempDir(), "body-scroll-wide-viewport.html")
	page := `<!doctype html><html><head><style>
body{font-family:"Golos Text",sans-serif;font-size:16px}
html{overflow:hidden}body{margin:0;width:300px;overflow-x:auto}.wide{width:1000px;height:40px}.ab{position:absolute;left:1000px;top:60px;font-family:Georgia}
</style></head><body><p>Top</p><div class=wide></div><span class=ab>abs on screen right of body</span></body></html>`
	if err := os.WriteFile(path, []byte(page), 0o644); err != nil {
		t.Fatal(err)
	}
	tabCtx, cancelTab := chromedp.NewContext(browserCtx)
	defer cancelTab()
	if err := chromedp.Do(tabCtx, chromedp.EmulateViewport(1440, 900), chromedp.Navigate("file://"+path)); err != nil {
		t.Fatal(err)
	}
	res, err := checkFonts(tabCtx)
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || len(res.Violations) != 1 || !strings.Contains(res.Violations[0].Text, "abs on screen right") {
		t.Fatalf("got %#v; want visible body-scroll text", res)
	}
}

func TestLegacyBoxNegativeScrollRange(t *testing.T) {
	browserCtx := newLayoutTestBrowser(t)
	path := filepath.Join(t.TempDir(), "legacy-box-negative-scroll.html")
	page := `<!doctype html><html><head><style>
body{font-family:"Golos Text",sans-serif;font-size:16px}
.sc{display:-webkit-box;overflow-x:auto;width:300px;height:60px}.it{width:120px;height:40px;flex:none}
.g{font-family:Georgia}.sc{-webkit-box-orient:horizontal;-webkit-box-direction:reverse}
</style></head><body><div class=sc id=s><div class=it>1</div><div class=it>2</div><div class=it>3</div><div class="it g" id=t>item 4</div><div class=it>5</div><div class=it>6</div><div class=it>7</div><div class=it>last</div></div><script>s.scrollLeft=-300</script></body></html>`
	if err := os.WriteFile(path, []byte(page), 0o644); err != nil {
		t.Fatal(err)
	}
	tabCtx, cancelTab := chromedp.NewContext(browserCtx)
	defer cancelTab()
	if err := chromedp.Do(tabCtx, chromedp.EmulateViewport(800, 600), chromedp.Navigate("file://"+path)); err != nil {
		t.Fatal(err)
	}
	state, err := chromedp.Run(tabCtx, chromedp.Evaluate[string](`JSON.stringify([s.scrollLeft,s.scrollWidth,s.clientWidth,getComputedStyle(s).display,t.getBoundingClientRect().left,t.getBoundingClientRect().right])`))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("legacy scroller state: %s", state)
	if !strings.HasPrefix(state, `[-300,`) {
		t.Fatalf("scroll position is not negative as expected: %s", state)
	}
	res, err := checkFonts(tabCtx)
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || len(res.Violations) != 1 || !strings.Contains(res.Violations[0].Text, "item 4") {
		t.Fatalf("got %#v; want the currently scrolled legacy-box item", res)
	}
}

func TestDocumentScrollDimensionsIncludeBothRoots(t *testing.T) {
	browserCtx := newLayoutTestBrowser(t)
	cases := []struct {
		name, page, text string
	}{
		{"doctype-absolute-below", `<!doctype html><html><head><style>body{font-family:"Golos Text",sans-serif}.bad{position:absolute;top:3000px;left:20px;font-family:Georgia}</style><script>window.Gazette={}</script></head><body><p>Top</p><span class=bad>plain absolute below</span></body></html>`, "plain absolute below"},
		{"doctype-absolute-right", `<!doctype html><html><head><style>body{font-family:"Golos Text",sans-serif}.bad{position:absolute;left:3000px;top:40px;font-family:Georgia}</style><script>window.Gazette={}</script></head><body><p>Top</p><span class=bad>plain absolute right</span></body></html>`, "plain absolute right"},
		{"quirks-absolute-below", `<html><head><style>body{font-family:"Golos Text",sans-serif}.bad{position:absolute;top:3000px;left:20px;font-family:Georgia}</style><script>window.Gazette={}</script></head><body><p>Top</p><span class=bad>quirks absolute below</span></body></html>`, "quirks absolute below"},
		{"quirks-absolute-right", `<html><head><style>body{font-family:"Golos Text",sans-serif}.bad{position:absolute;left:3000px;top:40px;font-family:Georgia}</style><script>window.Gazette={}</script></head><body><p>Top</p><span class=bad>quirks absolute right</span></body></html>`, "quirks absolute right"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), tc.name+".html")
			if err := os.WriteFile(path, []byte(tc.page), 0o644); err != nil {
				t.Fatal(err)
			}
			tabCtx, cancelTab := chromedp.NewContext(browserCtx)
			defer cancelTab()
			if err := chromedp.Do(tabCtx, chromedp.EmulateViewport(800, 600), chromedp.Navigate("file://"+path)); err != nil {
				t.Fatal(err)
			}
			res, err := checkFonts(tabCtx)
			if err != nil {
				t.Fatal(err)
			}
			if res == nil || len(res.Violations) != 1 || !strings.Contains(res.Violations[0].Text, tc.text) {
				t.Fatalf("got %#v; want document overflow text %q", res, tc.text)
			}
		})
	}
}

func TestFontViolationUsesLayoutExitPath(t *testing.T) {
	browserCtx := newLayoutTestBrowser(t)
	path := filepath.Join(t.TempDir(), "font.html")
	page := `<!doctype html><html><body><main><h2 style="font-family:Georgia">Bad family</h2></main><script>window.Gazette={}</script></body></html>`
	if err := os.WriteFile(path, []byte(page), 0o644); err != nil {
		t.Fatal(err)
	}
	err := shootViewports(browserCtx, "file://"+path, t.TempDir(), []time.Duration{0})
	if !errors.Is(err, errFonts) || exitCode(err) != exitLayout {
		t.Fatalf("font findings should use layout exit 2, got exit %d: %v", exitCode(err), err)
	}
}

func browserCheck(t *testing.T, browserCtx context.Context, page string) (int, string, error) {
	t.Helper()
	const familyStyle = `<style>body{font-family:"Golos Text",sans-serif}</style>`
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(page)), "<!doctype") {
		lower := strings.ToLower(page)
		if head := strings.Index(lower, "<head"); head >= 0 {
			if end := strings.IndexByte(page[head:], '>'); end >= 0 {
				at := head + end + 1
				page = page[:at] + familyStyle + page[at:]
			} else {
				page = familyStyle + page
			}
		} else if doctypeEnd := strings.IndexByte(page, '>'); doctypeEnd >= 0 {
			at := doctypeEnd + 1
			page = page[:at] + familyStyle + page[at:]
		}
	} else {
		page = familyStyle + page
	}
	return testSingleLayoutPage(t, browserCtx, page, func(ctx context.Context) (int, string, error) {
		res, err := checkFonts(ctx)
		if err != nil || res == nil {
			return 0, "", err
		}
		return len(res.Violations), res.report("800x600"), nil
	})
}
