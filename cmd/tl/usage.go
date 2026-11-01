package main

import (
	"cmp"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"

	"github.com/aiseeq/akit/price"
	"github.com/aiseeq/akit/session"
	"github.com/aiseeq/akit/transcript"
)

// tally — расход ответов модели: токены и цена по API.
type tally struct {
	replies                            int
	input, cacheWrite, cacheRead, outp int64
	usd                                float64
	unpriced                           int // ответов, у модели или режима которых нет цены
}

func (t *tally) add(model string, u *price.Usage) {
	t.replies++
	t.input += u.Input
	t.cacheWrite += u.CacheWrite
	t.cacheRead += u.CacheRead
	t.outp += u.Output
	if c, ok := price.Cost(model, *u); ok {
		t.usd += c
	} else {
		t.unpriced++
	}
}

func (t *tally) merge(o *tally) {
	t.replies += o.replies
	t.input += o.input
	t.cacheWrite += o.cacheWrite
	t.cacheRead += o.cacheRead
	t.outp += o.outp
	t.usd += o.usd
	t.unpriced += o.unpriced
}

// reportUsage печатает расход отобранных событий: строка на сессию и итог. Ответ модели, разложенный Claude Code
// на несколько записей, считается один раз — по id сообщения, без него — по id запроса; расход растёт к последней
// записи, берётся запись с наибольшим Output.
func reportUsage(res result, sources []source, opts options, e env, w io.Writer) int {
	type reply struct {
		session string
		r       *transcript.Reply
	}
	var replies []reply
	last := map[string]int{} // сессия и id ответа → индекс в replies
	for i := range res.hits {
		h := &res.hits[i]
		r := h.ev.Reply
		if r == nil || r.Usage == nil {
			continue
		}
		key := cmp.Or(r.ID, r.RequestID)
		if key == "" {
			replies = append(replies, reply{h.src.id, r})
			continue
		}
		key = h.src.id + "\x00" + key
		if j, ok := last[key]; ok {
			if r.Usage.Output > replies[j].r.Usage.Output {
				replies[j].r = r
			}
			continue
		}
		last[key] = len(replies)
		replies = append(replies, reply{h.src.id, r})
	}
	per := map[string]*tally{}
	for _, rp := range replies {
		t := per[rp.session]
		if t == nil {
			t = &tally{}
			per[rp.session] = t
		}
		t.add(rp.r.Model, rp.r.Usage)
	}
	if len(per) == 0 {
		fmt.Fprintf(w, "tl: no model replies with usage in %d transcript(s)%s; %s%s\n", res.files, windowNote(opts, e),
			usageScope(sources, opts, e), res.problems())
		if len(res.errs) > 0 {
			return exitUsage
		}
		return exitNone
	}
	ids := make([]string, 0, len(per))
	for id := range per {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b string) int { // дорогие сверху
		return cmp.Or(cmp.Compare(per[b].usd, per[a].usd), cmp.Compare(a, b))
	})
	fmt.Fprintln(w, "session\treplies\tinput\tcache_write\tcache_read\toutput\tusd_api")
	var total tally
	for _, id := range ids {
		t := per[id]
		total.merge(t)
		name := id
		if n := res.names[id]; n != "" {
			name += " " + n
		}
		fmt.Fprintln(w, usageRow(name, t))
	}
	fmt.Fprintln(w, usageRow("total", &total))
	summary := fmt.Sprintf("tl: %d session(s), %d reply(ies); usd_api — API list price (price table %s)", len(ids),
		total.replies, price.Checked)
	if total.unpriced > 0 {
		summary += fmt.Sprintf("; %d reply(ies) without a price not in usd_api", total.unpriced)
	}
	fmt.Fprintln(w, summary+"; "+usageScope(sources, opts, e)+res.problems())
	return 0
}

// usageScope — что прочитано: расход вне этих журналов (запуск `claude -p --no-session-persistence` журнала не
// пишет, у других клиентов он свой) в итог не входит, и читатель итога должен это видеть.
// Rollout codex пишет только нарастающий счёт токенов сессии, без расхода на ответ: в итог он не входит.
func usageScope(sources []source, opts options, e env) string {
	var read string
	switch {
	case opts.all:
		read = filepath.Join(session.ProjectsDir(e.home), "*")
		if opts.project != "" {
			read += opts.project + "*"
		}
		if opts.noSubagents {
			read += " without subagents"
		}
	case len(sources) > 0:
		read = sources[0].path
		if len(sources) > 1 {
			read += fmt.Sprintf(" and %d subagent transcript(s)", len(sources)-1)
		}
	}
	scope := "read: " + read + "; not counted: runs without a transcript (claude -p --no-session-persistence)"
	if opts.codex || slices.ContainsFunc(sources, func(s source) bool { return isCodexRollout(s.path) }) {
		scope += " and codex rollouts (no per-reply usage)"
	}
	return scope
}

func usageRow(name string, t *tally) string {
	return fmt.Sprintf("%s\t%d\t%d\t%d\t%d\t%d\t%.2f", name, t.replies, t.input, t.cacheWrite, t.cacheRead, t.outp, t.usd)
}

// isCodexRollout — файл codex: codex называет журналы сессий rollout-<время>-<id>.jsonl.
func isCodexRollout(path string) bool { return strings.HasPrefix(filepath.Base(path), "rollout-") }
