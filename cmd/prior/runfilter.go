package main

import (
	"fmt"
	"regexp"
	"regexp/syntax"
	"strings"
	"unicode"
	"unicode/utf8"
)

type runNeedle [][]string

// newRunFilter возвращает литералы, хотя бы один из которых обязателен для совпадения.
// Если это нельзя доказать для всех альтернатив, предфильтр не применяется.
func newRunFilter(re *regexp.Regexp) ([]runNeedle, error) {
	parsed, err := syntax.Parse(re.String(), syntax.Perl)
	if err != nil {
		return nil, fmt.Errorf("предфильтр отчётов: %w", err)
	}
	return requiredRunLiterals(parsed), nil
}

func requiredRunLiterals(re *syntax.Regexp) []runNeedle {
	switch re.Op {
	case syntax.OpLiteral:
		needle := runLiteralVariants(re.Rune)
		if len(needle) > 0 {
			return []runNeedle{needle}
		}
	case syntax.OpCapture, syntax.OpPlus:
		return requiredRunLiterals(re.Sub[0])
	case syntax.OpRepeat:
		if re.Min > 0 {
			return requiredRunLiterals(re.Sub[0])
		}
	case syntax.OpConcat:
		var best []runNeedle
		bestLen := 0
		for _, sub := range re.Sub {
			literals := requiredRunLiterals(sub)
			if len(literals) == 0 {
				continue
			}
			shortest := runNeedleSize(literals[0])
			for _, literal := range literals[1:] {
				shortest = min(shortest, runNeedleSize(literal))
			}
			if shortest > bestLen {
				best, bestLen = literals, shortest
			}
		}
		return best
	case syntax.OpAlternate:
		var literals []runNeedle
		for _, sub := range re.Sub {
			part := requiredRunLiterals(sub)
			if len(part) == 0 {
				return nil
			}
			literals = append(literals, part...)
		}
		return literals
	}
	return nil
}

func runNeedleSize(needle runNeedle) int {
	size := 0
	for _, variants := range needle {
		size += len(variants[0])
	}
	return size
}

// runLiteralVariants сворачивает обязательный литерал по Unicode SimpleFold. RuneError разделяет литерал:
// regexp видит его и в некорректных байтах UTF-8, которые нельзя искать как буквальную кодировку этой руны.
func runLiteralVariants(runes []rune) runNeedle {
	var needle runNeedle
	var part strings.Builder
	flush := func() {
		if part.Len() > 0 {
			needle = append(needle, []string{part.String()})
			part.Reset()
		}
	}
	for _, r := range runes {
		if r == utf8.RuneError {
			flush()
			continue
		}
		part.WriteRune(foldRune(r))
	}
	flush()
	return needle
}

func foldRune(r rune) rune {
	folded := r
	for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
		folded = min(folded, next)
	}
	return folded
}

func foldText(text string) string {
	var folded strings.Builder
	folded.Grow(len(text))
	for _, r := range text {
		folded.WriteRune(foldRune(r))
	}
	return folded.String()
}

func runMayMatch(data string, literals []runNeedle) bool {
	for _, needle := range literals {
		found := true
		for _, variants := range needle {
			part := false
			for _, variant := range variants {
				if strings.Contains(data, variant) {
					part = true
					break
				}
			}
			if !part {
				found = false
				break
			}
		}
		if found {
			return true
		}
	}
	return false
}

// matchFiltered rejects only strings that cannot match the regexp. The regexp remains
// authoritative for every candidate, including expressions with no provable literal.
func matchFiltered(re *regexp.Regexp, literals []runNeedle, text string) bool {
	return matchFilteredFolded(re, literals, text, foldText(text))
}

func matchFilteredFolded(re *regexp.Regexp, literals []runNeedle, text, folded string) bool {
	return (len(literals) == 0 || runMayMatch(folded, literals)) && re.MatchString(text)
}
