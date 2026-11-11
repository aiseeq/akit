package main

import (
	"bytes"
	"fmt"
	"regexp"
	"regexp/syntax"
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

// runLiteralVariants делит литерал на фрагменты до четырёх рун и перечисляет их свёртки. RuneError пропускается:
// regexp видит его и в некорректных байтах UTF-8, которые нельзя искать как буквальную кодировку этой руны.
func runLiteralVariants(runes []rune) runNeedle {
	var needle runNeedle
	variants := []string{""}
	count := 0
	flush := func() {
		if count > 0 {
			needle = append(needle, variants)
			variants, count = []string{""}, 0
		}
	}
	for i := len(runes) - 1; i >= 0; i-- {
		r := runes[i]
		if r == utf8.RuneError {
			flush()
			continue
		}
		forms := []rune{r}
		for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
			forms = append(forms, next)
		}
		if count == 4 || len(forms)*len(variants) > 32 {
			flush()
		}
		var expanded []string
		for _, form := range forms {
			for _, suffix := range variants {
				expanded = append(expanded, string(form)+suffix)
			}
		}
		variants = expanded
		count++
	}
	flush()
	return needle
}

func runMayMatch(data []byte, literals []runNeedle) bool {
	for _, needle := range literals {
		found := true
		for _, variants := range needle {
			part := false
			for _, variant := range variants {
				if bytes.Contains(data, []byte(variant)) {
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
