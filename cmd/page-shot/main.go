// page-shot снимает кадры HTML-страницы в headless Chrome на нескольких окнах
// и полную высоту, чтобы страницу смотрели глазами до публикации, и на каждом
// окне проверяет горизонтальный скролл и наложение подписей друг на друга:
// что-то из этого есть - код выхода 2.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
)

// viewport - окно съёмки в CSS px; scale - deviceScaleFactor (масштаб экрана).
type viewport struct {
	width, height int64
	scale         float64
}

// name - имя кадра: ШИРИНАxВЫСОТА, при масштабе не 1 - ещё @МАСШТАБ.
func (v viewport) name() string {
	n := fmt.Sprintf("%dx%d", v.width, v.height)
	if v.scale != 1 {
		n += "@" + strconv.FormatFloat(v.scale, 'f', -1, 64)
	}
	return n
}

// Окна съёмки. 1536x860@1.25 - типичный ноутбук: экран 1920x1200 при
// масштабе KDE 125% даёт 1536x960 CSS px, минус панель и интерфейс браузера;
// дробный масштаб ломает раскладку и даёт горизонтальный скролл, которого нет при 1.
// Мобильную эмуляцию не включаем: у страниц артефактов нет <meta viewport> (его
// добавляет обёртка при публикации), и мобильный режим рисует их уменьшенной
// десктопной раскладкой. Окно 390 CSS px даёт ту же раскладку, что на телефоне.
// Числа таблицы — сами окна, имя каждому дал бы только повтор его размеров.
var viewports = []viewport{
	{1440, 900, 1},    //nolint:magic-number
	{1536, 860, 1.25}, //nolint:magic-number
	{1366, 768, 1},    //nolint:magic-number
	{390, 844, 1},
}

// Коды выхода: 1 - съёмка не удалась, 2 - кадры сняты, но есть горизонтальный
// скролл или наложение подписей.
const (
	exitError  = 1
	exitLayout = 2
)

var (
	errOverflow = errors.New("горизонтальный скролл")
	errOverlap  = errors.New("наложение подписей")
)

func main() {
	err := run()
	switch {
	case err == nil:
	case errors.Is(err, errOverflow), errors.Is(err, errOverlap):
		fmt.Fprintln(os.Stderr, "page-shot:", err)
		os.Exit(exitLayout)
	default:
		fmt.Fprintln(os.Stderr, "page-shot:", err)
		os.Exit(exitError)
	}
}

