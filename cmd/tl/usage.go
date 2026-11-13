package main

import (
	"bufio"
	"cmp"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/akit/price"
	"github.com/aiseeq/akit/session"
	"github.com/aiseeq/akit/transcript"
)

// tally — расход ответов модели: токены и цена по API.
type tally struct {
	replies                            int
	input, cacheWrite, cacheRead, outp int64
	usd                                float64
	pctWeek100                         float64
	week100Replies                     int
	noWeek100                          int
	unpriced                           int // ответов, у модели или режима которых нет цены
}

type usageReply struct {
	session string
	r       *transcript.Reply
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

func (t *tally) addWeek100(model string, usd float64, priced bool, rates map[string]usageRate) {
	provider := usageProvider(model)
	rate, ok := rates[provider]
	if !ok || provider == "" || rate.apiWeek <= 0 || rate.planMonth <= 0 {
		t.noWeek100++
		return
	}
	if !priced {
		return
	}
	week100 := rate.apiWeek * 100 / rate.planMonth
	t.pctWeek100 += usd / week100 * 100
	t.week100Replies++
}

func (t *tally) merge(o *tally) {
	t.replies += o.replies
	t.input += o.input
	t.cacheWrite += o.cacheWrite
	t.cacheRead += o.cacheRead
	t.outp += o.outp
	t.usd += o.usd
	t.pctWeek100 += o.pctWeek100
	t.week100Replies += o.week100Replies
	t.noWeek100 += o.noWeek100
	t.unpriced += o.unpriced
}

// reportUsage печатает расход отобранных событий: строка на сессию и итог. Ответ модели, разложенный Claude Code
// на несколько записей, считается один раз — по id сообщения, без него — по id запроса; расход растёт к последней
// записи, берётся запись с наибольшим Output.
func reportUsage(res result, sources []source, opts options, e env, w io.Writer) int {
	ratePath := usageRatesPath(e)
	rates, rateErr := readUsageRates(ratePath, e.now)
	var replies []usageReply
	last := map[string]int{} // сессия и id ответа → индекс в replies
	for i := range res.hits {
		h := &res.hits[i]
		r := h.ev.Reply
		if r == nil || r.Usage == nil {
			continue
		}
		key := cmp.Or(r.ID, r.RequestID)
		if key == "" {
			replies = append(replies, usageReply{h.src.ID, r})
			continue
		}
		key = h.src.ID + "\x00" + key
		if j, ok := last[key]; ok {
			if r.Usage.Output > replies[j].r.Usage.Output {
				replies[j].r = r
			}
			continue
		}
		last[key] = len(replies)
		replies = append(replies, usageReply{h.src.ID, r})
	}
	per := map[string]*tally{}
	for _, rp := range replies {
		t := per[rp.session]
		if t == nil {
			t = &tally{}
			per[rp.session] = t
		}
		t.add(rp.r.Model, rp.r.Usage)
		usd, ok := price.Cost(rp.r.Model, *rp.r.Usage)
		t.addWeek100(rp.r.Model, usd, ok, rates)
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
		return cmp.Or(cmp.Compare(per[b].pctWeek100, per[a].pctWeek100), cmp.Compare(a, b))
	})
	fmt.Fprintln(w, "session\treplies\tinput\tcache_write\tcache_read\toutput\tpct_week100\tusd_api")
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
	fmt.Fprintln(w, usageSummary(total, replies, len(ids), ratePath, rates, rateErr, sources, opts, e, res))
	return 0
}

func usageSummary(total tally, replies []usageReply, sessionCount int, ratePath string, rates map[string]usageRate, rateErr error, sources []source, opts options, e env, res result) string {
	summary := fmt.Sprintf("tl: %d session(s), %d reply(ies); pct_week100 — %% of provider $100 subscription week; usd_api — API list price (price table %s); rates: %s",
		sessionCount, total.replies, price.Checked, ratePath)
	if rateErr != nil {
		summary += "; no subscription rates (" + rateErr.Error() + "); configure XDG_CONFIG_HOME/akit/rates.tsv or ~/.config/akit/rates.tsv"
	} else {
		providers := map[string]bool{}
		for _, rp := range replies {
			if provider := usageProvider(rp.r.Model); provider != "" {
				providers[provider] = true
			}
		}
		used := make([]string, 0, len(providers))
		for provider := range providers {
			if rate, ok := rates[provider]; ok {
				used = append(used, provider+"="+rate.validFrom.Format(time.DateOnly))
			} else {
				used = append(used, provider+"=missing")
			}
		}
		slices.Sort(used)
		summary += "; valid_from: " + strings.Join(used, ",")
	}
	if total.noWeek100 > 0 {
		summary += fmt.Sprintf("; %d reply(ies) without a subscription rate", total.noWeek100)
	}
	if total.unpriced > 0 {
		summary += fmt.Sprintf("; %d reply(ies) without a price not in usd_api", total.unpriced)
	}
	return summary + "; " + usageScope(sources, opts, e) + res.problems()
}

