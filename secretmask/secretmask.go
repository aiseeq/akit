// Package secretmask hides secret values in text before it goes further: to a
// log, a report or a model of another provider. It replaces each match with
// [скрыто:<kind>] and counts matches by kind; it never returns a value.
//
// Detection has deliberate limits:
//   - Short fragments after a bare letter or digit, and generic sk values
//     without digits, can leak.
//   - Base64url under generic sk can split into short fragments: at length 32,
//     0.47% after a space and 28.34% after a letter.
//   - Escapes outside escapeRE grammar can leak, including byte 0x9b.
//   - rk_, pk_ and sk_ names with test/live can be corrupted when a fragment
//     is at least 16 characters and contains a digit (see
//     network_test_HandlesUtf8Input).
//   - At a left boundary, digit-bearing names can be corrupted, including
//     sk-learn-contrib-imbalanced-2024-01, sk_buff_data_end_offset_2_bytes,
//     --sk-color-primary-500-hover-dark and xoxo-hugs-and-kisses-2026.
//     A hyphen or underscore neighbor can expose the same false positive, as
//     in task-sk-learn-contrib-imbalanced-2024-01.
package secretmask

import (
	"regexp"
	"sort"
	"strings"
)

var (
	privateKeyRE = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY(?: BLOCK)?-----[\s\S]*?(?:-----END [A-Z ]*PRIVATE KEY(?: BLOCK)?-----|\z)`)
	awsRE        = regexp.MustCompile(`AKIA[0-9A-Z]{16}`)
	escapeRE     = regexp.MustCompile(`(?:\\[A-Za-z]|\\[0-7]{1,3}|\\x[0-9A-Fa-f]{2}|\\u[0-9A-Fa-f]{4}|\\u\{[0-9A-Fa-f]+\}|\\U[0-9A-Fa-f]{8}|%(?:25)*[0-9A-Fa-f]{2}|=[0-9A-F]{2}|&#[0-9]+;?|&#x[0-9A-Fa-f]+;?|(?:\x1b|\\[eE]|\\033|\\33|\\x1[bB]|\\u001[bB]|\^\[|␛)\[[0-9;?]*[A-Za-z]|\x1b\(B|\[[0-9;]+m)$`)
)

type bodyClass uint8

const (
	alphaNum bodyClass = iota
	alphaUnderscore
	alphaDash
	tokenChars
)

type acceptance uint8

const (
	acceptDigitAndRun acceptance = iota
	acceptGenericSk
)

const (
	skAntMinimumLength       = 27
	githubPatMinimumLength   = 41
	githubTokenMinimumLength = 34
	googleTokenMinimumLength = 34
)

type pattern struct {
	kind       string
	variants   []tokenVariant
	acceptance acceptance
	firstBytes string
	re         *regexp.Regexp
}

type tokenVariant struct {
	prefix    string
	body      bodyClass
	minLength int
}

// patterns is the single ordered source for all secret kinds. Generic sk is
// last so it sees the text after more specific tokens have been replaced.
var patterns = [...]pattern{
	{kind: "private-key", re: privateKeyRE},
	{kind: "sk_live", variants: []tokenVariant{{"sk_live_", alphaNum, 24}, {"sk_test_", alphaNum, 24}, {"rk_live_", alphaNum, 24}, {"rk_test_", alphaNum, 24}, {"pk_live_", alphaNum, 24}, {"pk_test_", alphaNum, 24}}, acceptance: acceptDigitAndRun, firstBytes: "srp"},
	{kind: "sk", variants: []tokenVariant{{"sk-proj-", tokenChars, 28}, {"sk-ant-", tokenChars, skAntMinimumLength}}, acceptance: acceptDigitAndRun, firstBytes: "s"},
	{kind: "github", variants: []tokenVariant{{"ghp_", alphaNum, githubTokenMinimumLength}, {"gho_", alphaNum, githubTokenMinimumLength}, {"ghu_", alphaNum, githubTokenMinimumLength}, {"ghs_", alphaNum, githubTokenMinimumLength}, {"ghr_", alphaNum, githubTokenMinimumLength}, {"github_pat_", alphaUnderscore, githubPatMinimumLength}}, acceptance: acceptDigitAndRun, firstBytes: "g"},
	{kind: "slack", variants: []tokenVariant{{"xoxa-", alphaDash, 15}, {"xoxb-", alphaDash, 15}, {"xoxe-", alphaDash, 15}, {"xoxo-", alphaDash, 15}, {"xoxp-", alphaDash, 15}, {"xoxr-", alphaDash, 15}, {"xoxs-", alphaDash, 15}}, acceptance: acceptDigitAndRun, firstBytes: "x"},
	{kind: "aws", re: awsRE},
	{kind: "google", variants: []tokenVariant{{"AIza", tokenChars, googleTokenMinimumLength}}, acceptance: acceptDigitAndRun, firstBytes: "A"},
	{kind: "sk", variants: []tokenVariant{{"sk-", tokenChars, 23}, {"sk_", tokenChars, 23}}, acceptance: acceptGenericSk, firstBytes: "s"},
}

// Mask replaces every secret in text with [скрыто:<kind>] and returns the text
// and the number of matches by kind (empty, not nil, when there are none).
func Mask(text string) (string, map[string]int) {
	found := map[string]int{}
	var scan *scanIndex
	for _, p := range patterns {
		var indices [][]int
		if p.re != nil {
			indices = p.re.FindAllStringIndex(text, -1)
		} else {
			var indexed *scanIndex
			indices, indexed = findTokenMatches(text, p, scan)
			if indexed != nil {
				scan = indexed
			}
		}
		if len(indices) != 0 {
			text = replaceMatches(text, indices, p.kind, found)
			scan = nil // replacements invalidate byte offsets and run metadata
		}
	}
	return text, found
}

func replaceMatches(text string, indices [][]int, kind string, found map[string]int) string {
	if len(indices) == 0 {
		return text
	}
	var out strings.Builder
	last := 0
	for _, index := range indices {
		start, end := index[0], index[1]
		out.WriteString(text[last:start])
		out.WriteString("[скрыто:")
		out.WriteString(kind)
		out.WriteByte(']')
		last = end
		found[kind]++
	}
	out.WriteString(text[last:])
	return out.String()
}

func findTokenMatches(text string, p pattern, scan *scanIndex) ([][]int, *scanIndex) {
	var accepted [][]int
	var bodyEnds [4]bodyEndCache
	for offset := 0; offset < len(text); {
		relative := strings.IndexAny(text[offset:], p.firstBytes)
		if relative < 0 {
			break
		}
		offset += relative
		variant, matched := matchingPrefix(text, offset, p.variants)
		if !matched {
			offset++
			continue
		}
		body := offset + len(variant.prefix)
		end := bodyEnd(text, body, variant.body, &bodyEnds)
		if end-body < variant.minLength-len(variant.prefix) {
			offset++
			continue
		}
		if scan == nil {
			scan = &scanIndex{}
		}
		if scan.runs == nil {
			scan.runs = newRunIndex(text)
		}
		if acceptsToken(text, offset, body, end, p, scan.runs) {
			accepted = append(accepted, []int{offset, end})
			offset = end
		} else {
			offset++
		}
	}
	return accepted, scan
}

func matchingPrefix(text string, start int, variants []tokenVariant) (tokenVariant, bool) {
	for _, variant := range variants {
		if strings.HasPrefix(text[start:], variant.prefix) {
			return variant, true
		}
	}
	return tokenVariant{}, false
}

func acceptsToken(text string, start, body, end int, p pattern, runs *runIndex) bool {
	if p.acceptance == acceptGenericSk {
		if runs.has(body, end, genericAnyRun) {
			return true
		}
		return (boundaryBefore(text, start) || start > 0 && text[start-1] == '_') && runs.hasDigit(start, end)
	}
	return runs.hasDigit(start, end) && (boundaryBefore(text, start) || runs.has(start, end, 16))
}

type scanIndex struct {
	runs *runIndex
}

type bodyEndCache struct {
	start, end int
	valid      bool
}

func bodyEnd(text string, start int, class bodyClass, caches *[4]bodyEndCache) int {
	cached := &caches[class]
	if cached.valid && start >= cached.start && start < cached.end {
		return cached.end
	}
	end := start
	for end < len(text) && bodyContains(class, text[end]) {
		end++
	}
	if !cached.valid || start >= cached.start {
		*cached = bodyEndCache{start: start, end: end, valid: true}
	}
	return end
}

func bodyContains(class bodyClass, c byte) bool {
	if isAlphaNum(c) {
		return true
	}
	switch class {
	case alphaUnderscore:
		return c == '_'
	case alphaDash:
		return c == '-'
	case tokenChars:
		return c == '_' || c == '-'
	default:
		return false
	}
}

type runIndex struct {
	starts, ends    []int
	letters, digits []int
	next16, next20  []int
}

func newRunIndex(text string) *runIndex {
	r := &runIndex{letters: make([]int, len(text)+1), digits: make([]int, len(text)+1)}
	for i := 0; i < len(text); i++ {
		c := text[i]
		r.letters[i+1], r.digits[i+1] = r.letters[i], r.digits[i]
		if isLetter(c) {
			r.letters[i+1]++
		}
		if c >= '0' && c <= '9' {
			r.digits[i+1]++
		}
	}
	for start := 0; start < len(text); {
		if !isAlphaNum(text[start]) {
			start++
			continue
		}
		end := start
		for end < len(text) && isAlphaNum(text[end]) {
			end++
		}
		r.starts = append(r.starts, start)
		r.ends = append(r.ends, end)
		start = end
	}
	r.next16, r.next20 = make([]int, len(r.ends)+1), make([]int, len(r.ends)+1)
	r.next16[len(r.ends)], r.next20[len(r.ends)] = len(r.ends), len(r.ends)
	for i := len(r.ends) - 1; i >= 0; i-- {
		good := func(n int) bool {
			return r.ends[i]-r.starts[i] >= n && r.letters[r.ends[i]] > r.letters[r.starts[i]] && r.digits[r.ends[i]] > r.digits[r.starts[i]]
		}
		r.next16[i], r.next20[i] = r.next16[i+1], r.next20[i+1]
		if good(16) {
			r.next16[i] = i
		}
		if good(20) {
			r.next20[i] = i
		}
	}
	return r
}

func (r *runIndex) hasDigit(start, end int) bool {
	return r.digits[end] > r.digits[start]
}

func (r *runIndex) has(start, end, minimum int) bool {
	i := sort.Search(len(r.ends), func(i int) bool { return r.ends[i] >= start+minimum })
	if i == len(r.ends) {
		return false
	}
	if r.hasRunPart(i, start, end, minimum) {
		return true
	}
	if minimum >= 20 {
		i = r.next20[i+1]
	} else {
		i = r.next16[i+1]
	}
	if i < len(r.ends) && r.starts[i] >= start && r.ends[i] <= end {
		return true
	}
	last := sort.Search(len(r.starts), func(i int) bool { return r.starts[i] > end }) - 1
	return last >= 0 && last < len(r.ends) && r.hasRunPart(last, start, end, minimum)
}

func (r *runIndex) hasRunPart(i, start, end, minimum int) bool {
	lo, hi := max(start, r.starts[i]), min(end, r.ends[i])
	return hi-lo >= minimum && r.letters[hi] > r.letters[lo] && r.digits[hi] > r.digits[lo]
}

// genericAnyRun is the random run that lets a generic key be masked after any
// neighbour: the shortest generic body (20) is a single run of this length.
const genericAnyRun = 20

func isAlphaNum(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
}

func isLetter(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z'
}

func boundaryBefore(text string, index int) bool {
	if index == 0 {
		return true
	}
	c := text[index-1]
	return !(isLetter(c) || c >= '0' && c <= '9') || escapeBefore(text, index)
}

func escapeBefore(text string, index int) bool {
	start := max(index-32, 0)
	return escapeRE.MatchString(text[start:index])
}
