package main

import (
	"fmt"
	"regexp/syntax"
	"slices"
	"strings"
	"unicode"
)

// grepCondition is --grep over the events table. hay is lowerUTF8(who + ' ' + text), the string the file path
// matches with (?i): a pattern of one literal is LIKE over it, any other goes to multiMatchAny (hyperscan) with its
// literals in lower case. Only the literals: \S, \W or \D in lower case would change their meaning, and character
// classes already hold both cases after (?i). multiMatchAny takes the text index only from a pattern without
// (?i), which is why the pattern is lowered rather than flagged. A pattern hyperscan refuses goes to match (RE2)
// over the same hay; one with case-sensitive parts, to match over the text as written.
func grepCondition(pattern string, mode grepMode) (string, map[string]string, error) {
	if mode == grepAsWritten {
		return "match(concat(who, ' ', text), {grep:String})", map[string]string{"grep": chString("(?i)" + pattern)}, nil
	}
	lowered, literal, err := lowerPattern(pattern)
	if err != nil {
		return "", nil, err
	}
	if literal != "" {
		return "hay LIKE {grep:String}", map[string]string{"grep": chString("%" + likeEscape(literal) + "%")}, nil
	}
	if mode == grepRE2 {
		return "match(hay, {grep:String})", map[string]string{"grep": chString(lowered)}, nil
	}
	return "multiMatchAny(hay, {grep:Array(String)})", map[string]string{"grep": chStrings([]string{lowered})}, nil
}

// lowerPattern is the pattern with (?i) applied and its literals lowered; literal is set when the whole pattern
// is one literal.
func lowerPattern(pattern string) (lowered, literal string, err error) {
	re, err := syntax.Parse("(?i)"+pattern, syntax.Perl)
	if err != nil {
		return "", "", fmt.Errorf("--grep: %w", err)
	}
	lowerLiterals(re)
	if re.Op == syntax.OpLiteral {
		return re.String(), string(re.Rune), nil
	}
	return re.String(), "", nil
}

func lowerLiterals(re *syntax.Regexp) {
	if re.Op == syntax.OpLiteral && re.Flags&syntax.FoldCase != 0 {
		for i, r := range re.Rune {
			re.Rune[i] = unicode.ToLower(r)
		}
		re.Flags &^= syntax.FoldCase
	}
	for _, sub := range re.Sub {
		lowerLiterals(sub)
	}
}

// maxClassCheck — how many characters of a class caseSensitiveParts looks at: a larger class comes from (?i)
// itself or from a negation, and holds both cases.
const maxClassCheck = 4096

// caseSensitiveParts tells whether (?-i) turns case folding off in a part of the pattern: a literal or a class
// there holds a letter without its other case, and lower-case hay cannot match it.
func caseSensitiveParts(pattern string) (bool, error) {
	re, err := syntax.Parse("(?i)"+pattern, syntax.Perl)
	if err != nil {
		return false, fmt.Errorf("--grep: %w", err)
	}
	return foldIncomplete(re), nil
}

func foldIncomplete(re *syntax.Regexp) bool {
	switch re.Op {
	case syntax.OpLiteral:
		if re.Flags&syntax.FoldCase == 0 && slices.ContainsFunc(re.Rune, func(r rune) bool { return unicode.SimpleFold(r) != r }) {
			return true
		}
	case syntax.OpCharClass:
		if classFoldIncomplete(re.Rune) {
			return true
		}
	}
	return slices.ContainsFunc(re.Sub, foldIncomplete)
}

func classFoldIncomplete(ranges []rune) bool {
	in := func(r rune) bool {
		for i := 0; i+1 < len(ranges); i += 2 {
			if r >= ranges[i] && r <= ranges[i+1] {
				return true
			}
		}
		return false
	}
	seen := 0
	for i := 0; i+1 < len(ranges); i += 2 {
		for r := ranges[i]; r <= ranges[i+1]; r++ {
			if seen++; seen > maxClassCheck {
				return false
			}
			for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
				if !in(f) {
					return true
				}
			}
		}
	}
	return false
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
