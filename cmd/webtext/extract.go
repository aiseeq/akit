package main

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
	"golang.org/x/net/html/charset"
)

// skipped — элементы без текста для читателя.
var skipped = map[atom.Atom]bool{
	atom.Script: true, atom.Style: true, atom.Noscript: true, atom.Template: true, atom.Svg: true,
	atom.Iframe: true, atom.Head: true, atom.Button: true, atom.Select: true, atom.Canvas: true, atom.Object: true,
}

// chrome — обвязка сайта вокруг статьи: меню, шапка, подвал, боковые колонки. Внутри статьи шапку не трогаем — там
// её заголовок.
var chrome = map[atom.Atom]bool{atom.Nav: true, atom.Aside: true, atom.Header: true, atom.Footer: true}

// blocks — элементы, текст которых идёт с новой строки; 2 — абзац, отделённый пустой строкой.
var blocks = map[atom.Atom]int{
	atom.Div: 1, atom.Section: 1, atom.Article: 1, atom.Main: 1, atom.Li: 1, atom.Ul: 1, atom.Ol: 1, atom.Dl: 1,
	atom.Dt: 1, atom.Dd: 1, atom.Tr: 1, atom.Figcaption: 1, atom.Br: 1, atom.Header: 1, atom.Footer: 1, atom.Form: 1,
	atom.Fieldset: 1, atom.Address: 1, atom.Details: 1, atom.Summary: 1, atom.Caption: 1,
	atom.P: 2, atom.Blockquote: 2, atom.Pre: 2, atom.Table: 2, atom.Figure: 2, atom.Hr: 2, atom.H1: 2, atom.H2: 2,
	atom.H3: 2, atom.H4: 2, atom.H5: 2, atom.H6: 2,
}

// minArticle — статья или main короче этого — не тело страницы: карточка в ленте или пустая обёртка.
const minArticle = 500

// htmlText — заголовок и текст страницы. Тело — main, иначе article, если он один или больше половины страницы,
// иначе body без меню, шапки, подвала и боковых колонок.
func htmlText(body []byte, contentType string) (string, string, error) {
	r, err := charset.NewReader(bytes.NewReader(body), contentType)
	if err != nil {
		return "", "", fmt.Errorf("charset: %w", err)
	}
	doc, err := html.Parse(r)
	if err != nil {
		return "", "", fmt.Errorf("parse html: %w", err)
	}
	title := clean(textOf(find(doc, atom.Title), false))
	root, inArticle := bodyRoot(doc)
	text := tidy(render(root, inArticle))
	if title == "" {
		title, _, _ = strings.Cut(text, "\n")
	}
	return title, text, nil
}

// bodyRoot — узел с телом страницы и признак «это статья», при котором шапка внутри неё остаётся.
func bodyRoot(doc *html.Node) (*html.Node, bool) {
	body := find(doc, atom.Body)
	if body == nil {
		body = doc
	}
	for _, m := range findAll(doc, isMain) {
		if len(textOf(m, true)) >= minArticle {
			return m, true
		}
	}
	articles := findAll(doc, func(n *html.Node) bool { return n.DataAtom == atom.Article })
	var best *html.Node
	bestLen := 0
	for _, a := range articles {
		if l := len(textOf(a, true)); l > bestLen {
			best, bestLen = a, l
		}
	}
	if best != nil && bestLen >= minArticle && (len(articles) == 1 || 2*bestLen >= len(textOf(body, true))) {
		return best, true
	}
	return body, false
}

func isMain(n *html.Node) bool {
	return n.DataAtom == atom.Main || attr(n, "role") == "main"
}

func find(n *html.Node, a atom.Atom) *html.Node {
	if n.Type == html.ElementNode && n.DataAtom == a {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if f := find(c, a); f != nil {
			return f
		}
	}
	return nil
}

