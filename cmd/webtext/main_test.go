package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func readFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Статья (синтетика по образцу снимка Wayback): заголовок, тело, без меню, скриптов и пустых пунктов.
func TestHTMLText(t *testing.T) {
	title, text, err := htmlText(readFile(t, "article.html"), "text/html; charset=utf-8")
	if err != nil {
		t.Fatal(err)
	}
	if title != "Independent review finds the city library network needs reform" {
		t.Errorf("title %q", title)
	}
	if !strings.Contains(text, "finding that the current framework is not fully delivering on its statutory objectives") {
		t.Errorf("нет фразы из тела статьи:\n%s", text)
	}
	for _, bad := range []string{"\n-\n", "\n\n\n", "window.dataLayer", "function("} {
		if strings.Contains(text, bad) {
			t.Errorf("в тексте %q", bad)
		}
	}
	if challenge(title, text) {
		t.Error("статья принята за проверку")
	}
}

func TestChallengePages(t *testing.T) {
	for _, name := range []string{"cloudflare.html", "browser-check.html"} {
		title, text, err := htmlText(readFile(t, name), "text/html")
		if err != nil {
			t.Fatal(err)
		}
		if !challenge(title, text) {
			t.Errorf("%s: проверка не узнана: «%s» %q", name, title, text[:min(len(text), 200)])
		}
	}
}

func TestHTMLStructure(t *testing.T) {
	page := `<html><head><title>T</title><script>var x=1</script></head><body>
<nav><a>Home</a><a>News</a></nav><header>Site header</header>
<main><h1>Head</h1><p>First   para
graph.</p><ul><li>one</li><li><a href="#"><svg></svg></a></li><li>two</li></ul>
<pre>a  b
c</pre><div hidden>secret</div><p style="display: none">gone</p>` + strings.Repeat("<p>filler text here.</p>", 40) + `</main>
<footer>Footer links</footer></body></html>`
	title, text, err := htmlText([]byte(page), "text/html")
	if err != nil {
		t.Fatal(err)
	}
	if title != "T" {
		t.Errorf("title %q", title)
	}
	want := "Head\n\nFirst para graph.\n\n- one\n- two\n\na  b\nc\n\nfiller text here."
	if !strings.HasPrefix(text, want) {
		t.Errorf("text:\n%q\nwant prefix:\n%q", text[:min(len(text), 120)], want)
	}
	for _, bad := range []string{"Home", "Site header", "Footer", "secret", "gone", "var x"} {
		if strings.Contains(text, bad) {
			t.Errorf("в тексте %q", bad)
		}
	}
}

func TestArchivable(t *testing.T) {
	cases := map[string]bool{
		"https://www.cbc.ca/news/a-b":                              true,
		"https://example.com/p?id=42&page=2":                       true,
		"http://localhost:8080/x":                                  false,
		"http://127.0.0.1/x":                                       false,
		"http://10.1.2.3/x":                                        false,
		"http://[::1]/x":                                           false,
		"http://100.100.1.1/x":                                     false,
		"https://app.test/x":                                       false,
		"https://printer.local/":                                   false,
		"http://intranet/x":                                        false,
		"https://user:pw@example.com/":                             false,
		"https://example.com/x?access_token=abc":                   false,
		"https://example.com/x?X-Amz-Signature=abc":                false,
		"https://example.com/x?api_key=1":                          false,
		"https://example.com/x?t=eyJhbGciOiJIUzI1NiJ9abc123def456": false,
	}
	for raw, want := range cases {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got, why := archivable(context.Background(), u); got != want {
			t.Errorf("%s: %v (%s), want %v", raw, got, why, want)
		}
	}
}

// fake — сайт и Wayback на одном тестовом сервере: /site отдаёт код site, /cdx — список снимков, /web/ — снимок.
type fake struct {
	srv       *httptest.Server
	siteHits  atomic.Int32
	cdxHits   atomic.Int32
	siteCode  int
	cdxBody   string
	snapshots map[string][]byte // метка → тело снимка
}

