// Package secretmask hides secret values in text before it goes further: to a
// log, a report or a model of another provider. It replaces each match with
// [скрыто:<kind>] and counts matches by kind; it never returns a value.
package secretmask

import (
	"regexp"
	"strings"
)

var (
	privateKeyRE    = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?(?:-----END [A-Z ]*PRIVATE KEY-----|\z)`)
	skLiveRE        = regexp.MustCompile(`(?:sk|rk|pk)_(?:live|test)_[0-9A-Za-z]{16,}`)
	skDistinctiveRE = regexp.MustCompile(`sk(?:-proj-|-ant-)[A-Za-z0-9_-]{20,}`)
	skAnyRE         = regexp.MustCompile(`sk[-_][A-Za-z0-9_-]{20,}`)
	githubRE        = regexp.MustCompile(`(?:gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{30,})`)
	slackRE         = regexp.MustCompile(`xox[abeoprs]-[A-Za-z0-9-]{10,}`)
	awsRE           = regexp.MustCompile(`AKIA[0-9A-Z]{16}`)
	googleRE        = regexp.MustCompile(`AIza[0-9A-Za-z_-]{30,}`)
	escapeRE        = regexp.MustCompile(`(?:\\[A-Za-z]|\\[0-7]{1,3}|\\x[0-9A-Fa-f]{2}|\\u[0-9A-Fa-f]{4}|%(?:25)*[0-9A-Fa-f]{2}|=[0-9A-F]{2}|&#[0-9]+;?|&#x[0-9A-Fa-f]+;?|(?:\x1b|\\[eE]|\\033|\\33|\\x1[bB]|\\u001[bB])\[[0-9;?]*[A-Za-z])$`)
)

type pattern struct {
	kind string
	re   *regexp.Regexp
	// digit asks for a digit in the match: a generated key always has one,
	// names and words mostly do not
	digit bool
	// gluedRandom: without a left boundary, the match must hold a random run
	// of this length (see hasRandomRun)
	gluedRandom int
}

// patterns apply private key blocks first, then distinctive prefixes, and the
// generic sk form last so it can match after a replacement marker.
func patterns() []pattern {
	return []pattern{
		{kind: "private-key", re: privateKeyRE},
		{kind: "sk_live", re: skLiveRE, digit: true, gluedRandom: 16},
		{kind: "sk", re: skDistinctiveRE, digit: true, gluedRandom: 16},
		{kind: "github", re: githubRE, digit: true, gluedRandom: 16},
		{kind: "slack", re: slackRE, digit: true, gluedRandom: 16},
		{kind: "aws", re: awsRE},
		{kind: "google", re: googleRE, digit: true, gluedRandom: 16},
	}
}

// Mask replaces every secret in text with [скрыто:<kind>] and returns the text
// and the number of matches by kind (empty, not nil, when there are none).
func Mask(text string) (string, map[string]int) {
	found := map[string]int{}
	for _, p := range patterns() {
		indices := p.re.FindAllStringIndex(text, -1)
		if len(indices) == 0 {
			continue
		}
		var out strings.Builder
		last := 0
		for _, index := range indices {
			start, end := index[0], index[1]
			match := text[start:end]
			if p.digit && !strings.ContainsAny(match, "0123456789") ||
				p.gluedRandom > 0 && !boundaryBefore(text, start) &&
					!hasRandomRun(match, p.gluedRandom) {
				continue
			}
			out.WriteString(text[last:start])
			out.WriteString("[скрыто:")
			out.WriteString(p.kind)
			out.WriteByte(']')
			last = end
			found[p.kind]++
		}
		if last > 0 {
			out.WriteString(text[last:])
			text = out.String()
		}
	}
	text = maskGenericSk(text, found)
	return text, found
}

// maskGenericSk: a generic key is masked after any neighbour when its body
// holds a random run of genericAnyRun, otherwise at a left boundary with a
// digit; the neighbour itself stays in the text.
func maskGenericSk(text string, found map[string]int) string {
	indices := skAnyRE.FindAllStringIndex(text, -1)
	if len(indices) == 0 {
		return text
	}
	var out strings.Builder
	last := 0
	for _, index := range indices {
		start, end := index[0], index[1]
		match := text[start:end]
		accept := hasRandomRun(match[3:], genericAnyRun)
		if !accept {
			accept = (boundaryBefore(text, start) || start > 0 && text[start-1] == '_') &&
				strings.ContainsAny(match, "0123456789")
		}
		if !accept {
			continue
		}
		out.WriteString(text[last:start])
		out.WriteString("[скрыто:sk]")
		last = end
		found["sk"]++
	}
	if last == 0 {
		return text
	}
	out.WriteString(text[last:])
	return out.String()
}

// genericAnyRun is the random run that lets a generic key be masked after any
// neighbour: the shortest generic body (20) is a single run of this length.
const genericAnyRun = 20

// hasRandomRun: text holds a run of at least n [A-Za-z0-9] with both a letter
// and a digit. A generated key has one; names, words and dates do not. The
// caller passes the part of the match to search: the body after a generic
// «sk-»/«sk_», the whole match for distinctive kinds whose prefixes end with
// «_» or «-».
func hasRandomRun(text string, n int) bool {
	start, letter, digit := 0, false, false
	for i := 0; i <= len(text); i++ {
		if i < len(text) && isAlphaNum(text[i]) {
			letter = letter || isLetter(text[i])
			digit = digit || text[i] >= '0' && text[i] <= '9'
			continue
		}
		if i-start >= n && letter && digit {
			return true
		}
		start, letter, digit = i+1, false, false
	}
	return false
}

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