// usageScope — что прочитано: расход вне этих журналов (запуск `claude -p --no-session-persistence` журнала не
// пишет, у других клиентов он свой) в итог не входит, и читатель итога должен это видеть.
// Счёт токенов rollout codex (token_count) --usage не читает: rollout в итог не входит.
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
		read = sources[0].Path
		if len(sources) > 1 {
			read += fmt.Sprintf(" and %d subagent transcript(s)", len(sources)-1)
		}
	}
	scope := "read: " + read + "; not counted: runs without a transcript (claude -p --no-session-persistence)"
	if opts.codex || slices.ContainsFunc(sources, func(s source) bool { return isCodexRollout(s.Path) }) {
		scope += " and codex rollouts (their token counts are not read)"
	}
	return scope
}

func usageRow(name string, t *tally) string {
	pct := "-"
	if t.week100Replies > 0 {
		pct = fmt.Sprintf("%.2f", t.pctWeek100)
	}
	return fmt.Sprintf("%s\t%d\t%d\t%d\t%d\t%d\t%s\t%.2f", name, t.replies, t.input, t.cacheWrite, t.cacheRead, t.outp, pct, t.usd)
}

type usageRate struct {
	apiWeek, planMonth float64
	validFrom          time.Time
}

func usageRatesPath(e env) string {
	base := e.configHome
	if base == "" {
		base = filepath.Join(e.home, ".config")
	}
	return filepath.Join(base, "akit", "rates.tsv")
}

func readUsageRates(path string, now time.Time) (map[string]usageRate, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open rates %s: %w", path, err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	columns := map[string]int{}
	width := 0
	rates := map[string]usageRate{}
	for lineNo := 1; scanner.Scan(); lineNo++ {
		line := strings.TrimRight(scanner.Text(), "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if width == 0 {
			width = len(fields)
			for i, name := range fields {
				columns[name] = i
			}
			for _, name := range []string{"valid_from", "provider", "plan_usd_month", "api_usd_week"} {
				if _, ok := columns[name]; !ok {
					return nil, fmt.Errorf("header missing %s", name)
				}
			}
			continue
		}
		if len(fields) != width {
			return nil, fmt.Errorf("line %d: wrong number of columns", lineNo)
		}
		provider, rate, future, err := parseUsageRateRow(lineNo, fields, columns, now)
		if err != nil {
			return nil, err
		}
		if future {
			continue
		}
		old, exists := rates[provider]
		if !exists || rate.validFrom.After(old.validFrom) || rate.validFrom.Equal(old.validFrom) {
			rates[provider] = rate
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read rates %s: %w", path, err)
	}
	if width == 0 {
		return nil, fmt.Errorf("no header")
	}
	return rates, nil
}

func parseUsageRateRow(lineNo int, fields []string, columns map[string]int, now time.Time) (string, usageRate, bool, error) {
	provider := fields[columns["provider"]]
	valid, err := parseUsageValidFrom(fields[columns["valid_from"]])
	if err != nil {
		return "", usageRate{}, false, fmt.Errorf("line %d: invalid valid_from", lineNo)
	}
	if valid.After(now) {
		return provider, usageRate{}, true, nil
	}
	week, weekErr := strconv.ParseFloat(fields[columns["api_usd_week"]], 64)
	plan, planErr := strconv.ParseFloat(fields[columns["plan_usd_month"]], 64)
	if weekErr != nil || planErr != nil || week <= 0 || plan <= 0 {
		return "", usageRate{}, false, fmt.Errorf("line %d: invalid subscription rate", lineNo)
	}
	return provider, usageRate{apiWeek: week, planMonth: plan, validFrom: valid}, false, nil
}

func parseUsageValidFrom(value string) (time.Time, error) {
	if date, err := time.ParseInLocation(time.DateOnly, value, time.Local); err == nil {
		return date, nil
	}
	return time.Parse(time.RFC3339, value)
}

func usageProvider(model string) string {
	model = strings.ToLower(model)
	switch {
	case strings.HasPrefix(model, "claude-"):
		return "anthropic"
	case strings.HasPrefix(model, "gpt-"), strings.HasPrefix(model, "codex-"):
		return "openai"
	case len(model) > 1 && model[0] == 'o' && model[1] >= '0' && model[1] <= '9':
		return "openai"
	default:
		return ""
	}
}

// isCodexRollout — файл codex: codex называет журналы сессий rollout-<время>-<id>.jsonl.
func isCodexRollout(path string) bool { return strings.HasPrefix(filepath.Base(path), "rollout-") }
