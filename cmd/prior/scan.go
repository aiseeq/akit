package main

import (
	"bytes"
	"unicode"
	"unicode/utf8"
)

// lineScanner finds lines that may pass a run filter without case-folding the text. For every needle it
// searches one component: each variant is located through an anchor byte (the last byte of every case
// variant of one of its runes) and verified rune by rune around the anchor. A line that the filter can accept
// contains such a verified variant, so lines without one are skipped; the caller still checks every
// candidate line in full.
type lineScanner struct {
	literals []scanLiteral
}

type scanLiteral struct {
	runes   []rune // folded runes of the variant
	anchor  int    // index of the rune whose encodings are searched
	anchors []byte // last bytes of every encoding that folds to runes[anchor]
	// span is the most bytes an occurrence can take. The anchor search runs span bytes past limit, so the state
	// below stays exact for every start before limit; the lines nextLine returns would be the same without it,
	// because an occurrence that starts before limit and ends past it overlaps the match at limit and its line.
	span int
	// State for one text. No occurrence starts in [first from, clear); match is the first occurrence that
	// starts at or after clear (-1: there is none, -2: not searched yet). For each anchor byte, next is its
	// first position at or after the search cursor, or -1 when it does not occur before seen.
	clear int
	match int
	next  []int
	seen  []int
}

// scannerFor returns a fresh scanner for filter, copying the template prepared by newQuery when filter is q.runs.
func (q query) scannerFor(filter []runNeedle) (*lineScanner, bool) {
	runs := len(filter) == len(q.runs) && len(filter) > 0 && &filter[0] == &q.runs[0]
	if runs && q.runsScan != nil {
		return q.runsScan.clone(), true
	}
	if runs && q.runsScanTried {
		return nil, false
	}
	template, ok := newLineScanner(filter)
	if !ok {
		return nil, false
	}
	return template.clone(), true
}

// clone copies a template scanner with its own state for one text.
func (s *lineScanner) clone() *lineScanner {
	c := &lineScanner{literals: append([]scanLiteral(nil), s.literals...)}
	for i := range c.literals {
		l := &c.literals[i]
		l.next = make([]int, len(l.anchors))
		l.seen = make([]int, len(l.anchors))
		for j := range l.next {
			l.next[j] = -1
		}
	}
	return c
}

// newLineScanner returns a template scanner for filter, to be cloned per text, or false when some needle cannot
// be searched this way (no filter, an empty variant, or a variant with U+FFFD, which also stands for invalid
// bytes).
func newLineScanner(filter []runNeedle) (*lineScanner, bool) {
	if len(filter) == 0 {
		return nil, false
	}
	s := &lineScanner{}
	for _, needle := range filter {
		best, bestCost := -1, 0.0
		var bestLiterals []scanLiteral
		for c, variants := range needle {
			var literals []scanLiteral
			cost := 0.0
			usable := true
			for _, variant := range variants {
				if variant == "" || !utf8.ValidString(variant) || bytes.ContainsRune([]byte(variant), utf8.RuneError) {
					usable = false
					break
				}
				if bytes.IndexByte([]byte(variant), '\n') >= 0 {
					continue // never inside one line
				}
				literal := newScanLiteral(variant)
				literals = append(literals, literal)
				cost += anchorCost(literal.anchors)
			}
			if !usable {
				continue
			}
			if best < 0 || cost < bestCost {
				best, bestCost, bestLiterals = c, cost, literals
			}
		}
		if best < 0 {
			return nil, false
		}
		s.literals = append(s.literals, bestLiterals...)
	}
	return s, true
}

func newScanLiteral(variant string) scanLiteral {
	literal := scanLiteral{runes: []rune(variant), match: -2}
	literal.span = len(literal.runes) * utf8.UTFMax
	bestCost := -1.0
	for i, r := range literal.runes {
		anchors := runeAnchors(r)
		if cost := anchorCost(anchors); bestCost < 0 || cost < bestCost {
			literal.anchor, literal.anchors, bestCost = i, anchors, cost
		}
	}
	return literal
}

