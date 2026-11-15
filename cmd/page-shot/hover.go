package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/chromedp"
)

const (
	maxHoverCandidates    = 20
	redBrightnessWeight   = 299
	greenBrightnessWeight = 587
	blueBrightnessWeight  = 114
	brightnessWeightTotal = redBrightnessWeight + greenBrightnessWeight + blueBrightnessWeight
	lightBubbleBrightness = 180
	darkPageBrightness    = 120
	hoverWaitLimit        = 400 * time.Millisecond
)

type hoverCandidate struct {
	Index    int `json:"index"`
	Priority int `json:"priority"`
	WaitMS   int `json:"waitMs"`
}
type hoverBubble struct {
	NodeID   int     `json:"nodeID"`
	Selector string  `json:"selector"`
	Text     string  `json:"text"`
	Lines    int     `json:"lines"`
	Height   float64 `json:"height"`
	BG       string  `json:"bg"`
	PageBG   string  `json:"pageBg"`
}
type hoverWarning struct {
	Bubble hoverBubble
	Theme  string
	Frame  string
	Kind   string
}

func (w hoverWarning) report() string {
	return fmt.Sprintf("page-shot: предупреждение: hover-пузырь (%s), селектор %s, тема %s: «%s», %d строк, высота %.0f px, кадр %s\n", w.Kind, w.Bubble.Selector, w.Theme, w.Bubble.Text, w.Bubble.Lines, w.Bubble.Height, w.Frame)
}

