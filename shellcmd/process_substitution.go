package shellcmd

import (
	"slices"
	"sort"

	"mvdan.cc/sh/v3/syntax"
)

type processSubstitutionSpan struct{ start int }

type processSubstitutionIndex struct {
	spans           []processSubstitutionSpan
	work            *int
	spansOutOfOrder bool
	calls           []pendingProcessSubstitutionCall
	stmts           []*syntax.Stmt
	pipelines       []*syntax.Stmt
}

func processSubstitutionCallKind(call *syntax.CallExpr, src string, vars map[string]string) (source, relevant bool) {
	if len(call.Args) == 0 {
		return false, false
	}
	name, literal := wordValue(call.Args[0])
	if !literal {
		resolved, _ := words(call.Args[:1], src, vars)
		if len(resolved) > 0 {
			name = resolved[0]
		}
	}
	name = baseName(name)
	if name == "source" || name == "." {
		if !literal {
			return false, false
		}
		return true, true
	}
	return false, isShell(name) || wrappers[name] || !literal
}

func (index *processSubstitutionIndex) add(subst *syntax.ProcSubst) {
	start := int(subst.Pos().Offset())
	if n := len(index.spans); n > 0 && index.spans[n-1].start > start {
		index.spansOutOfOrder = true
	}
	index.spans = append(index.spans, processSubstitutionSpan{start: start})
}

func (index *processSubstitutionIndex) sort() {
	if !index.spansOutOfOrder {
		return
	}
	slices.SortFunc(index.spans, func(a, b processSubstitutionSpan) int {
		if index.work != nil {
			*index.work++
		}
		switch {
		case a.start < b.start:
			return -1
		case a.start > b.start:
			return 1
		default:
			return 0
		}
	})
	index.spansOutOfOrder = false
}

func (info *walkInfo) hasProcessSubstitution(words []*syntax.Word) bool {
	if !info.hasProcessSubstitutionText || info.processSubstitutions == nil {
		return false
	}
	return info.processSubstitutions.contains(words)
}

func (index *processSubstitutionIndex) contains(words []*syntax.Word) bool {
	if index == nil || len(index.spans) == 0 {
		return false
	}
	for _, word := range words {
		start, end := int(word.Pos().Offset()), int(word.End().Offset())
		i := sort.Search(len(index.spans), func(i int) bool {
			if index.work != nil {
				*index.work++
			}
			return index.spans[i].start >= start
		})
		// Search guarantees start >= word.start; every ProcSubst lies within
		// its containing Word, so checking start < word.end is sufficient.
		if i < len(index.spans) && index.spans[i].start < end {
			return true
		}
	}
	return false
}
