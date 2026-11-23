package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

type options struct {
	column, artifacts string
	page, snapshot    string
}

type block struct {
	start, end int
	name       string
	normalized string
	stamp      string
}

type openElement struct {
	tag      string
	selected bool
	block    int
}

func execute(args []string, now func() time.Time) (string, error) {
	opt, err := parseArgs(args)
	if err != nil {
		return "", err
	}
	if opt.snapshot == "" {
		if opt.artifacts == "" {
			return "", errors.New("укажите снимок или --artifacts DIR/ID")
		}
		opt.snapshot, err = latestSnapshot(opt.artifacts)
		if err != nil {
			return "", err
		}
	}
	pageBytes, err := os.ReadFile(opt.page)
	if err != nil {
		return "", fmt.Errorf("страница %s: %w", opt.page, err)
	}
	pageInfo, err := os.Stat(opt.page)
	if err != nil {
		return "", fmt.Errorf("страница %s: %w", opt.page, err)
	}
	snapshotBytes, err := os.ReadFile(opt.snapshot)
	if err != nil {
		return "", fmt.Errorf("снимок %s: %w", opt.snapshot, err)
	}
	current, err := parseBlocks(pageBytes, opt.column)
	if err != nil {
		return "", fmt.Errorf("страница %s: %w", opt.page, err)
	}
	previous, err := parseBlocks(snapshotBytes, opt.column)
	if err != nil {
		return "", fmt.Errorf("снимок %s: %w", opt.snapshot, err)
	}
	stamp := now().Format("2006-01-02T15:04-07:00")
	available := make(map[string]int, len(previous))
	for _, b := range previous {
		available[b.normalized]++
	}
	var edits []block
	for _, b := range current {
		if available[b.normalized] > 0 {
			available[b.normalized]--
			continue
		}
		if alreadyStamped(b, previous) {
			continue
		}
		b.stamp = stamp
		edits = append(edits, b)
	}
	if len(edits) > 0 {
		updated := append([]byte(nil), pageBytes...)
		for i := len(edits) - 1; i >= 0; i-- {
			b := edits[i]
			raw, err := setDataUpd(updated[b.start:b.end], b.stamp)
			if err != nil {
				return "", fmt.Errorf("блок %s: %w", b.name, err)
			}
			updated = append(updated[:b.start], append(raw, updated[b.end:]...)...)
		}
		if err := os.WriteFile(opt.page, updated, pageInfo.Mode().Perm()); err != nil {
			return "", fmt.Errorf("записать страницу %s: %w", opt.page, err)
		}
	}
	labels := make([]string, len(edits))
	for i, b := range edits {
		labels[i] = b.name
	}
	if len(labels) == 0 {
		return "page-upd: помечено блоков: 0", nil
	}
	return fmt.Sprintf("page-upd: помечено блоков: %d (%s)", len(edits), strings.Join(labels, ", ")), nil
}

func alreadyStamped(current block, previous []block) bool {
	if !dataUpdStamp.MatchString(current.stamp) {
		return false
	}
	for _, old := range previous {
		if current.stamp == old.stamp {
			return false
		}
	}
	return true
}

var dataUpdStamp = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}[+-]\d{2}:\d{2}$`)

func parseArgs(args []string) (options, error) {
	opt := options{column: "main"}
	var positional []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-h", "--help":
			return opt, errHelp
		case "--column", "--artifacts":
			if i+1 == len(args) {
				return opt, fmt.Errorf("после %s нужно значение", args[i])
			}
			i++
			if args[i-1] == "--column" {
				opt.column = args[i]
			} else {
				opt.artifacts = args[i]
			}
		default:
			if strings.HasPrefix(args[i], "-") {
				return opt, fmt.Errorf("неизвестный флаг %s", args[i])
			}
			positional = append(positional, args[i])
		}
	}
	if len(positional) < 1 || len(positional) > 2 {
		return opt, errors.New("нужны файл страницы и необязательный файл снимка")
	}
	opt.page = positional[0]
	if len(positional) == 2 {
		opt.snapshot = positional[1]
	}
	if !validSelector(opt.column) {
		return opt, fmt.Errorf("неподдерживаемый селектор столбца %q (нужен tag, #id или .class)", opt.column)
	}
	return opt, nil
}

func validSelector(s string) bool {
	return regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]*$|^#[A-Za-z_][A-Za-z0-9_-]*$|^\.[A-Za-z_][A-Za-z0-9_-]*$`).MatchString(s)
}

