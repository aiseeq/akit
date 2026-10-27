package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"time"
)

const (
	sourceDirect  = "direct"
	sourceWayback = "wayback"
	waybackWeb    = "https://web.archive.org/web/"
	// maxBody — потолок тела ответа: страница или PDF больше этого агенту текстом не нужны.
	maxBody = 30 << 20
	// stepTimeout — потолок одного запроса: медленный сайт не съедает время архива.
	stepTimeout = 25 * time.Second
	// minText — меньше слов на странице — это не статья: проверка, заглушка или страница на одном JS.
	minText = 200
	// snapshots — сколько последних снимков пробовать: в последнем бывает та же проверка, снятая со статусом 200.
	snapshots = 3
)

// step — итог одной ступени для отчёта.
type step struct {
	Step   string `json:"step"`
	Result string `json:"result"`
}

// page — открытая страница: что и откуда.
type page struct {
	status      int
	contentType string
	body        []byte

	kind, title string
	text        string
}

type fetcher struct {
	client *http.Client
	ua     string
	cdx    string // адрес CDX API; в тестах — свой сервер
	web    string // префикс снимков Wayback
	pdf    func(ctx context.Context, body []byte) (string, error)
	// allow — можно ли отдать адрес архиву; в тестах сайт на 127.0.0.1, и правило подменяется.
	allow func(ctx context.Context, u *url.URL) (bool, string)
}

func newFetcher() *fetcher {
	tr, _ := http.DefaultTransport.(*http.Transport)
	tr = tr.Clone()
	tr.ForceAttemptHTTP2 = true
	return &fetcher{
		client: &http.Client{Transport: tr},
		ua:     userAgent(),
		cdx:    "https://web.archive.org/cdx/search/cdx",
		web:    waybackWeb,
		pdf:    pdfText,
		allow:  archivable,
	}
}

// userAgent — короткое честное имя с версией akit. Длинное, со словами «AI agent», режут часть сайтов (замер на
// новостном сайте); имени обычного браузера нет и не будет.
func userAgent() string {
	v := "dev"
	if bi, ok := debug.ReadBuildInfo(); ok && strings.HasPrefix(bi.Main.Version, "v") {
		v, _, _ = strings.Cut(bi.Main.Version, "-")
		v = strings.TrimSuffix(v, "+dirty")
	}
	return "akit-webtext/" + v
}

// open проходит ступени: прямой запрос (direct=false — сайт уже ответил 429 и не спрашивается), затем снимки
// Wayback. Прямой запрос не повторяется.
func open(ctx context.Context, f *fetcher, target *url.URL, direct bool) (result, page) {
	res := result{URL: target.String()}
	hintTitle := ""
	if direct {
		pg, verdict, err := f.direct(ctx, target)
		if err == nil {
			res.Steps = append(res.Steps, step{sourceDirect, verdict})
			res.fill(sourceDirect, "", pg)
			return res, pg
		}
		res.Steps = append(res.Steps, step{sourceDirect, err.Error()})
		hintTitle = pg.title
	}
	if ok, why := f.allow(ctx, target); !ok {
		res.Steps = append(res.Steps, step{sourceWayback, "skipped: " + why})
		res.Hint = hint(hintTitle, target)
		return res, page{}
	}
	ts, pg, verdict := f.wayback(ctx, target)
	res.Steps = append(res.Steps, step{sourceWayback, verdict})
	if ts != "" {
		res.fill(sourceWayback, ts, pg)
		return res, pg
	}
	res.Hint = hint(hintTitle, target)
	return res, page{}
}

func (r *result) fill(source, snapshot string, pg page) {
	r.OK, r.Source, r.Snapshot = true, source, snapshot
	r.Status, r.Kind, r.Title, r.Chars = pg.status, pg.kind, pg.title, len([]rune(pg.text))
}

// hint — что делать агенту, когда не открылось ничем.
func hint(title string, target *url.URL) string {
	if title == "" {
		title = slug(target)
	}
	return fmt.Sprintf("WebSearch for «%s» to find a copy; in an interactive session open the page in the user's "+
		"Chrome through the extension; otherwise tell the user the page is unavailable", title)
}

// slug — последний кусок пути словами: заголовок, когда сама страница его не дала.
func slug(u *url.URL) string {
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	s := parts[len(parts)-1]
	s = strings.TrimSuffix(s, ".html")
	s = strings.NewReplacer("-", " ", "_", " ").Replace(s)
	if s == "" {
		return u.Host
	}
	return s
}

// direct — прямой запрос: страница и итог ступени; ошибка — страница не открылась, в ней итог для отчёта.
func (f *fetcher) direct(ctx context.Context, target *url.URL) (page, string, error) {
	pg, err := f.get(ctx, target.String())
	if err != nil {
		return pg, "", fmt.Errorf("error: %w", err)
	}
	return f.judge(ctx, pg)
}

