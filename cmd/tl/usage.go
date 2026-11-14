package main

import (
	"bufio"
	"cmp"
	"fmt"
	"io"
	"maps"
	"math"
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
	pctWeek100                         map[string]float64
	week100Replies                     map[string]int
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
	if !ok || provider == "" || !rate.usable() {
		t.noWeek100++
		return
	}
	if !priced {
		return
	}
	week100 := rate.apiWeek * 100 / rate.planMonth
	if t.pctWeek100 == nil {
		t.pctWeek100 = map[string]float64{}
		t.week100Replies = map[string]int{}
	}
	t.pctWeek100[provider] += usd / week100 * 100
	t.week100Replies[provider]++
}

func (t *tally) merge(o *tally) {
	t.replies += o.replies
	t.input += o.input
	t.cacheWrite += o.cacheWrite
	t.cacheRead += o.cacheRead
	t.outp += o.outp
	t.usd += o.usd
	for provider, pct := range o.pctWeek100 {
		if t.pctWeek100 == nil {
			t.pctWeek100 = map[string]float64{}
			t.week100Replies = map[string]int{}
		}
		t.pctWeek100[provider] += pct
		t.week100Replies[provider] += o.week100Replies[provider]
	}
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
	last := map[string]int{} // id ответа во всей выборке → индекс в replies
	for i := range res.hits {
		h := &res.hits[i]
		r := h.ev.Reply
		if r == nil || r.Usage == nil {
			continue
		}
		key := cmp.Or(r.ID, r.RequestID)
		if key == "" {
			replies = append(replies, usageReply{h.src.Path, r})
			continue
		}
		if j, ok := last[key]; ok {
			if r.Usage.Output > replies[j].r.Usage.Output {
				replies[j].r = r
			}
			continue
		}
		last[key] = len(replies)
		replies = append(replies, usageReply{h.src.Path, r})
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
	providerSet := make(map[string]bool, len(rates))
	for provider := range rates {
		providerSet[provider] = true
	}
	for _, rp := range replies {
		if provider := usageProvider(rp.r.Model); provider != "" {
			providerSet[provider] = true
		}
	}
	providers := slices.Sorted(maps.Keys(providerSet))
	hasPct := slices.ContainsFunc(ids, func(id string) bool { return len(per[id].week100Replies) > 0 })
	slices.SortFunc(ids, func(a, b string) int {
		if hasPct {
			return cmp.Or(cmp.Compare(usageMaxPct(per[b], providers), usageMaxPct(per[a], providers)), cmp.Compare(res.linkIDs[a], res.linkIDs[b]))
		}
		return cmp.Or(cmp.Compare(per[b].usd, per[a].usd), cmp.Compare(res.linkIDs[a], res.linkIDs[b]))
	})
	header := []string{"session", "replies", "input", "cache_write", "cache_read", "output"}
	for _, provider := range providers {
		header = append(header, "pct100_"+provider)
	}
	header = append(header, "usd_api")
	fmt.Fprintln(w, strings.Join(header, "\t"))
	var total tally
	for _, id := range ids {
		t := per[id]
		total.merge(t)
		name := res.linkIDs[id]
		if name == "" {
			name = id
		}
		if n := res.names[id]; n != "" {
			name += " " + n
		}
		fmt.Fprintln(w, usageRow(name, t, providers))
	}
	fmt.Fprintln(w, usageRow("total", &total, providers))
	fmt.Fprintln(w, usageSummary(total, replies, len(ids), ratePath, rates, rateErr, sources, opts, e, res))
	return 0
}

func usageSummary(total tally, replies []usageReply, sessionCount int, ratePath string, rates map[string]usageRate, rateErr error, sources []source, opts options, e env, res result) string {
	summary := fmt.Sprintf("tl: %d session(s), %d reply(ies); pct100_<provider> — %% of provider $100 subscription week; usd_api — API list price (price table %s); rates: %s",
		sessionCount, total.replies, price.Checked, ratePath)
	if rateErr != nil {
		summary += "; no subscription rates (" + rateErr.Error() + "); configure XDG_CONFIG_HOME/akit/rates.tsv or ~/.config/akit/rates.tsv"
	} else {
		providers := map[string]bool{}
		for provider := range rates {
			providers[provider] = true
		}
		for _, rp := range replies {
			if provider := usageProvider(rp.r.Model); provider != "" {
				providers[provider] = true
			}
		}
		used := make([]string, 0, len(providers))
		for provider := range providers {
			if rate, ok := rates[provider]; ok && !rate.validFrom.IsZero() {
				used = append(used, provider+"="+rate.validFrom.Format(time.DateOnly))
			} else {
				used = append(used, provider+"=missing")
			}
		}
		slices.Sort(used)
		summary += "; valid_from: " + strings.Join(used, ",")
	}
	var invalidRates strings.Builder
	for _, provider := range slices.Sorted(maps.Keys(rates)) {
		if rate := rates[provider]; rate.invalid != "" {
			invalidRates.WriteString("; invalid subscription rate: ")
			invalidRates.WriteString(rate.invalid)
		}
	}
	summary += invalidRates.String()
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

func usageRow(name string, t *tally, providers []string) string {
	fields := []string{name, strconv.Itoa(t.replies), strconv.FormatInt(t.input, 10), strconv.FormatInt(t.cacheWrite, 10),
		strconv.FormatInt(t.cacheRead, 10), strconv.FormatInt(t.outp, 10)}
	for _, provider := range providers {
		pct := "-"
		if t.week100Replies[provider] > 0 {
			pct = fmt.Sprintf("%.2f", t.pctWeek100[provider])
		}
		fields = append(fields, pct)
	}
	fields = append(fields, fmt.Sprintf("%.2f", t.usd))
	return strings.Join(fields, "\t")
}

func usageMaxPct(t *tally, providers []string) float64 {
	var max float64
	for _, provider := range providers {
		if pct := t.pctWeek100[provider]; pct > max {
			max = pct
		}
	}
	return max
}

type usageRate struct {
	apiWeek, planMonth float64
	validFrom          time.Time
	line               int
	invalid            string
}

func (r usageRate) usable() bool {
	return r.invalid == "" && r.apiWeek > 0 && r.planMonth > 0 && !math.IsNaN(r.apiWeek) && !math.IsInf(r.apiWeek, 0) &&
		!math.IsNaN(r.planMonth) && !math.IsInf(r.planMonth, 0)
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
					return nil, fmt.Errorf("%s: header missing %s", path, name)
				}
			}
			continue
		}
		if len(fields) != width {
			return nil, fmt.Errorf("%s:%d: wrong number of columns", path, lineNo)
		}
		provider, rate, future, err := parseUsageRateRow(path, lineNo, fields, columns, now)
		if err != nil {
			return nil, err
		}
		if future {
			if _, exists := rates[provider]; !exists {
				rates[provider] = usageRate{}
			}
			continue
		}
		old, exists := rates[provider]
		if !exists || old.validFrom.IsZero() || rate.validFrom.After(old.validFrom) || rate.validFrom.Equal(old.validFrom) {
			rates[provider] = rate
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read rates %s: %w", path, err)
	}
	if width == 0 {
		return nil, fmt.Errorf("%s: no header", path)
	}
	return rates, nil
}