func newFake(t *testing.T, siteCode int, cdxBody string, snaps map[string][]byte) (*fake, *fetcher) {
	t.Helper()
	fk := &fake{siteCode: siteCode, cdxBody: cdxBody, snapshots: snaps}
	fk.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/site"):
			fk.siteHits.Add(1)
			if r.UserAgent() != userAgent() {
				t.Errorf("UA %q", r.UserAgent())
			}
			w.WriteHeader(fk.siteCode)
			fmt.Fprint(w, "<html><title>Just a moment...</title></html>")
		case r.URL.Path == "/cdx":
			fk.cdxHits.Add(1)
			if r.URL.Query().Get("filter") != "statuscode:200" {
				t.Errorf("cdx query %s", r.URL.RawQuery)
			}
			fmt.Fprint(w, fk.cdxBody)
		case strings.HasPrefix(r.URL.Path, "/web/"):
			ts, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/web/"), "id_/")
			body, ok := fk.snapshots[ts]
			if !ok {
				http.NotFound(w, r)
				return
			}
			if bytes.HasPrefix(body, []byte("%PDF")) {
				w.Header().Set("Content-Type", "application/pdf")
			} else {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
			}
			w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fk.srv.Close)
	f := newFetcher()
	f.cdx, f.web = fk.srv.URL+"/cdx", fk.srv.URL+"/web/"
	f.allow = func(context.Context, *url.URL) (bool, string) { return true, "" }
	return fk, f
}

// 429 — сайт не повторяется, сразу архив; последний снимок — проверка, берётся предыдущий.
func TestOpenWayback(t *testing.T) {
	fk, f := newFake(t, http.StatusTooManyRequests, "20250101000000 https://x/site\n20260208211225 https://x/site\n",
		map[string][]byte{"20250101000000": readFile(t, "article.html"), "20260208211225": readFile(t, "cloudflare.html")})
	target, _ := url.Parse(fk.srv.URL + "/site")
	res, pg := open(context.Background(), f, target, true)
	if !res.OK || res.Source != sourceWayback || res.Snapshot != "20250101000000" || res.Kind != "html" {
		t.Fatalf("res %+v", res)
	}
	if fk.siteHits.Load() != 1 {
		t.Errorf("сайт спрошен %d раз, на 429 — один", fk.siteHits.Load())
	}
	if !strings.Contains(pg.text, "statutory objectives") {
		t.Error("нет текста статьи")
	}
	if len(res.Steps) != 2 || res.Steps[0].Result != "429" ||
		res.Steps[1].Result != "20260208211225 200 bot check «Just a moment...»; 20250101000000 200 html" {
		t.Errorf("steps %+v", res.Steps)
	}
}

func TestOpenClosed(t *testing.T) {
	fk, f := newFake(t, http.StatusForbidden, "", nil)
	target, _ := url.Parse(fk.srv.URL + "/site/some-article-name")
	res, _ := open(context.Background(), f, target, true)
	if res.OK || !strings.Contains(res.Hint, "«some article name»") || res.Steps[1].Result != "no snapshot with status 200" {
		t.Errorf("res %+v", res)
	}
	if res, _ := open(context.Background(), f, target, false); len(res.Steps) != 1 || fk.siteHits.Load() != 1 {
		t.Errorf("--no-direct спросил сайт: %+v, hits %d", res.Steps, fk.siteHits.Load())
	}
	f.allow = archivable
	res, _ = open(context.Background(), f, target, true)
	if res.Steps[1].Result != "skipped: internal host" || fk.cdxHits.Load() != 2 {
		t.Errorf("внутренний адрес ушёл в архив: %+v, cdx %d", res.Steps, fk.cdxHits.Load())
	}
}