const hoverPrepareJS = `(() => {
  const visible=el=>{if(!(el instanceof Element)||!el.isConnected)return false;try{return el.checkVisibility({opacityProperty:true,visibilityProperty:true})}catch(_){const s=getComputedStyle(el);return s.display!=='none'&&s.visibility!=='hidden'}};
  const eventTypes=/^(mouse|pointer)/,groups=[[],[],[],[]],rank=new Map(),waitByNode=new WeakMap();
  const add=(el,p,wait=__QUICK_WAIT_MS__)=>{if(!(el instanceof Element)||!el.isConnected)return;const old=rank.get(el);if(old!==undefined){waitByNode.set(el,Math.max(waitByNode.get(el)||0,wait));if(p<old){groups[old]=groups[old].filter(x=>x!==el);groups[p].push(el);rank.set(el,p)}return}if(!visible(el))return;const r=el.getBoundingClientRect();if(r.width<=0||r.height<=0)return;rank.set(el,p);waitByNode.set(el,wait);groups[p].push(el)};
  const roots=[document],all=[];for(let i=0;i<roots.length;i++)for(const el of roots[i].querySelectorAll('*')){all.push(el);if(el.shadowRoot)roots.push(el.shadowRoot)}
  const pointerListeners=el=>{try{return Object.entries(getEventListeners(el)).filter(([type,list])=>eventTypes.test(type)&&list.length).flatMap(([,list])=>list)}catch(_){return []}};
  const marker='[data-gz-tip],[data-tip],[data-tooltip],[aria-describedby]',marked=el=>{try{return el.matches(marker)}catch(_){return false}};
  const parent=e=>e.parentElement||(e.getRootNode&&e.getRootNode().host),within=(root,el)=>{for(let n=el;n;n=parent(n))if(n===root)return true;return false};
  const delayedSource=source=>/\b(setTimeout|requestAnimationFrame|requestIdleCallback)\b/.test(source),delayedFor=el=>getComputedStyle(el).transitionDelay.split(',').some(value=>parseFloat(value)>0),hasDelay=style=>style.transitionDelay.split(',').some(value=>parseFloat(value)>0)||style.transition.split(',').some(part=>{const times=part.match(/-?\d*\.?\d+m?s/g)||[];return times.length>1&&parseFloat(times[1])>0});
  const delayedTokens=new Set(),inspectRules=rules=>{for(const rule of rules){if(rule.selectorText&&rule.style&&hasDelay(rule.style)){for(const token of rule.selectorText.match(/[#.][\w-]+/g)||[])delayedTokens.add(token.slice(1))}if(rule.cssRules)inspectRules(rule.cssRules)}};for(const sheet of document.styleSheets)try{inspectRules(sheet.cssRules||[])}catch(_){}for(const root of roots.slice(1))for(const style of root.querySelectorAll('style'))try{inspectRules(style.sheet?.cssRules||[])}catch(_){}
  const delayedHandler=source=>delayedSource(source)||[...delayedTokens].some(token=>source.split(/[^\w-]+/).includes(token));
  const delegated=[];for(const el of all){const listeners=pointerListeners(el);if(listeners.length){const delayed=delayedFor(el)||listeners.some(item=>delayedHandler(Function.prototype.toString.call(item.listener)));add(el,0,delayed?__MAX_WAIT_MS__:__QUICK_WAIT_MS__);if(listeners.some(item=>/\b(target|closest|composedPath)\b/.test(Function.prototype.toString.call(item.listener))))delegated.push(el)}}
  const rootListeners=[document,window,document.documentElement,document.body,...roots.slice(1)].flatMap(pointerListeners),rootDelayed=rootListeners.some(item=>delayedSource(Function.prototype.toString.call(item.listener)));
  for(const el of all)if(marked(el))add(el,1,rootDelayed||delayedFor(el)?__MAX_WAIT_MS__:__QUICK_WAIT_MS__);
  for(const el of delegated)if(el!==document.body&&el!==document.documentElement)for(const child of all)if(child!==el&&within(el,child))add(child,1);
  const selectors=[];
  const scan=rules=>{for(const rule of rules){if(rule.selectorText&&/:hover\b/.test(rule.selectorText))for(const branch of rule.selectorText.split(',')){const at=branch.indexOf(':hover');if(at>=0){const selector=branch.slice(0,at+6).replace(/:hover\b/g,'').replace(/[>+~]\s*$/,'').trim();if(selector)selectors.push(selector)}}if(rule.cssRules)scan(rule.cssRules)}};
  for(const sheet of document.styleSheets)try{scan(sheet.cssRules||[])}catch(_){}
  for(const root of roots.slice(1))for(const style of root.querySelectorAll('style'))try{scan(style.sheet?.cssRules||[])}catch(_){}
  for(const selector of selectors)for(const root of roots)try{for(const el of root.querySelectorAll(selector))add(el,2,__QUICK_WAIT_MS__)}catch(_){}
  for(const root of roots)for(const el of root.querySelectorAll('a[href]'))add(el,3,__QUICK_WAIT_MS__);
  const unique=[];for(let p=0;p<groups.length;p++){const group=groups[p],slots=__MAX_HOVER_CANDIDATES__-unique.length;if(slots<=0)break;if(group.length<=slots)unique.push(...group);else for(let i=0;i<slots;i++)unique.push(group[Math.floor(i*group.length/slots)])}
  window.__pageShotHover={nodes:unique,roots,ids:new WeakMap(),nextID:0,before:new Set(),x:0,y:0};return unique.map((el,index)=>({index,priority:rank.get(el),waitMs:waitByNode.get(el)}));
})()`

const hoverScrollJS = `new Promise(resolve=>{const el=window.__pageShotHover.nodes[__CANDIDATE_INDEX__];if(!el||!el.isConnected){resolve(false);return}el.scrollIntoView({block:'center',inline:'nearest',behavior:'instant'});requestAnimationFrame(()=>requestAnimationFrame(()=>requestAnimationFrame(()=>resolve(true))))})`
const hoverSnapshotBeforeJS = `(() => {
 const s=window.__pageShotHover,el=s.nodes[__CANDIDATE_INDEX__];if(!el||!el.isConnected)return null;
 const all=[],roots=[document];for(let i=0;i<roots.length;i++)for(const e of roots[i].querySelectorAll('*')){all.push(e);if(e.shadowRoot)roots.push(e.shadowRoot)}s.roots=roots;
 const visible=e=>{if(!(e instanceof Element)||!e.isConnected)return false;const r=e.getBoundingClientRect();if(r.width<=0||r.height<=0||r.right<=0||r.bottom<=0||r.left>=innerWidth||r.top>=innerHeight)return false;try{return e.checkVisibility({opacityProperty:true,visibilityProperty:true})}catch(_){const c=getComputedStyle(e);return c.display!=='none'&&c.visibility!=='hidden'}};
 const id=e=>{let n=s.ids.get(e);if(n===undefined){n=s.nextID++;s.ids.set(e,n)}return n};s.before=new Set(all.filter(visible).map(id));
 const r=el.getBoundingClientRect();s.x=r.left+r.width/2;s.y=r.top+r.height/2;return {x:s.x,y:s.y};
})()`

