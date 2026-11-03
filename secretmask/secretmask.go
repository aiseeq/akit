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
	skRE            = regexp.MustCompile(`(^|\\[ntr]|\\u[0-9A-Fa-f]{4}|\\f|\\(?:e\[[0-9;]*m|033\[[0-9;]*m|x1b\[[0-9;]*m)|\x1b\[[0-9;]*m|%[0-9A-Fa-f]{2}|_|[^A-Za-z0-9_])(sk[-_][A-Za-z0-9_-]{20,})`)
	githubRE        = regexp.MustCompile(`(?:gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{30,})`)
	slackRE         = regexp.MustCompile(`xox[abeoprs]-[A-Za-z0-9-]{10,}`)
	awsRE           = regexp.MustCompile(`AKIA[0-9A-Z]{16}`)
	googleRE        = regexp.MustCompile(`AIza[0-9A-Za-z_-]{30,}`)
)

type pattern struct {
	kind string
	re   *regexp.Regexp
	// digit asks for a digit in the match: «sk-» starts ordinary hyphenated
	// words as well, a generated key always has one
	digit bool
}

// patterns apply private key blocks first, then distinctive prefixes, and the
// generic sk form last so it can match after a replacement marker.
func patterns() []pattern {
	return []pattern{
		{kind: "private-key", re: privateKeyRE},
		{kind: "sk_live", re: skLiveRE},
		{kind: "sk", re: skDistinctiveRE},
		{kind: "github", re: githubRE},
		{kind: "slack", re: slackRE},
		{kind: "aws", re: awsRE},
		{kind: "google", re: googleRE},
		{kind: "sk", re: skRE, digit: true},
	}
}

// Mask replaces every secret in text with [скрыто:<kind>] and returns the text
// and the number of matches by kind (empty, not nil, when there are none).
func Mask(text string) (string, map[string]int) {
	found := map[string]int{}
	for _, p := range patterns() {
		text = p.re.ReplaceAllStringFunc(text, func(m string) string {
			prefix, secret := "", m
			if p.digit {
				// the generic sk form captures the character before the key; it stays in the text
				groups := p.re.FindStringSubmatch(m)
				prefix, secret = groups[1], groups[2]
			}
			if p.digit && !strings.ContainsAny(secret, "0123456789") {
				return m
			}
			found[p.kind]++
			return prefix + "[скрыто:" + p.kind + "]"
		})
	}
	return text, found
}