func run() error {
	fs := flag.NewFlagSet("page-shot", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: page-shot [--at 2s,6s] [--out DIR] [--element CSS] <file.html|URL>")
		fs.PrintDefaults()
		fmt.Fprintln(fs.Output(), "--element: только окно 1536x860@1.25, каждый элемент целиком в <id>.png (без id - el-<n>.png), без проверок раскладки.")
		fmt.Fprintln(fs.Output(), "Код выхода 2 - на каком-то окне горизонтальный скролл или подписи наложились, подробности в stderr.")
	}
	at := fs.String("at", "1s", "моменты съёмки после загрузки через запятую (анимация - несколько)")
	out := fs.String("out", "", "папка кадров (по умолчанию <файл>-shots рядом со страницей)")
	timeout := fs.Duration("timeout", 2*time.Minute, "общий таймаут")
	selector := fs.String("element", "", "CSS-селектор: снять каждый подходящий элемент отдельным png в последний момент --at")
	// flag останавливается на первом позиционном аргументе, а флаги пишут и после файла.
	var positional []string
	for args := os.Args[1:]; ; {
		if err := fs.Parse(args); err != nil {
			return fmt.Errorf("аргументы: %w", err)
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(positional) != 1 {
		fs.Usage()
		return errors.New("нужен ровно один файл или URL")
	}
	moments, err := parseMoments(*at)
	if err != nil {
		return err
	}
	target, defaultOut, err := resolveTarget(positional[0])
	if err != nil {
		return err
	}
	dir := *out
	if dir == "" {
		dir = defaultOut
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("папка кадров %s: %w", dir, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("hide-scrollbars", true))...)
	defer cancelAlloc()

	if *selector != "" {
		return shootElements(allocCtx, target, dir, *selector, moments[len(moments)-1])
	}
	var overflowed, overlapped []string
	for _, vp := range viewports {
		lay, err := shoot(allocCtx, target, dir, vp, moments)
		if err != nil {
			return fmt.Errorf("%s: %w", vp.name(), err)
		}
		if lay.overflow != nil {
			fmt.Fprint(os.Stderr, lay.overflow.report(vp.name()))
			overflowed = append(overflowed, vp.name())
		}
		if lay.overlap != nil {
			fmt.Fprint(os.Stderr, lay.overlap.report(vp.name()))
			overlapped = append(overlapped, vp.name())
		}
	}
	var errs []error
	if len(overflowed) > 0 {
		errs = append(errs, fmt.Errorf("%w на окнах %s", errOverflow, strings.Join(overflowed, ", ")))
	}
	if len(overlapped) > 0 {
		errs = append(errs, fmt.Errorf("%w на окнах %s", errOverlap, strings.Join(overlapped, ", ")))
	}
	return errors.Join(errs...)
}

// layout - находки раскладки на окне: переполнение по ширине и наложение подписей.
type layout struct {
	overflow *overflow
	overlap  *overlapResult
}

// shoot открывает страницу заново на каждом окне: анимация и раскладка идут с нуля.
// Возвращает находки раскладки в последний момент съёмки.
func shoot(allocCtx context.Context, target, dir string, vp viewport, moments []time.Duration) (layout, error) {
	tabCtx, cancel := chromedp.NewContext(allocCtx)
	defer cancel()
	emulate := chromedp.EmulateViewport(vp.width, vp.height, chromedp.EmulateScale(vp.scale))
	if err := chromedp.Do(tabCtx, emulate, chromedp.Navigate(target)); err != nil {
		return layout{}, fmt.Errorf("открыть страницу: %w", err)
	}
	var lay layout
	var elapsed time.Duration
	for i, m := range moments {
		if err := chromedp.Do(tabCtx, chromedp.Sleep(m-elapsed)); err != nil {
			return layout{}, fmt.Errorf("ожидание до %s: %w", m, err)
		}
		elapsed = m
		suffix := ""
		if len(moments) > 1 {
			suffix = "-" + m.String()
		}
		buf, err := chromedp.Run(tabCtx, chromedp.CaptureScreenshot())
		if err != nil {
			return layout{}, fmt.Errorf("кадр %s: %w", m, err)
		}
		if err := save(filepath.Join(dir, vp.name()+suffix+".png"), buf); err != nil {
			return layout{}, err
		}
		// Полная высота - один раз, в последний момент, когда страница уже дорисована.
		// Качество 100 - PNG.
		if i == len(moments)-1 {
			if lay.overflow, err = checkOverflow(tabCtx); err != nil {
				return layout{}, err
			}
			if lay.overlap, err = checkOverlap(tabCtx); err != nil {
				return layout{}, err
			}
			buf, err := chromedp.Run(tabCtx, chromedp.FullScreenshot(100))
			if err != nil {
				return layout{}, fmt.Errorf("кадр полной высоты: %w", err)
			}
			if err := save(filepath.Join(dir, vp.name()+"-full.png"), buf); err != nil {
				return layout{}, err
			}
		}
	}
	return lay, nil
}

func save(path string, buf []byte) error {
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		return fmt.Errorf("запись кадра: %w", err)
	}
	fmt.Println(path)
	return nil
}

func parseMoments(s string) ([]time.Duration, error) {
	var res []time.Duration
	for part := range strings.SplitSeq(s, ",") {
		d, err := time.ParseDuration(strings.TrimSpace(part))
		if err != nil {
			return nil, fmt.Errorf("--at %q: %w", part, err)
		}
		if d < 0 || (len(res) > 0 && d <= res[len(res)-1]) {
			return nil, fmt.Errorf("--at: моменты должны идти по возрастанию и быть неотрицательными: %q", s)
		}
		res = append(res, d)
	}
	return res, nil
}

// resolveTarget превращает путь к файлу в file:// URL; URL оставляет как есть.
func resolveTarget(arg string) (target, outDir string, err error) {
	if u, perr := url.Parse(arg); perr == nil && (u.Scheme == "http" || u.Scheme == "https") {
		return arg, filepath.Join(".", "page-shots"), nil
	}
	abs, err := filepath.Abs(arg)
	if err != nil {
		return "", "", fmt.Errorf("путь к странице: %w", err)
	}
	if _, err := os.Stat(abs); err != nil {
		return "", "", fmt.Errorf("страница: %w", err)
	}
	base := strings.TrimSuffix(filepath.Base(abs), filepath.Ext(abs))
	return (&url.URL{Scheme: "file", Path: abs}).String(), filepath.Join(filepath.Dir(abs), base+"-shots"), nil
}

// overflow - горизонтальное переполнение страницы: документ шире окна.
type overflow struct {
	ScrollWidth int          `json:"scrollWidth"`
	ClientWidth int          `json:"clientWidth"`
	Culprits    []overflower `json:"culprits"`
}

// overflower - элемент, который вылезает правее окна.
type overflower struct {
	Selector string  `json:"selector"`
	Right    float64 `json:"right"`
}

// maxCulprits - сколько вылезающих элементов показывать на окно.
const maxCulprits = 5

// overflowTolerance - допуск переполнения в CSS px (объяснение у overflowJS);
// overflowToleranceJS - то же число для скрипта.
const (
	overflowTolerance   = 1
	overflowToleranceJS = "1"
)

// overflowJS меряет документ и ищет элементы правее окна. Допуск 1 px: при
// дробном масштабе (1.25) scrollWidth и границы округляются и дают лишний пиксель
// без всякого скролла. Виновник - либо элемент, чей бокс вылезает, а бокс родителя
// нет (его потомки вылезают вслед за ним и в список не идут), либо элемент, чей
// бокс влезает, а своё содержимое (длинная строка в pre) вылезает и ни один
// дочерний элемент не вылезает. Внутри предка с overflow-x не visible скролла
// страницы нет, такие элементы пропускаются.
const overflowJS = `(() => {
  const tol = ` + overflowToleranceJS + `;
  const se = document.scrollingElement || document.documentElement;
  const cw = se.clientWidth;
  const res = {scrollWidth: se.scrollWidth, clientWidth: cw, culprits: []};
  if (se.scrollWidth - cw <= tol || !document.body) return res;
  const out = x => x > cw + tol;
  const boxRight = el => el.getBoundingClientRect().right;
  const extent = el => {
    const r = el.getBoundingClientRect();
    return getComputedStyle(el).overflowX === 'visible' ? Math.max(r.right, r.left + el.scrollWidth) : r.right;
  };
  const clipped = el => {
    for (let p = el.parentElement; p && p !== document.body; p = p.parentElement) {
      if (getComputedStyle(p).overflowX !== 'visible') return true;
    }
    return false;
  };
  const describe = el => {
    const tag = el.tagName.toLowerCase();
    if (el.id) return tag + '#' + el.id;
    const cls = typeof el.className === 'string' ? el.className.trim().split(/\s+/).filter(Boolean).slice(0, 2) : [];
    return cls.length ? tag + '.' + cls.join('.') : tag;
  };
  const found = [];
  for (const el of document.body.querySelectorAll('*')) {
    const ext = extent(el);
    if (!out(ext) || clipped(el)) continue;
    const p = el.parentElement;
    const ownBox = out(boxRight(el)) && !(p && p !== document.body && out(boxRight(p)));
    const ownContent = !out(boxRight(el)) && ![...el.children].some(c => out(extent(c)));
    if (ownBox || ownContent) found.push({selector: describe(el), right: Math.round(ext * 100) / 100});
  }
  found.sort((a, b) => b.right - a.right);
  res.culprits = found;
  return res;
})()`

// checkOverflow возвращает переполнение по ширине или nil, если страница влезает.
func checkOverflow(ctx context.Context) (*overflow, error) {
	ov, err := chromedp.Run(ctx, chromedp.Evaluate[overflow](overflowJS))
	if err != nil {
		return nil, fmt.Errorf("проверка ширины: %w", err)
	}
	if ov.ScrollWidth-ov.ClientWidth <= overflowTolerance {
		return nil, nil
	}
	return &ov, nil
}

// report - текст для stderr: окно, на сколько шире, вылезающие элементы.
func (o *overflow) report(name string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "page-shot: %s: горизонтальный скролл, страница шире окна на %d px (scrollWidth %d, окно %d)\n",
		name, o.ScrollWidth-o.ClientWidth, o.ScrollWidth, o.ClientWidth)
	if len(o.Culprits) == 0 {
		b.WriteString("  вылезающих элементов не нашлось (возможно, псевдоэлемент или отступ)\n")
	}
	for i, c := range o.Culprits {
		if i == maxCulprits {
			fmt.Fprintf(&b, "  ...и ещё %d\n", len(o.Culprits)-maxCulprits)
			break
		}
		fmt.Fprintf(&b, "  %s right=%g\n", c.Selector, c.Right)
	}
	return b.String()
}