const hoverCollectJS = `(() => {
 const s=window.__pageShotHover,all=[],rootList=[document];for(let i=0;i<rootList.length;i++)for(const e of rootList[i].querySelectorAll('*')){all.push(e);if(e.shadowRoot)rootList.push(e.shadowRoot)}s.roots=rootList;
 const visible=e=>{if(!(e instanceof Element)||!e.isConnected)return false;const r=e.getBoundingClientRect();if(r.width<=0||r.height<=0||r.right<=0||r.bottom<=0||r.left>=innerWidth||r.top>=innerHeight)return false;try{return e.checkVisibility({opacityProperty:true,visibilityProperty:true})}catch(_){const c=getComputedStyle(e);return c.display!=='none'&&c.visibility!=='hidden'}};
 const id=e=>{let n=s.ids.get(e);if(n===undefined){n=s.nextID++;s.ids.set(e,n)}return n};
 const near=e=>{const r=e.getBoundingClientRect(),dx=Math.max(r.left-s.x,0,s.x-r.right),dy=Math.max(r.top-s.y,0,s.y-r.bottom);return Math.hypot(dx,dy)<=Math.max(300,innerHeight)};
 const fresh=all.filter(e=>visible(e)&&!s.before.has(id(e))&&near(e)),freshSet=new Set(fresh),parent=e=>e.parentElement||(e.getRootNode&&e.getRootNode().host);
 const roots=fresh.filter(e=>{for(let p=parent(e);p;p=parent(p))if(freshSet.has(p))return false;return true});
 const describe=e=>{if(e.id)return e.tagName.toLowerCase()+'#'+e.id;const c=(e.getAttribute('class')||'').trim().split(/\\s+/).filter(Boolean).slice(0,2);return e.tagName.toLowerCase()+(c.length?'.'+c.join('.'): '')};
 const color=e=>{for(let p=e;p;p=parent(p)){const c=getComputedStyle(p).backgroundColor;if(c!=='rgba(0, 0, 0, 0)'&&c!=='transparent')return c}const c=getComputedStyle(document.documentElement).backgroundColor;return c==='rgba(0, 0, 0, 0)'||c==='transparent'?'rgb(255, 255, 255)':c};
 const pageBG=color(document.body||document.documentElement);
 const visualText=el=>{
  const doc=el.ownerDocument,walker=doc.createTreeWalker(el,NodeFilter.SHOW_TEXT),nodes=[];let node;
  while((node=walker.nextNode()))if(node.nodeValue.trim()&&visible(node.parentElement))nodes.push(node);
  const rects=[];for(const textNode of nodes){const r=textNode.ownerDocument.createRange();r.selectNodeContents(textNode);for(const x of r.getClientRects())if(x.height>0)rects.push({top:x.top,bottom:x.bottom})}
  rects.sort((a,b)=>a.top-b.top);const rows=[];for(const r of rects){const row=rows[rows.length-1];if(row&&r.top<row.bottom&&r.bottom>row.top){row.top=Math.min(row.top,r.top);row.bottom=Math.max(row.bottom,r.bottom)}else rows.push({top:r.top,bottom:r.bottom})}
  const firstRow=rows[0]||{top:0,bottom:0};let first='';for(const textNode of nodes){const text=textNode.nodeValue;for(let i=0;i<text.length;i++){const r=textNode.ownerDocument.createRange();r.setStart(textNode,i);r.setEnd(textNode,i+1);const x=r.getBoundingClientRect();if(x.height>0&&x.top<firstRow.bottom&&x.bottom>firstRow.top)first+=text[i]}}
  return {lines:Math.max(1,rows.length),first:first.replace(/\\s+/g,' ').trim()};
 };
 const surface=root=>{const subtree=[root,...root.querySelectorAll('*')];for(const el of subtree.slice())if(el.shadowRoot)subtree.push(...el.shadowRoot.querySelectorAll('*'));const colored=subtree.filter(visible).map(el=>({el,style:getComputedStyle(el),r:el.getBoundingClientRect()})).filter(x=>x.style.backgroundColor!=='rgba(0, 0, 0, 0)'&&x.style.backgroundColor!=='transparent');colored.sort((a,b)=>{const depth=e=>{let n=0;for(;e&&e!==root;e=parent(e))n++;return n};return depth(b.el)-depth(a.el)||b.r.width*b.r.height-a.r.width*a.r.height});return colored[0]?.style.backgroundColor||color(root)};
 return roots.map(el=>{const r=el.getBoundingClientRect(),text=visualText(el);return {nodeID:id(el),selector:describe(el),text:text.first.slice(0,100),lines:text.lines,height:r.height,bg:surface(el),pageBg:pageBG}}).filter(x=>x.text);
})()`

