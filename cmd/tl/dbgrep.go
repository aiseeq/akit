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
// dot matching a newline, which Go's regexp does not. Before it, a prefilter over hay — lowerUTF8 of the same
// string — keeps the rows that hold the literals every match has (hayPrefilter): where every choice of a group has
// a piece of three or more characters, its trigrams in the ngram index of hay, which the server answers from the
// index alone; otherwise LIKE, which reads hay.
func grepCondition(pattern string) (string, map[string]string, error) {
	parsed, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return "", nil, fmt.Errorf("--grep: %w", err)
	}
	params := map[string]string{"grep": chString("(?-s)(?i)" + pattern)}
	var conds []string
	for i, group := range hayPrefilter(parsed) {
		var choices []string
		indexed := !slices.ContainsFunc(group, func(c hayChoice) bool { return len(c.trigrams) == 0 })
		for j, choice := range group {
			name := fmt.Sprintf("hay%d_%d", i, j)
			if indexed {
				params[name] = chStrings(choice.trigrams)
				choices = append(choices, fmt.Sprintf("hasAllTokens(hay, {%s:Array(String)})", name))
				continue
			}
			params[name] = chString(choice.like)
			choices = append(choices, fmt.Sprintf("hay LIKE {%s:String}", name))
		}
		conds = append(conds, "("+strings.Join(choices, " OR ")+")")
	}
	conds = append(conds, "match(concat(who, ' ', text), {grep:String})")
	return strings.Join(conds, " AND "), params, nil
}

// hayChoice is one literal over hay: its LIKE pattern and the trigrams of its pieces of three or more characters.
type hayChoice struct {
	like     string
	trigrams []string
}

// hayPrefilter is the literals over hay that every match passes: groups that all hold, each of choices one of
// which holds. A literal goes in lowered rune by rune; a rune whose case folding lowerUTF8 may not bring to one
// rune — ſ and s, the Kelvin sign and k, σ and ς, İ — splits it, so a match with any of its forms passes. A group
// with a choice of no kept rune is left out.
func hayPrefilter(re *syntax.Regexp) [][]hayChoice {
	var out [][]hayChoice
	for _, group := range requiredLiterals(re) {
		var choices []hayChoice
		for _, choice := range group {
			pieces := hayPieces(choice)
			if len(pieces) == 0 {
				choices = nil
				break
			}
			choices = append(choices, hayChoice{like: hayLike(pieces), trigrams: trigrams(pieces)})
		}
		if len(choices) > 0 {
			out = append(out, choices)
		}
	}
	return out
}

// hayPieces is a literal lowered as hay has it, split at the runes lowerUTF8 may not fold to one: the runs of
// the rest, in order.
func hayPieces(literal string) []string {
	var pieces []string
	var b strings.Builder
	for _, r := range literal {
		lower, ok := foldsToOneLower(r)
		if !ok {
			if b.Len() > 0 {
				pieces = append(pieces, b.String())
				b.Reset()
			}
			continue
		}
		b.WriteRune(lower)
	}
	if b.Len() > 0 {
		pieces = append(pieces, b.String())
	}
	return pieces
}

// hayLike is the LIKE pattern over hay of the pieces of one literal.
func hayLike(pieces []string) string {
	escaped := make([]string, len(pieces))
	for i, p := range pieces {
		escaped[i] = likeEscape(p)
	}
	return "%" + strings.Join(escaped, "%") + "%"
}

// trigrams are the tokens the ngrams(3) tokenizer of the index on hay makes of the pieces: every three
// consecutive characters. A hay that holds the pieces holds all of them; a piece shorter than three gives none.
func trigrams(pieces []string) []string {
	var out []string
	for _, p := range pieces {
		r := []rune(p)
		for i := 0; i+3 <= len(r); i++ {
			if t := string(r[i : i+3]); !slices.Contains(out, t) {
				out = append(out, t)
			}
		}
	}
	return out
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