func latestSnapshot(root string) (string, error) {
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && (strings.EqualFold(filepath.Ext(path), ".html") || strings.EqualFold(filepath.Base(path), "index.html")) {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("поиск снимка в %s: %w", root, err)
	}
	if len(paths) == 0 {
		return "", fmt.Errorf("в %s нет файлов .html", root)
	}
	sort.Strings(paths)
	return paths[len(paths)-1], nil
}

func parseBlocks(source []byte, selector string) ([]block, error) {
	if _, err := html.Parse(bytes.NewReader(source)); err != nil {
		return nil, fmt.Errorf("некорректный HTML: %w", err)
	}
	blocks, columnFound, err := scanBlocks(source, selector)
	if err != nil {
		return nil, err
	}
	if !columnFound {
		return nil, fmt.Errorf("столбец %q не найден", selector)
	}
	for i := range blocks {
		if blocks[i].normalized != "" {
			continue
		}
		blocks[i].normalized, err = normalizeHTML(source[blocks[i].start:blocks[i].end])
		if err != nil {
			return nil, fmt.Errorf("блок %s: нормализация HTML: %w", blocks[i].name, err)
		}
	}
	return blocks, nil
}

func scanBlocks(source []byte, selector string) ([]block, bool, error) {
	z := html.NewTokenizer(bytes.NewReader(source))
	var stack []openElement
	var blocks []block
	columnFound := false
	offset := 0
	for {
		typ := z.Next()
		if typ == html.ErrorToken {
			if err := z.Err(); err != io.EOF {
				return nil, false, fmt.Errorf("HTML около байта %d: %w", offset, err)
			}
			break
		}
		raw := append([]byte(nil), z.Raw()...)
		start := offset
		end := start + len(raw)
		offset = end
		var err error
		stack, blocks, columnFound, err = consumeToken(typ, z.Token(), raw, start, end, source, selector, stack, blocks, columnFound)
		if err != nil {
			return nil, false, err
		}
	}
	for _, el := range stack {
		if el.selected {
			return nil, false, fmt.Errorf("HTML: не закрыт столбец %s", el.tag)
		}
		if el.block >= 0 {
			return nil, false, fmt.Errorf("HTML: не закрыт тег блока %s", el.tag)
		}
	}
	return blocks, columnFound, nil
}

func consumeToken(typ html.TokenType, tok html.Token, raw []byte, start, end int, source []byte, selector string, stack []openElement, blocks []block, columnFound bool) ([]openElement, []block, bool, error) {
	switch typ {
	case html.StartTagToken, html.SelfClosingTagToken:
		tag := strings.ToLower(tok.Data)
		if closeAt := implicitCloseIndex(stack, tag); closeAt >= 0 {
			var err error
			stack, blocks, err = closeElements(stack, closeAt, start, source, blocks)
			if err != nil {
				return nil, nil, columnFound, err
			}
		}
		parentSelected := len(stack) > 0 && stack[len(stack)-1].selected
		selected := !columnFound && matches(tok, selector)
		columnFound = columnFound || selected
		bi := -1
		if parentSelected {
			blocks = append(blocks, block{start: start, end: end, name: shortName(tok), stamp: attr(tok, "data-upd")})
			bi = len(blocks) - 1
		}
		if voidTag(tag) || typ == html.SelfClosingTagToken && inForeignContent(stack) {
			if bi >= 0 {
				normalized, err := normalizeHTML(raw)
				if err != nil {
					return nil, nil, columnFound, err
				}
				blocks[bi].normalized = normalized
			}
			return stack, blocks, columnFound, nil
		}
		stack = append(stack, openElement{tag: tag, selected: selected, block: bi})
	case html.EndTagToken:
		return consumeEndTag(tok, start, end, source, stack, blocks, columnFound)
	}
	return stack, blocks, columnFound, nil
}