// runeAnchors lists the distinct last bytes of the encodings of every rune that folds to r.
func runeAnchors(r rune) []byte {
	var anchors []byte
	add := func(v rune) {
		var encoded [utf8.UTFMax]byte
		n := utf8.EncodeRune(encoded[:], v)
		if bytes.IndexByte(anchors, encoded[n-1]) < 0 {
			anchors = append(anchors, encoded[n-1])
		}
	}
	add(r)
	for v := unicode.SimpleFold(r); v != r; v = unicode.SimpleFold(v) {
		add(v)
	}
	return anchors
}

func anchorCost(anchors []byte) float64 {
	cost := 0.0
	for _, b := range anchors {
		cost += anchorByteCost[b]
	}
	return cost
}

// anchorByteCost estimates how often a byte ends a character in Russian and English text, so that the scanner
// anchors on rarer bytes. It affects only speed, never which lines are found.
var anchorByteCost = buildAnchorByteCost()

func buildAnchorByteCost() [256]float64 {
	var cost [256]float64
	for b := range cost {
		cost[b] = 0.5
	}
	for _, c := range " \t,.:;-()[]/`'\"*#|_=<>" {
		cost[c] = 10
	}
	english := map[byte]float64{
		'e': 12, 't': 9, 'a': 8, 'o': 7.5, 'i': 7, 'n': 6.7, 's': 6.3, 'h': 6, 'r': 6, 'd': 4.3, 'l': 4, 'c': 2.8,
		'u': 2.8, 'm': 2.4, 'w': 2.4, 'f': 2.2, 'g': 2, 'y': 2, 'p': 1.9, 'b': 1.5, 'v': 1, 'k': 0.8, 'j': 0.15,
		'x': 0.15, 'q': 0.1, 'z': 0.07,
	}
	for b, f := range english {
		cost[b] = f
		cost[b-'a'+'A'] = f / 5
	}
	for b := byte('0'); b <= '9'; b++ {
		cost[b] = 2
	}
	russian := []float64{ // а..я
		8, 1.6, 4.5, 1.7, 3, 8.5, 0.9, 1.6, 7.4, 1.2, 3.5, 4.4, 3.2, 6.7, 11, 2.8,
		4.7, 5.5, 6.3, 2.6, 0.3, 1, 0.5, 1.4, 0.7, 0.4, 0.04, 1.9, 1.7, 0.3, 0.6, 2,
	}
	for i, f := range russian {
		lower, upper := 'а'+rune(i), 'А'+rune(i)
		cost[lastByte(lower)] += f
		cost[lastByte(upper)] += f / 5
	}
	for _, r := range "—–«»“”’…→" {
		cost[lastByte(r)] += 1
	}
	return cost
}

func lastByte(r rune) byte {
	var encoded [utf8.UTFMax]byte
	n := utf8.EncodeRune(encoded[:], r)
	return encoded[n-1]
}

// nextLine returns the start of the first line at or after from that may pass the filter. Calls for one text
// must come with line starts in increasing order.
func (s *lineScanner) nextLine(data []byte, from int) (int, bool) {
	first := -1
	limit := len(data) + 1
	for i := range s.literals {
		if at := s.literals[i].before(data, from, limit); at >= 0 {
			first, limit = at, at
		}
	}
	if first < 0 {
		return 0, false
	}
	return bytes.LastIndexByte(data[from:first], '\n') + 1 + from, true
}

// before returns the start of the first occurrence that starts in [from, limit), or -1 when there is none.
// The search stops near limit, so a literal that occurs late or never does not cost a scan of the whole text
// once another literal has matched earlier.
func (l *scanLiteral) before(data []byte, from, limit int) int {
	if l.clear < from {
		l.clear = from
		if l.match >= 0 && l.match < from {
			l.match = -2
		}
	}
	if l.match == -2 {
		l.search(data, limit)
	}
	if l.match >= 0 && l.match < limit {
		return l.match
	}
	return -1
}

