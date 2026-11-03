// Package secretmask hides secret values in text before it goes further: to a
// log, a report or a model of another provider. It replaces each match with
// [скрыто:<kind>] and counts matches by kind; it never returns a value.
package secretmask

import (
	"regexp"
	"strings"
)

var (
	privateKeyRE = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?(?:-----END [A-Z ]*PRIVATE KEY-----|\z)`)
	skLiveRE     = regexp.MustCompile(`\b(?:sk|rk|pk)_(?:live|test)_[0-9A-Za-z]{16,}`)
	skRE         = regexp.MustCompile(`\bsk[-_][A-Za-z0-9_-]{20,}`)
	githubRE     = regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{30,})`)
	slackRE      = regexp.MustCompile(`\bxox[abeoprs]-[A-Za-z0-9-]{10,}`)
	awsRE        = regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)
	googleRE     = regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{30,}`)
)

type pattern struct {
	kind string
	re   *regexp.Regexp
	// digit asks for a digit in the match: «sk-» starts ordinary hyphenated
	// words as well, a generated key always has one
	digit bool
}

// patterns in the order they apply: a private key block first, so the keys
// inside it are not counted again
func patterns() []pattern {
	return []pattern{
		{kind: "private-key", re: privateKeyRE},
		{kind: "sk_live", re: skLiveRE},
		{kind: "sk", re: skRE, digit: true},
		{kind: "github", re: githubRE},
		{kind: "slack", re: slackRE},
		{kind: "aws", re: awsRE},
		{kind: "google", re: googleRE},
	}
}

// Mask replaces every secret in text with [скрыто:<kind>] and returns the text
// and the number of matches by kind (empty, not nil, when there are none).
func Mask(text string) (string, map[string]int) {
	found := map[string]int{}
	for _, p := range patterns() {
		text = p.re.ReplaceAllStringFunc(text, func(m string) string {
			if p.digit && !strings.ContainsAny(m, "0123456789") {
				return m
			}
			found[p.kind]++
			return "[скрыто:" + p.kind + "]"
		})
	}
	return text, found
}