func consumeEndTag(tok html.Token, start, end int, source []byte, stack []openElement, blocks []block, columnFound bool) ([]openElement, []block, bool, error) {
	tag := strings.ToLower(tok.Data)
	for i := len(stack) - 1; i >= 0; i-- {
		if stack[i].tag != tag {
			continue
		}
		var err error
		stack, blocks, err = closeElements(stack, i+1, start, source, blocks)
		if err != nil {
			return nil, nil, columnFound, err
		}
		stack, blocks, err = closeElement(stack, i, end, source, blocks)
		return stack[:i], blocks, columnFound, err
	}
	return stack, blocks, columnFound, nil
}

func closeElements(stack []openElement, from, end int, source []byte, blocks []block) ([]openElement, []block, error) {
	for i := len(stack) - 1; i >= from; i-- {
		var err error
		stack, blocks, err = closeElement(stack, i, end, source, blocks)
		if err != nil {
			return nil, nil, err
		}
	}
	return stack[:from], blocks, nil
}

func closeElement(stack []openElement, index, end int, source []byte, blocks []block) ([]openElement, []block, error) {
	el := stack[index]
	if el.block >= 0 {
		blocks[el.block].end = end
		normalized, err := normalizeHTML(source[blocks[el.block].start:end])
		if err != nil {
			return nil, nil, err
		}
		blocks[el.block].normalized = normalized
	}
	return stack, blocks, nil
}

func implicitCloseIndex(stack []openElement, tag string) int {
	for i := len(stack) - 1; i >= 0; i-- {
		if closesOpenTag(stack[i].tag, tag) {
			return i
		}
		if stack[i].tag == "table" || stack[i].tag == "ul" || stack[i].tag == "ol" {
			break
		}
	}
	return -1
}

func closesOpenTag(open, next string) bool {
	if open == "p" {
		switch next {
		case "address", "article", "aside", "blockquote", "div", "dl", "fieldset", "footer", "form", "h1", "h2", "h3", "h4", "h5", "h6", "header", "hgroup", "hr", "main", "menu", "nav", "ol", "p", "pre", "section", "table", "ul":
			return true
		}
	}
	if open == "li" && next == "li" || (open == "dt" || open == "dd") && (next == "dt" || next == "dd") || open == "option" && (next == "option" || next == "optgroup") {
		return true
	}
	if (open == "td" || open == "th") && (next == "td" || next == "th") || open == "tr" && next == "tr" {
		return true
	}
	return false
}

func inForeignContent(stack []openElement) bool {
	for i := len(stack) - 1; i >= 0; i-- {
		if stack[i].tag == "svg" || stack[i].tag == "math" {
			return true
		}
		if stack[i].tag == "body" {
			return false
		}
	}
	return false
}

func matches(tok html.Token, selector string) bool {
	if strings.HasPrefix(selector, "#") {
		return attr(tok, "id") == selector[1:]
	}
	if strings.HasPrefix(selector, ".") {
		for class := range strings.FieldsSeq(attr(tok, "class")) {
			if class == selector[1:] {
				return true
			}
		}
		return false
	}
	return strings.EqualFold(tok.Data, selector)
}

func attr(tok html.Token, key string) string {
	for _, a := range tok.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

func shortName(tok html.Token) string {
	if id := attr(tok, "id"); id != "" {
		return "#" + id
	}
	classes := strings.Fields(attr(tok, "class"))
	if len(classes) > 0 {
		return tok.Data + "." + classes[0]
	}
	return tok.Data
}

func voidTag(tag string) bool {
	switch tag {
	case "area", "base", "br", "col", "embed", "hr", "img", "input", "link", "meta", "param", "source", "track", "wbr":
		return true
	default:
		return false
	}
}

func normalizeHTML(source []byte) (string, error) {
	context := &html.Node{Type: html.ElementNode, Data: "div", DataAtom: atom.Div}
	nodes, err := html.ParseFragment(bytes.NewReader(source), context)
	if err != nil {
		return "", fmt.Errorf("разобрать фрагмент: %w", err)
	}
	var b bytes.Buffer
	for _, n := range nodes {
		stripUpd(n)
		if err := html.Render(&b, n); err != nil {
			return "", fmt.Errorf("сериализовать узел: %w", err)
		}
	}
	return b.String(), nil
}

func stripUpd(n *html.Node) {
	if n.Type == html.ElementNode {
		attrs := n.Attr[:0]
		for _, a := range n.Attr {
			if !strings.EqualFold(a.Key, "data-upd") {
				attrs = append(attrs, a)
			}
		}
		sort.SliceStable(attrs, func(i, j int) bool {
			if strings.EqualFold(attrs[i].Key, attrs[j].Key) {
				return attrs[i].Key < attrs[j].Key
			}
			return strings.ToLower(attrs[i].Key) < strings.ToLower(attrs[j].Key)
		})
		n.Attr = attrs
	}
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		stripUpd(child)
	}
}