// search looks for the first occurrence starting at or after l.clear and below limit. Occurrences are met in
// the order of their anchors, which is also the order of their starts.
func (l *scanLiteral) search(data []byte, limit int) {
	anchorLimit := min(len(data), limit+l.span)
	for cursor := l.clear; ; {
		p, which := -1, -1
		for i := range l.anchors {
			if l.next[i] < cursor {
				l.advance(data, i, cursor, anchorLimit)
			}
			if l.next[i] >= 0 && l.next[i] < anchorLimit && (p < 0 || l.next[i] < p) {
				p, which = l.next[i], i
			}
		}
		if p < 0 {
			if anchorLimit == len(data) {
				l.match = -1
			} else {
				l.clear = max(l.clear, limit)
			}
			return
		}
		if start, ok := l.verify(data, p); ok && start >= l.clear {
			l.match = start
			return
		}
		cursor = p + 1
		if l.next[which] < cursor {
			l.next[which] = -1
		}
	}
}

// advance sets l.next[i] to the first position of anchor byte i in [cursor, anchorLimit), or -1 when there is
// none; l.seen[i] remembers how far the byte is known to be absent, so that no stretch is searched twice.
func (l *scanLiteral) advance(data []byte, i, cursor, anchorLimit int) {
	l.next[i] = -1
	from := max(cursor, l.seen[i])
	if from >= anchorLimit {
		return
	}
	if at := bytes.IndexByte(data[from:anchorLimit], l.anchors[i]); at >= 0 {
		l.next[i] = from + at
	} else {
		l.seen[i] = anchorLimit
	}
}

// verify checks that the rune ending at data[p] and its neighbours fold to the literal.
func (l *scanLiteral) verify(data []byte, p int) (int, bool) {
	end := p + 1
	r, n := decodeLastRune(data[:end])
	if r == utf8.RuneError && n <= 1 || foldRuneFast(r) != l.runes[l.anchor] {
		return 0, false
	}
	start := end - n
	for k := l.anchor - 1; k >= 0; k-- {
		r, n = decodeLastRune(data[:start])
		if r == utf8.RuneError && n <= 1 || foldRuneFast(r) != l.runes[k] {
			return 0, false
		}
		start -= n
	}
	at := end
	for k := l.anchor + 1; k < len(l.runes); k++ {
		r, n = decodeRune(data[at:])
		if r == utf8.RuneError && n <= 1 || foldRuneFast(r) != l.runes[k] {
			return 0, false
		}
		at += n
	}
	return start, true
}

// decodeRune is utf8.DecodeRune with ASCII and valid two-byte sequences decoded inline.
func decodeRune(p []byte) (rune, int) {
	if len(p) > 0 && p[0] < utf8.RuneSelf {
		return rune(p[0]), 1
	}
	if len(p) > 1 && p[0] >= 0xC2 && p[0] < 0xE0 && p[1]&0xC0 == 0x80 {
		return rune(p[0]&0x1F)<<6 | rune(p[1]&0x3F), 2
	}
	return utf8.DecodeRune(p)
}

// decodeLastRune is utf8.DecodeLastRune with ASCII and valid two-byte sequences decoded inline.
func decodeLastRune(p []byte) (rune, int) {
	n := len(p)
	if n > 0 && p[n-1] < utf8.RuneSelf {
		return rune(p[n-1]), 1
	}
	if n > 1 && p[n-1]&0xC0 == 0x80 && p[n-2] >= 0xC2 && p[n-2] < 0xE0 {
		return rune(p[n-2]&0x1F)<<6 | rune(p[n-1]&0x3F), 2
	}
	return utf8.DecodeLastRune(p)
}