const hoverAwaitJS = `new Promise(resolve=>{const start=performance.now(),collect=()=> ( __COLLECT_JS__ ),poll=()=>{const bubbles=collect();if(bubbles.length||performance.now()-start>=__WAIT_MS__)resolve(bubbles);else setTimeout(poll,25)};if(__WAIT_MS__<=75)setTimeout(()=>resolve(collect()),__WAIT_MS__);else poll()})`

func setColorScheme(ctx context.Context, scheme string) error {
	return chromedp.Do(ctx, chromedp.Func(func(ctx context.Context, target *chromedp.Target) error {
		_, err := cdp.Call(ctx, target, emulation.SetEmulatedMedia, emulation.SetEmulatedMediaParams{Features: []*emulation.MediaFeature{{Name: "prefers-color-scheme", Value: scheme}}})
		return err
	}))
}

func checkHoverBubbles(ctx context.Context, theme string, viewport float64, frame string) ([]hoverWarning, error) {
	if err := setColorScheme(ctx, theme); err != nil {
		return nil, fmt.Errorf("тема %s: %w", theme, err)
	}
	prepareJS := strings.ReplaceAll(hoverPrepareJS, "__MAX_HOVER_CANDIDATES__", strconv.Itoa(maxHoverCandidates))
	prepareJS = strings.ReplaceAll(prepareJS, "__QUICK_WAIT_MS__", strconv.FormatInt((25*time.Millisecond).Milliseconds(), 10))
	prepareJS = strings.ReplaceAll(prepareJS, "__MAX_WAIT_MS__", strconv.FormatInt(hoverWaitLimit.Milliseconds(), 10))
	prepare, err := chromedp.Run(ctx, chromedp.Evaluate[[]hoverCandidate](prepareJS, chromedp.EvalWithCommandLineAPI))
	if err != nil {
		return nil, fmt.Errorf("поиск кандидатов наведения: %w", err)
	}
	return scanHoverCandidates(ctx, theme, viewport, frame, prepare)
}