func findAll(n *html.Node, match func(*html.Node) bool) []*html.Node {
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && match(n) {
			out = append(out, n)
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return out
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// hidden — элемент, которого не видно: hidden, aria-hidden или display:none.
func hidden(n *html.Node) bool {
	for _, a := range n.Attr {
		switch a.Key {
		case "hidden":
			return true
		case "aria-hidden":
			if a.Val == "true" {
				return true
			}
		case "style":
			if strings.Contains(strings.ReplaceAll(strings.ToLower(a.Val), " ", ""), "display:none") {
				return true
			}
		}
	}
	return false
}

// textOf — весь видимый текст узла одной строкой; skipChrome пропускает и обвязку сайта.
func textOf(n *html.Node, skipChrome bool) string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		switch n.Type {
		case html.TextNode:
			b.WriteString(n.Data)
			return
		case html.ElementNode:
			if skipped[n.DataAtom] || hidden(n) || skipChrome && chrome[n.DataAtom] {
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return clean(b.String())
}

// render — текст узла с переводами строк по блокам; pre — как есть. В статье шапка остаётся, меню и колонки — нет.
func render(root *html.Node, inArticle bool) string {
	var b strings.Builder
	var walk func(n *html.Node, pre bool)
	walk = func(n *html.Node, pre bool) {
		switch n.Type {
		case html.TextNode:
			if pre {
				b.WriteString(n.Data)
			} else {
				b.WriteString(spaces.ReplaceAllString(n.Data, " "))
			}
			return
		case html.ElementNode:
			if skipped[n.DataAtom] || hidden(n) || chrome[n.DataAtom] && n != root &&
				!(inArticle && (n.DataAtom == atom.Header || n.DataAtom == atom.Footer)) {
				return
			}
		}
		breaks := 0
		if n.Type == html.ElementNode {
			breaks = blocks[n.DataAtom]
		}
		lineBreak(&b, breaks)
		if n.DataAtom == atom.Li {
			b.WriteString("- ")
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, pre || n.DataAtom == atom.Pre)
		}
		lineBreak(&b, breaks)
		if n.DataAtom == atom.Td || n.DataAtom == atom.Th {
			b.WriteString(" ")
		}
	}
	walk(root, false)
	return b.String()
}

// lineBreak доводит число переводов строки в конце текста до n: соседние блоки не плодят пустых строк.
func lineBreak(b *strings.Builder, n int) {
	s := b.String()
	have := len(s) - len(strings.TrimRight(s, " \n"))
	have = strings.Count(s[len(s)-have:], "\n")
	for ; have < n; have++ {
		b.WriteString("\n")
	}
}

var spaces = regexp.MustCompile(`\s+`)

func clean(s string) string { return strings.TrimSpace(spaces.ReplaceAllString(s, " ")) }

// tidy — строки без пробелов по краям, пустые подряд — одной.
func tidy(s string) string {
	var out []string
	blank := false
	for line := range strings.SplitSeq(s, "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "-"); ok && (rest == "" || rest[0] == ' ') {
			line = strings.TrimSpace("- " + strings.TrimSpace(rest)) // пункт списка; пустой — значок-ссылка
			if line == "-" {
				continue
			}
		}
		if line == "" {
			blank = len(out) > 0
			continue
		}
		if blank {
			out = append(out, "")
			blank = false
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// checkMarks — слова страниц проверки от ботов и отказа по частоте (Cloudflare, Akamai, Imperva, DataDome и
// сайты сами). В заголовке — всегда проверка; в тексте — только на короткой странице: статья может их цитировать.
var checkMarks = []string{
	"just a moment", "checking your browser", "browser check", "attention required", "rate limited", "access denied",
	"enable javascript and cookies", "verify you are human", "verifying you are human", "are you a robot",
	"request unsuccessful", "pardon our interruption", "too many requests", "ddos protection by",
	"please enable cookies", "security check", "captcha", "bot verification", "unusual traffic",
}

// shortPage — текст длиннее этого — статья, а не страница проверки.
const shortPage = 3000

func challenge(title, text string) bool {
	t := strings.ToLower(title)
	low := strings.ToLower(text)
	for _, m := range checkMarks {
		if strings.Contains(t, m) || len(text) < shortPage && strings.Contains(low, m) {
			return true
		}
	}
	return false
}