// judge решает, открылась ли страница: статус, тип, проверка от ботов, есть ли текст. Итог открытой — строкой,
// отказ — ошибкой с тем же видом итога («403», «200 bot check «…»»).
func (f *fetcher) judge(ctx context.Context, pg page) (page, string, error) {
	if pg.status != http.StatusOK && pg.status != http.StatusNonAuthoritativeInfo {
		// 202 с пустой страницей — проверка AWS WAF (замер на сайте под WAF), не страница
		return pg, "", fmt.Errorf("%d", pg.status)
	}
	if err := f.extract(ctx, &pg); err != nil {
		return pg, "", fmt.Errorf("%d %w", pg.status, err)
	}
	if challenge(pg.title, pg.text) {
		title := pg.title
		pg.title = "" // заголовок проверки — не заголовок статьи
		return pg, "", fmt.Errorf("%d bot check «%s»", pg.status, title)
	}
	if n := len([]rune(pg.text)); n < minText {
		return pg, "", fmt.Errorf("%d %s with %d chars of text", pg.status, pg.kind, n)
	}
	return pg, fmt.Sprintf("%d %s", pg.status, pg.kind), nil
}

// mediaType — тип из Content-Type без параметров, строчными.
func mediaType(contentType string) string {
	mt, _, _ := strings.Cut(contentType, ";")
	return strings.ToLower(strings.TrimSpace(mt))
}

// extract — текст по типу содержимого: HTML, PDF или простой текст.
func (f *fetcher) extract(ctx context.Context, pg *page) error {
	mt := mediaType(pg.contentType)
	switch {
	case mt == "application/pdf" || bytes.HasPrefix(pg.body, []byte("%PDF-")):
		text, err := f.pdf(ctx, pg.body)
		if err != nil {
			return err
		}
		pg.kind, pg.text = "pdf", text
		pg.title, _, _ = strings.Cut(strings.TrimSpace(text), "\n")
	case mt == "" || mt == "text/html" || mt == "application/xhtml+xml" || looksHTML(pg.body):
		title, text, err := htmlText(pg.body, pg.contentType)
		if err != nil {
			return err
		}
		pg.kind, pg.title, pg.text = "html", title, text
	case strings.HasPrefix(mt, "text/") || mt == "application/json" || strings.HasSuffix(mt, "+json") ||
		strings.HasSuffix(mt, "xml"):
		pg.kind, pg.text = "text", strings.TrimSpace(string(pg.body))
	default:
		return fmt.Errorf("unsupported content type %s", mt)
	}
	return nil
}

func looksHTML(body []byte) bool {
	head := bytes.ToLower(bytes.TrimSpace(body[:min(len(body), 512)]))
	return bytes.HasPrefix(head, []byte("<!doctype html")) || bytes.HasPrefix(head, []byte("<html"))
}

func (f *fetcher) get(ctx context.Context, rawURL string) (page, error) {
	ctx, cancel := context.WithTimeout(ctx, stepTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return page{}, fmt.Errorf("request: %w", err)
	}
	req.Header.Set("User-Agent", f.ua)
	// */*, как у curl: Accept браузера при небраузерном имени Cloudflare режет 403 (замер на сайте за Cloudflare)
	req.Header.Set("Accept", "*/*")
	resp, err := f.client.Do(req)
	if err != nil {
		return page{}, shortErr(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return page{}, shortErr(err)
	}
	return page{status: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), body: body}, nil
}

// shortErr — ошибка запроса без повтора адреса: он и так в отчёте.
func shortErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// wayback — последний снимок со статусом 200, где есть текст: метка снимка, страница и итог ступени.
func (f *fetcher) wayback(ctx context.Context, target *url.URL) (string, page, string) {
	q := url.Values{"url": {target.String()}, "filter": {"statuscode:200"}, "fl": {"timestamp,original"},
		"limit": {fmt.Sprint(-snapshots)}}
	list, err := f.get(ctx, f.cdx+"?"+q.Encode())
	if err != nil {
		return "", page{}, "cdx error: " + err.Error()
	}
	if list.status != http.StatusOK {
		return "", page{}, fmt.Sprintf("cdx %d", list.status)
	}
	type snap struct{ ts, original string }
	var snaps []snap
	sc := bufio.NewScanner(bytes.NewReader(list.body))
	for sc.Scan() {
		if ts, original, ok := strings.Cut(strings.TrimSpace(sc.Text()), " "); ok {
			snaps = append(snaps, snap{ts, original})
		}
	}
	if len(snaps) == 0 {
		return "", page{}, "no snapshot with status 200"
	}
	var tried []string
	for i := len(snaps) - 1; i >= 0; i-- {
		s := snaps[i]
		pg, err := f.get(ctx, f.web+s.ts+"id_/"+s.original)
		if err != nil {
			tried = append(tried, s.ts+" error: "+err.Error())
			continue
		}
		pg, verdict, err := f.judge(ctx, pg)
		if err != nil {
			tried = append(tried, s.ts+" "+err.Error())
			continue
		}
		return s.ts, pg, strings.Join(append(tried, s.ts+" "+verdict), "; ")
	}
	return "", page{}, strings.Join(tried, "; ")
}

// snapshotURL — снимок без обвязки Wayback (id_): страница как её отдал сайт.
func snapshotURL(ts, original string) string {
	return waybackWeb + ts + "id_/" + original
}