func parseUsageRateRow(path string, lineNo int, fields []string, columns map[string]int, now time.Time) (string, usageRate, bool, error) {
	provider := fields[columns["provider"]]
	valid, err := parseUsageValidFrom(fields[columns["valid_from"]])
	if err != nil {
		return "", usageRate{}, false, fmt.Errorf("%s:%d:valid_from: invalid date", path, lineNo)
	}
	if valid.After(now) {
		return provider, usageRate{validFrom: valid}, true, nil
	}
	week, weekErr := strconv.ParseFloat(fields[columns["api_usd_week"]], 64)
	plan, planErr := strconv.ParseFloat(fields[columns["plan_usd_month"]], 64)
	rate := usageRate{apiWeek: week, planMonth: plan, validFrom: valid, line: lineNo}
	switch {
	case planErr != nil || !validRateNumber(plan):
		rate.invalid = fmt.Sprintf("%s:%d:plan_usd_month: invalid value %q", path, lineNo, fields[columns["plan_usd_month"]])
	case weekErr != nil || !validRateNumber(week):
		rate.invalid = fmt.Sprintf("%s:%d:api_usd_week: invalid value %q", path, lineNo, fields[columns["api_usd_week"]])
	}
	return provider, rate, false, nil
}

func validRateNumber(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
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