func scanHoverCandidates(ctx context.Context, theme string, viewport float64, frame string, prepare []hoverCandidate) ([]hoverWarning, error) {
	// A page may reuse one tooltip element for every trigger, so a node is
	// checked on each show and only a finding already reported is skipped.
	type finding struct {
		node int
		kind string
	}
	warnings, reported := make([]hoverWarning, 0), make(map[finding]bool)
	for _, candidate := range prepare {
		index := strconv.Itoa(candidate.Index)
		if _, err := chromedp.Run(ctx, chromedp.Func(func(ctx context.Context, target *chromedp.Target) error {
			if _, err := chromedp.MouseEvent(chromedp.MouseMoved, 0, 0)(ctx, target); err != nil {
				return err
			}
			_, err := chromedp.Evaluate[bool](strings.ReplaceAll(hoverScrollJS, "__CANDIDATE_INDEX__", index), chromedp.EvalAwaitPromise)(ctx, target)
			return err
		})); err != nil {
			return nil, fmt.Errorf("прокрутка к кандидату: %w", err)
		}
		beforeJS := strings.ReplaceAll(hoverSnapshotBeforeJS, "__CANDIDATE_INDEX__", index)
		point, err := chromedp.Run(ctx, chromedp.Evaluate[*mousePoint](beforeJS))
		if err != nil {
			return nil, fmt.Errorf("снимок до наведения: %w", err)
		}
		if point == nil {
			continue
		}
		if err := chromedp.Do(ctx, chromedp.MouseEvent(chromedp.MouseMoved, point.X, point.Y)); err != nil {
			return nil, fmt.Errorf("наведение мыши: %w", err)
		}
		waitJS := strings.ReplaceAll(hoverAwaitJS, "__COLLECT_JS__", hoverCollectJS)
		waitJS = strings.ReplaceAll(waitJS, "__WAIT_MS__", strconv.Itoa(candidate.WaitMS))
		bubbles, err := chromedp.Run(ctx, chromedp.Evaluate[[]hoverBubble](waitJS, chromedp.EvalAwaitPromise))
		if err != nil {
			return nil, fmt.Errorf("ожидание пузыря: %w", err)
		}
		framePath, captured := hoverFramePath(frame, candidate.Index+1), false
		for _, bubble := range bubbles {
			kinds := make([]string, 0, 2)
			if isLightBubbleOnDarkPage(bubble.BG, bubble.PageBG) {
				kinds = append(kinds, "фон")
			}
			if bubble.Lines > 12 || bubble.Height > viewport/3 {
				kinds = append(kinds, "объём")
			}
			for _, kind := range kinds {
				key := finding{bubble.NodeID, kind}
				if reported[key] {
					continue
				}
				reported[key] = true
				warnings = append(warnings, hoverWarning{Bubble: bubble, Theme: theme, Frame: framePath, Kind: kind})
				if captured {
					continue
				}
				buf, err := chromedp.Run(ctx, chromedp.CaptureScreenshot())
				if err != nil {
					return nil, fmt.Errorf("кадр при наведении: %w", err)
				}
				if err := save(framePath, buf); err != nil {
					return nil, err
				}
				captured = true
			}
		}
	}
	return warnings, nil
}

type mousePoint struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

func hoverFramePath(base string, candidate int) string {
	ext := filepath.Ext(base)
	return strings.TrimSuffix(base, ext) + fmt.Sprintf("-%02d", candidate) + ext
}

func isLightBubbleOnDarkPage(bubble, page string) bool {
	b, err := colorBrightness(bubble)
	if err != nil {
		return false
	}
	p, err := colorBrightness(page)
	return err == nil && b >= lightBubbleBrightness && p <= darkPageBrightness
}

func colorBrightness(color string) (float64, error) {
	start, end := strings.IndexByte(color, '('), strings.IndexByte(color, ')')
	if start < 0 || end <= start {
		return 0, fmt.Errorf("неподдерживаемый формат цвета")
	}
	channels := strings.Split(color[start+1:end], ",")
	if len(channels) < 3 {
		return 0, fmt.Errorf("ожидалось три цветовых канала")
	}
	var values [3]float64
	for i := range values {
		value, err := strconv.ParseFloat(strings.TrimSpace(channels[i]), 64)
		if err != nil {
			return 0, fmt.Errorf("цветовой канал %d: %w", i+1, err)
		}
		values[i] = value
	}
	return (redBrightnessWeight*values[0] + greenBrightnessWeight*values[1] + blueBrightnessWeight*values[2]) / brightnessWeightTotal, nil
}
