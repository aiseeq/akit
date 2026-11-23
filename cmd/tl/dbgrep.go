package main

import (
	"fmt"
	"regexp/syntax"
	"slices"
	"strings"
	"unicode"
)

// grepCondition is --grep over the events table: match (RE2, the syntax and case folding of Go's regexp) over
// who + ' ' + text as written decides, as the file path's regexp does; (?-s) turns off the server's default of a
// dot matching a newline, which Go's regexp does not. Before it, LIKE over hay — lowerUTF8 of the same string,
// with the ngram index — keeps the rows that hold the literals every match has (hayPrefilter).
func grepCondition(pattern string) (string, map[string]string, error) {
	parsed, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return "", nil, fmt.Errorf("--grep: %w", err)
	}
	params := map[string]string{"grep": chString("(?-s)(?i)" + pattern)}
	var conds []string
	for i, group := range hayPrefilter(parsed) {
		var likes []string
		for j, like := range group {
			name := fmt.Sprintf("hay%d_%d", i, j)
			params[name] = chString(like)
			likes = append(likes, fmt.Sprintf("hay LIKE {%s:String}", name))
		}
		conds = append(conds, "("+strings.Join(likes, " OR ")+")")
	}
	conds = append(conds, "match(concat(who, ' ', text), {grep:String})")
	return strings.Join(conds, " AND "), params, nil
}

// hayPrefilter is the LIKE patterns over hay that every match passes: groups that all hold, each of choices one
// of which holds. A literal goes in lowered rune by rune; a rune whose case folding lowerUTF8 may not bring to
// one rune — ſ and s, the Kelvin sign and k, σ and ς, İ — becomes %, so a match with any of its forms passes.
// A group whose choices are all wildcards is left out.
func hayPrefilter(re *syntax.Regexp) [][]string {
	var out [][]string
	for _, group := range requiredLiterals(re) {
		var likes []string
		for _, choice := range group {
			like, ok := hayLike(choice)
			if !ok {
				likes = nil
				break
			}
			likes = append(likes, like)
		}
		if len(likes) > 0 {
			out = append(out, likes)
		}
	}
	return out
}

// hayLike is the LIKE pattern over hay of one literal; ok is false when no rune of it is kept.
func hayLike(literal string) (string, bool) {
	var b strings.Builder
	b.WriteByte('%')
	kept := false
	for _, r := range literal {
		lower, ok := foldsToOneLower(r)
		if !ok {
			if !strings.HasSuffix(b.String(), "%") {
				b.WriteByte('%')
			}
			continue
		}
		b.WriteString(likeEscape(string(lower)))
		kept = true
	}
	if !strings.HasSuffix(b.String(), "%") {
		b.WriteByte('%')
	}
	return b.String(), kept
}

// foldsToOneLower is the lower case of every rune case folding matches r with, when it is one rune: a rune
// without case, or a pair of an upper and a lower case.
func foldsToOneLower(r rune) (rune, bool) {
	orbit := []rune{r}
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		orbit = append(orbit, f)
	}
	lower := unicode.ToLower(r)
	switch len(orbit) {
	case 1:
		return r, lower == r
	case 2:
		return lower, unicode.ToLower(orbit[0]) == lower && unicode.ToLower(orbit[1]) == lower && slices.Contains(orbit, lower)
	}
	return 0, false
}

// likeEscape escapes the LIKE wildcards of a literal.
func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// rawCondition is --raw over the raw lines: match (RE2, the syntax and case folding of Go's regexp) after a
// case-insensitive search of the literals every match has, as the file path takes them for --grep.
func rawCondition(pattern string) (string, map[string]string, error) {
	parsed, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return "", nil, fmt.Errorf("--raw: %w", err)
	}
	params := map[string]string{"raw": chString("(?i)" + pattern)}
	var conds []string
	for i, group := range requiredLiterals(parsed) {
		name := fmt.Sprintf("rawlit%d", i)
		params[name] = chStrings(group)
		conds = append(conds, fmt.Sprintf("multiSearchAnyCaseInsensitiveUTF8(raw, {%s:Array(String)})", name))
	}
	conds = append(conds, "match(raw, {raw:String})")
	return strings.Join(conds, " AND "), params, nil
}