// Вызов целиком: --json, файл с заголовком и источником, коды выхода.
func TestRun(t *testing.T) {
	fk, f := newFake(t, http.StatusServiceUnavailable, "20260208211225 https://x/site\n",
		map[string][]byte{"20260208211225": readFile(t, "article.html")})
	out := filepath.Join(t.TempDir(), "page.txt")
	var stdout, stderr bytes.Buffer
	if code := runWith(f, []string{"--json", "--out", out, fk.srv.URL + "/site"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code %d: %s %s", code, stdout.String(), stderr.String())
	}
	var res result
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil || res.Path != out || res.Chars < minText {
		t.Fatalf("json %s: %v", stdout.String(), err)
	}
	body, _ := os.ReadFile(out)
	lines := strings.SplitN(string(body), "\n", 4)
	if lines[0] != res.Title || lines[1] != "source: https://web.archive.org/web/20260208211225id_/"+fk.srv.URL+"/site (Wayback snapshot 08.02.2026)" || lines[2] != "" {
		t.Errorf("шапка файла: %q", lines[:3])
	}
	stdout.Reset()
	fk.cdxBody = ""
	if code := runWith(f, []string{fk.srv.URL + "/site"}, &stdout, &stderr); code != exitClosed ||
		!strings.Contains(stdout.String(), "direct: 503") || !strings.Contains(stdout.String(), "next: WebSearch") {
		t.Errorf("закрытая страница: %d %s", code, stdout.String())
	}
	for _, args := range [][]string{{}, {"ftp://x/y"}, {"a", "b"}} {
		if code := runWith(f, args, &stdout, &stderr); code != exitUsage {
			t.Errorf("%q: code %d", args, code)
		}
	}
}

// --timeout — граница всех ступеней: медленный сайт и архив не держат вызов дольше неё, выход 1 с шагами.
func TestDeadline(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(slow.Close)
	f := newFetcher()
	f.cdx, f.web = slow.URL+"/cdx", slow.URL+"/web/"
	f.allow = func(context.Context, *url.URL) (bool, string) { return true, "" }
	var stdout, stderr bytes.Buffer
	start := time.Now()
	code := runWith(f, []string{"--json", "--timeout", "300ms", slow.URL + "/site"}, &stdout, &stderr)
	if took := time.Since(start); code != exitClosed || took > 2*time.Second {
		t.Fatalf("code %d за %v: %s", code, took, stdout.String())
	}
	var res result
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil || len(res.Steps) != 2 || res.Hint == "" {
		t.Errorf("json %s: %v", stdout.String(), err)
	}
}

func TestStatus202(t *testing.T) {
	fk, f := newFake(t, http.StatusAccepted, "", nil)
	target, _ := url.Parse(fk.srv.URL + "/site")
	if res, _ := open(context.Background(), f, target, true); res.OK || res.Steps[0].Result != "202" {
		t.Errorf("202 принят: %+v", res.Steps)
	}
}

// minimalPDF — одностраничный PDF с одной строкой текста.
const minimalPDF = `%PDF-1.4
1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj
2 0 obj<</Type/Pages/Kids[3 0 R]/Count 1>>endobj
3 0 obj<</Type/Page/Parent 2 0 R/MediaBox[0 0 300 100]/Contents 4 0 R/Resources<</Font<</F1 5 0 R>>>>>>endobj
4 0 obj<</Length 0>>stream
BT /F1 8 Tf 10 80 Td (StarCraft survey: real-time strategy game AI research, open problems and directions.) Tj
0 -12 Td (Planning, learning, opponent modeling and spatial reasoning under partial observability.) Tj
0 -12 Td (Micro-management, macro-management, build orders and tactical decisions in combat.) Tj ET
endstream endobj
5 0 obj<</Type/Font/Subtype/Type1/BaseFont/Helvetica>>endobj
trailer<</Root 1 0 R>>
%%EOF
`

func TestOpenPDF(t *testing.T) {
	if _, err := exec.LookPath("pdftotext"); err != nil {
		t.Skip("pdftotext not installed")
	}
	fk, f := newFake(t, http.StatusForbidden, "20250522121153 https://x/site\n",
		map[string][]byte{"20250522121153": []byte(minimalPDF)})
	target, _ := url.Parse(fk.srv.URL + "/site")
	_, pg, verdict := f.wayback(context.Background(), target)
	if pg.kind != "pdf" || !strings.Contains(pg.text, "StarCraft survey") {
		t.Errorf("pdf: %q %q %s", pg.kind, pg.text, verdict)
	}
}
