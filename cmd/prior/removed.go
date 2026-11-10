package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
)

// maxRemoved — сколько снятых правил показывать.
const maxRemoved = 10

// removedRule — запись архива снятых правил: заголовок «## <файл>:<строка> — <дата>», строки «Класс:» и
// «Причина:», затем текст правила цитатой («> »).
type removedRule struct {
	line                        int // строка заголовка в архиве
	where, class, reason, quote string
}

// parseRemoved делит архив на записи; текст до первого заголовка — вступление, не запись.
func parseRemoved(data string) []removedRule {
	var rules []removedRule
	var cur *removedRule
	var quote []string
	flush := func() {
		if cur != nil {
			cur.quote = strings.Join(quote, " ")
			rules = append(rules, *cur)
		}
		quote = nil
	}
	for i, l := range strings.Split(data, "\n") {
		t := strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(l, "## "):
			flush()
			cur = &removedRule{line: i + 1, where: strings.TrimSpace(strings.TrimPrefix(l, "## "))}
		case cur == nil:
		case strings.HasPrefix(t, "Класс:"):
			cur.class = strings.TrimSpace(strings.TrimPrefix(t, "Класс:"))
		case strings.HasPrefix(t, "Причина:"):
			cur.reason = strings.TrimSpace(strings.TrimPrefix(t, "Причина:"))
		case strings.HasPrefix(t, ">"):
			if s := strings.TrimSpace(strings.TrimPrefix(t, ">")); s != "" {
				quote = append(quote, s)
			}
		}
	}
	flush()
	return rules
}

// removed — архив правил, снятых разбором без ревью пользователя (~/.claude/improve/state/removed-rules.md):
// снятое находится по словам правила, места или причины и может быть возвращено. Свежие (ниже в архиве) сверху;
// на запись — место и дата, класс, причина снятия и текст правила. Нет архива — ничего не снимали.
func removed(w io.Writer, q query, path string) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintln(w, "(нет)")
		return
	}
	if err != nil {
		fmt.Fprintf(w, "(не прочитался: %v)\n", err)
		return
	}
	var hits []removedRule
	rules := parseRemoved(string(data))
	for i := len(rules) - 1; i >= 0; i-- {
		r := rules[i]
		if q.any.MatchString(strings.Join([]string{r.where, r.class, r.reason, r.quote}, "\n")) {
			hits = append(hits, r)
		}
	}
	if len(hits) == 0 {
		fmt.Fprintln(w, "(нет)")
		return
	}
	if len(hits) > maxRemoved {
		fmt.Fprintf(w, "(совпадений %d, ниже %d свежих - сузьте слова)\n", len(hits), maxRemoved)
		hits = hits[:maxRemoved]
	}
	for _, r := range hits {
		head := fmt.Sprintf("removed-rules.md:%d %s", r.line, r.where)
		if r.class != "" {
			head += " [" + r.class + "]"
		}
		fmt.Fprintln(w, cut(head))
		reason := r.reason
		if reason == "" {
			reason = "(не указана)"
		}
		fmt.Fprintln(w, cut("  причина: "+reason))
		fmt.Fprintln(w, cut("  правило: "+r.quote))
	}
}