func setDataUpd(source []byte, stamp string) ([]byte, error) {
	if len(source) == 0 || source[0] != '<' {
		return nil, errors.New("не найден открывающий тег")
	}
	openEnd := openingTagEnd(source)
	if openEnd < 0 {
		return nil, errors.New("открывающий тег не закрыт")
	}
	if start, end, found, hasValue := dataUpdSpan(source[:openEnd]); found {
		if !hasValue {
			return splice(source, end, end, []byte(`="`+stamp+`"`)), nil
		}
		return splice(source, start, end, []byte(`"`+stamp+`"`)), nil
	}
	insert := openEnd - 1
	if insert > 0 && source[insert-1] == '/' {
		insert--
	}
	return append(append(append([]byte(nil), source[:insert]...), []byte(` data-upd="`+stamp+`"`)...), source[insert:]...), nil
}

func dataUpdSpan(tag []byte) (int, int, bool, bool) {
	z := html.NewTokenizer(bytes.NewReader(tag))
	typ := z.Next()
	if typ != html.StartTagToken && typ != html.SelfClosingTagToken {
		return 0, 0, false, false
	}
	parsed := false
	for _, a := range z.Token().Attr {
		if strings.EqualFold(a.Key, "data-upd") {
			parsed = true
			break
		}
	}
	if !parsed {
		return 0, 0, false, false
	}
	return findAttributeSpan(tag, "data-upd")
}

func findAttributeSpan(tag []byte, target string) (int, int, bool, bool) {
	i := 1
	for i < len(tag) && !htmlSpace(tag[i]) && tag[i] != '/' && tag[i] != '>' {
		i++
	}
	for i < len(tag) {
		for i < len(tag) && (htmlSpace(tag[i]) || tag[i] == '/') {
			i++
		}
		if i >= len(tag) || tag[i] == '>' {
			return 0, 0, false, false
		}
		name, valueStart, valueEnd, hasValue, next := readAttribute(tag, i)
		if strings.EqualFold(name, target) {
			return valueStart, valueEnd, true, hasValue
		}
		i = next
	}
	return 0, 0, false, false
}

func readAttribute(tag []byte, start int) (string, int, int, bool, int) {
	nameStart := start
	for start < len(tag) && !htmlSpace(tag[start]) && tag[start] != '=' && tag[start] != '/' && tag[start] != '>' {
		start++
	}
	name := string(tag[nameStart:start])
	for start < len(tag) && htmlSpace(tag[start]) {
		start++
	}
	if start >= len(tag) || tag[start] != '=' {
		return name, start, start, false, start
	}
	start++
	for start < len(tag) && htmlSpace(tag[start]) {
		start++
	}
	valueStart := start
	start = scanAttributeValue(tag, start)
	return name, valueStart, start, true, start
}

func scanAttributeValue(tag []byte, start int) int {
	if start < len(tag) && (tag[start] == '"' || tag[start] == '\'') {
		quote := tag[start]
		start++
		for start < len(tag) && tag[start] != quote {
			start++
		}
		if start < len(tag) {
			start++
		}
		return start
	}
	for start < len(tag) && !htmlSpace(tag[start]) && tag[start] != '>' {
		start++
	}
	return start
}

func htmlSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\f'
}

func splice(source []byte, start, end int, replacement []byte) []byte {
	out := make([]byte, 0, len(source)-(end-start)+len(replacement))
	out = append(out, source[:start]...)
	out = append(out, replacement...)
	out = append(out, source[end:]...)
	return out
}

func openingTagEnd(source []byte) int {
	var quote byte
	for i, ch := range source {
		if quote != 0 {
			if ch == quote {
				quote = 0
			}
			continue
		}
		if ch == '"' || ch == '\'' {
			quote = ch
		} else if ch == '>' {
			return i + 1
		}
	}
	return -1
}
