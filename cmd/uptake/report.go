package main

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

func (u *run) writeReport() error {
	from, err := time.Parse(time.RFC3339Nano, u.start)
	if err != nil {
		return fmt.Errorf("parse report u.start: %w", err)
	}
	to, err := time.Parse(time.RFC3339Nano, u.end)
	if err != nil {
		return fmt.Errorf("parse report u.end: %w", err)
	}
	lastDay := to.In(time.Local).Format("2006-01-02")
	rows := u.reportTallies(lastDay)
	var b strings.Builder
	u.writeReportTitle(&b, from, to)
	u.writeReportUtilityRows(&b, rows)
	u.writeWebtextHookSummary(&b, from)
	u.writeReportIssueSummary(&b)
	u.writeHookInputSummary(&b)
	u.writeReportNotes(&b)
	u.writePairReport(&b)
	if err := os.WriteFile(filepath.Join(u.outputDir, "report.md"), []byte(b.String()), fileMode); err != nil {
		return fmt.Errorf("report.md: %w", err)
	}
	return nil
}

// pairReportTally — one layer of a pair; errors counts the new way's call errors and failures (callerr + fail).
type pairReportTally struct{ newEpisodes, oldEpisodes, mixedEpisodes, calls, errors, unknown int }

type pairReportLayer struct {
	agent, instructed, period string
}

func (u *run) writePairReport(b *strings.Builder) {
	b.WriteString("\n## Пары этапа 2")
	if u.labelsPath == "" {
		b.WriteString(" — доля: не проверено разметкой")
	}
	b.WriteString("\n\n")
	if u.releasesPath == "" {
		b.WriteString("даты выпуска не заданы; период — no-date.\n\n")
	}
	b.WriteString("| утилита | агент | instructed | период | точность (нижняя) | полнота (нижняя) | доля по эпизодам (95% Уилсон) | new / old / mixed | ошибки вызова (callerr + fail) |\n|---|---|---|---|---:|---:|---:|---:|---:|\n")
	for _, tool := range pairTools() {
		layers := u.pairReportLayers(tool)
		for _, layer := range layers {
			u.writePairReportRow(b, tool, layer, u.pairReportTallyForLayer(tool, layer))
		}
	}
	fmt.Fprintf(b, "\nВне пар busy: без последующего решения — %d; `busy --quiet` — %d.\n", u.busyNoDecision, u.busyQuiet)
	fmt.Fprintf(b, "Поиск решений (по правилу): %d вызовов.\n", u.decisionRuleSearches)
	fmt.Fprintf(b, "Prior old по источнику: .claude/improve/ — %d вызовов; tl --role user --grep — %d вызовов.\n", u.priorImproveCalls, u.priorTLCalls)
	t := u.tkRefusalTotals()
	fmt.Fprintf(b, "Отказы tk: %d; исправлены потом %d (той же подкомандой %d, другой %d), обход ручной правкой %d, сессия кончилась раньше 10 мин %d, брошены %d — [tk-refusals.tsv](tk-refusals.tsv).\n", t.refusals, t.same+t.other, t.same, t.other, t.bypass, t.incomplete, t.dropped)
	b.WriteString("\n## Ограничения замера\n\nЦель определяется по ключу, а не по смыслу; instructed означает упоминание утилиты, а не обязательность её применения. Полнота детекторов оценивается только внутри широкого надмножества кандидатов. Взвешивания по составу проектов нет; версии утилит не учитываются. Повтор не означает ошибку. Польза tk измерена прокси.\n")
	b.WriteString("Неизвестные исходы новых пар по причинам: ")
	if len(u.pairUnknownReasons) == 0 {
		b.WriteString("нет; ")
	} else {
		for i, reason := range slices.Sorted(maps.Keys(u.pairUnknownReasons)) {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(b, "%s — %d", reason, u.pairUnknownReasons[reason])
		}
		b.WriteString("; ")
	}
	fmt.Fprintf(b, "отклонённых вызовов tool use не учтено: %d.\n", u.rejectedToolUses)
}

func (u *run) pairReportLayers(tool string) []pairReportLayer {
	set := map[pairReportLayer]bool{}
	for i := range u.pairCalls {
		c := &u.pairCalls[i]
		if c.tool != tool || c.goal == "" {
			continue
		}
		instructed := "no"
		if c.instructed {
			instructed = "yes"
		}
		period := c.period
		if period == "" {
			period = "no-date"
		}
		agent := c.agent
		if agent == "" {
			agent = "unknown"
		}
		set[pairReportLayer{agent, instructed, period}] = true
	}
	if len(set) == 0 {
		set[pairReportLayer{"unknown", "no", "no-date"}] = true
	}
	layers := make([]pairReportLayer, 0, len(set))
	for layer := range set {
		layers = append(layers, layer)
	}
	sort.Slice(layers, func(i, j int) bool {
		a, c := layers[i], layers[j]
		return strings.Join([]string{a.agent, a.instructed, a.period}, "\t") < strings.Join([]string{c.agent, c.instructed, c.period}, "\t")
	})
	return layers
}

func (u *run) pairReportTallyForLayer(tool string, layer pairReportLayer) pairReportTally {
	tally := pairReportTally{}
	episodes := map[string][]pairCall{}
	for i := range u.pairCalls {
		call := &u.pairCalls[i]
		agent, instructed, period := call.agent, "no", call.period
		if call.instructed {
			instructed = "yes"
		}
		if agent == "" {
			agent = "unknown"
		}
		if period == "" {
			period = "no-date"
		}
		if call.tool != tool || call.goal == "" || agent != layer.agent || instructed != layer.instructed || period != layer.period {
			continue
		}
		episodes[call.session+"|"+call.goal] = append(episodes[call.session+"|"+call.goal], *call)
		if call.way == "new" {
			tally.calls++
			if call.outcome == "fail" || call.outcome == "callerr" {
				tally.errors++
			}
			if call.outcome == "unknown" {
				tally.unknown++
			}
		}
	}
	for _, calls := range episodes {
		sort.Slice(calls, func(i, j int) bool { return calls[i].at.Before(calls[j].at) })
		tallyPairEpisodes(&tally, calls)
	}
	return tally
}

func tallyPairEpisodes(tally *pairReportTally, calls []pairCall) {
	for start := 0; start < len(calls); {
		end, ways := reportEpisodeEnd(calls, start)
		switch {
		case ways["new"] && ways["old"]:
			tally.mixedEpisodes++
		case ways["new"]:
			tally.newEpisodes++
		default:
			tally.oldEpisodes++
		}
		start = end
	}
}

func reportEpisodeEnd(calls []pairCall, start int) (int, map[string]bool) {
	end := start + 1
	ways := map[string]bool{calls[start].way: true}
	for end < len(calls) && calls[end].at.Sub(calls[end-1].at) <= 30*time.Minute {
		ways[calls[end].way] = true
		end++
	}
	return end, ways
}

// labelVerdict — the blind labelling check of a pair detector: gate is "ok" or why the share is withheld
// (unlabelled, no-yes-sample, precision<0.8, recall<0.7); reason says it in the report's words.
type labelVerdict struct {
	precision, recall float64
	gate, reason      string
}

func (u *run) pairLabelMetrics(tool string) labelVerdict {
	if u.labelsPath == "" {
		return labelVerdict{gate: "unlabelled", reason: "не проверено разметкой"}
	}
	nYes, nNo := 0, 0
	for i := range u.pairCandidates {
		c := &u.pairCandidates[i]
		if c.pair != tool {
			continue
		}
		if c.yes {
			nYes++
		} else {
			nNo++
		}
	}
	sampleYes, trueYes, sampleNo, falseYes := u.pairLabelCounts(tool)
	if sampleYes == 0 {
		return labelVerdict{gate: "no-yes-sample", reason: "нет кандидатов «да» — точность не определена"}
	}
	pLo, _ := u.wilson(trueYes, sampleYes)
	// Without labelled "no" candidates the miss rate is unknown: take its worst case, not zero.
	qHi := 0.0
	if sampleNo > 0 {
		_, qHi = u.wilson(falseYes, sampleNo)
	} else if nNo > 0 {
		qHi = 1
	}
	denom := pLo*float64(nYes) + qHi*float64(nNo)
	recallLo := 0.0
	if denom > 0 {
		recallLo = pLo * float64(nYes) / denom
	}
	v := labelVerdict{precision: pLo, recall: recallLo, gate: "ok"}
	if pLo < 0.8 {
		v.gate, v.reason = "precision<0.8", fmt.Sprintf("разметка: точность %.2f < 0,8", pLo)
	} else if recallLo < 0.7 {
		v.gate, v.reason = "recall<0.7", fmt.Sprintf("разметка: полнота %.2f < 0,7", recallLo)
	}
	return v
}

// pairLabelCounts counts the labelled sample of a pair: detector "yes" candidates and how many of them the
// labels confirm, detector "no" candidates and how many of them the labels call "yes".
func (u *run) pairLabelCounts(tool string) (sampleYes, trueYes, sampleNo, falseYes int) {
	for i := range u.pairCandidateSample {
		c := &u.pairCandidateSample[i]
		if c.pair != tool {
			continue
		}
		label, ok := u.labels[c.id]
		if !ok {
			continue
		}
		if c.yes {
			sampleYes++
			if label == "yes" {
				trueYes++
			}
		} else {
			sampleNo++
			if label == "yes" {
				falseYes++
			}
		}
	}
	return sampleYes, trueYes, sampleNo, falseYes
}

func (u *run) pairLabelGate(tool string) string {
	return u.pairLabelMetrics(tool).gate
}

func (u *run) writePairReportRow(b *strings.Builder, tool string, layer pairReportLayer, tally pairReportTally) {
	total := tally.newEpisodes + tally.oldEpisodes
	verdict := u.pairLabelMetrics(tool)
	precisionText, recallText := "—", "—"
	if verdict.gate == "unlabelled" {
		precisionText, recallText = verdict.reason, verdict.reason
	} else if verdict.gate != "no-yes-sample" {
		precisionText = fmt.Sprintf("%.3f", verdict.precision)
		recallText = fmt.Sprintf("%.3f", verdict.recall)
	}
	share := "мало данных"
	if verdict.gate != "ok" {
		share = verdict.reason
	} else if total >= 20 {
		low, high := u.wilson(tally.newEpisodes, total)
		share = fmt.Sprintf("%.1f%% (%.1f–%.1f%%)", 100*float64(tally.newEpisodes)/float64(total), 100*low, 100*high)
	}
	fmt.Fprintf(b, "| %s | %s | %s | %s | %s | %s | %s | %d / %d / %d | %d |\n",
		tool, layer.agent, layer.instructed, layer.period, precisionText, recallText, share,
		tally.newEpisodes, tally.oldEpisodes, tally.mixedEpisodes, tally.errors)
}

type reportTally struct{ window, recent, old int }

func (u *run) reportTallies(lastDay string) map[string]*reportTally {
	rows := make(map[string]*reportTally)
	for key, n := range u.counts {
		p := strings.Split(key, "\t")
		if len(p) != 4 || p[3] != "calls" {
			continue
		}
		r := rows[p[2]]
		if r == nil {
			r = &reportTally{}
			rows[p[2]] = r
		}
		r.window += n
		day, err := time.ParseInLocation("2006-01-02", p[0], time.Local)
		if err == nil && day.Format("2006-01-02") == lastDay {
			r.recent += n
		}
	}
	for key, n := range u.counts {
		p := strings.Split(key, "\t")
		if len(p) == 4 && p[3] == "calls" && strings.HasPrefix(p[2], "habit-") {
			tool := strings.TrimPrefix(p[2], "habit-")
			if r := rows[tool]; r != nil {
				r.old += n
			}
		}
	}
	return rows
}

func (u *run) writeReportTitle(b *strings.Builder, from, to time.Time) {
	fmt.Fprintf(b, "# Замер внедрения akit\n\nОкно: %s — %s (%s). Последние сутки показаны отдельно; время событий сгруппировано в местном часовом поясе.\n\n", from.In(time.Local).Format("2006-01-02 15:04 MST"), to.In(time.Local).Format("2006-01-02 15:04 MST"), time.Local)
	fmt.Fprintf(b, "| утилита | %s / окно | доля от старого способа | польза | crash / неудачи утилиты | killed | call-error |\n|---|---:|---:|---|---:|---:|---:|\n", to.In(time.Local).Format("02.01"))
}

func (u *run) writeReportUtilityRows(b *strings.Builder, rows map[string]*reportTally) {
	for _, tool := range u.utilityNames {
		r := rows[tool]
		if r == nil {
			r = &reportTally{}
		}
		share := "пары нет"
		if contains(pairTools(), tool) {
			share = "см. «Пары этапа 2»"
		} else if r.old > 0 {
			share = fmt.Sprintf("%.1f%%", 100*float64(r.window)/float64(r.window+r.old))
		}
		crash, total, killed, callerr := u.reportFaultCounts(tool)
		benefit := "не замерено"
		switch tool {
		case "webtext":
			benefit = fmt.Sprintf("открыто %d/%d; пропущено %d", u.hookSummary.opened, u.hookSummary.total, u.hookSummary.skipped)
		case "tk":
			t := u.tkRefusalTotals()
			benefit = fmt.Sprintf("отказов %d, исправлено потом %d (tk-refusals.tsv)", t.refusals, t.same+t.other)
		}
		fmt.Fprintf(b, "| %s | %d / %d | %s | %s | %d/%d | %d | %d |\n", tool, r.recent, r.window, share, benefit, crash, total, killed, callerr)
	}
}

func (u *run) writeWebtextHookSummary(b *strings.Builder, since time.Time) {
	fmt.Fprintf(b, "\nwebtext через хук: открыто %d из %d отказов WebFetch (direct %d, wayback %d), пропущено %d.\n", u.hookSummary.opened, u.hookSummary.total, u.hookSummary.direct, u.hookSummary.wayback, u.hookSummary.skipped)
	switch {
	case !u.hookSummary.hasCoverage:
		b.WriteString("Покрытие лога: нет доступного времени первой строки.\n")
	case u.hookSummary.coverage.After(since):
		fmt.Fprintf(b, "Покрытие лога: лог покрывает окно с %s, раньше — нет данных.\n", u.hookSummary.coverage.In(time.Local).Format("2006-01-02 15:04:05"))
	default:
		fmt.Fprintf(b, "Покрытие лога: первая строка старейшего доступного файла — %s.\n", u.hookSummary.coverage.In(time.Local).Format("2006-01-02 15:04:05"))
	}
	if len(u.hookMissing) > 0 {
		fmt.Fprintf(b, "Отсутствуют файлы лога: %s.\n", strings.Join(u.hookMissing, ", "))
	}
}

func (u *run) writeReportIssueSummary(b *strings.Builder) {
	if u.unreadFiles > 0 || u.badLines > 0 {
		first := u.firstBad
		if first == "" {
			first = u.firstUnread
		}
		fmt.Fprintf(b, "\nНе прочитано файлов: %d; повреждённых строк: %d. Первый путь с проблемой: `%s`.", u.unreadFiles, u.badLines, first)
		if u.firstUnread != "" && u.firstUnread != first {
			fmt.Fprintf(b, " Первый не прочитанный файл: `%s`.", u.firstUnread)
		}
		b.WriteString("\n")
	}
}

func (u *run) writeHookInputSummary(b *strings.Builder) {
	if len(u.hookReadErrors) == 0 && u.hookBadLines == 0 {
		return
	}
	if len(u.hookReadErrors) > 0 {
		fmt.Fprintf(b, "\nНе прочитано файлов журнала хуков: %d. Первый путь с проблемой: `%s`.\n", len(u.hookReadErrors), u.hookReadErrors[0])
	}
	if u.hookBadLines > 0 {
		fmt.Fprintf(b, "Повреждённых строк в журналах хуков: %d. Первый путь: `%s`.\n", u.hookBadLines, u.firstHookBad)
	}
}

func (u *run) writeReportNotes(b *strings.Builder) {
	b.WriteString("\nДоля показывает вызовы утилиты среди вызовов утилиты и распознанного прежнего способа. Для инструментов без сопоставимого старого способа доля не рассчитывается. Счёт относится только к событиям внутри указанного окна.\n\n")
	b.WriteString("Исходы вызовов и признаки ошибок собраны в [errors.tsv](errors.tsv); повторы — в [repeats.tsv](repeats.tsv). Пары записей tk и ручных исправлений приведены в [tk-pair.tsv](tk-pair.tsv). Пустые поиски tl — в [tl-empty.tsv](tl-empty.tsv), прокси объёма ввода при правках — в [sub-cost.tsv](sub-cost.tsv). Примеры событий находятся в examples.tsv.\n\n")
	b.WriteString("Таблицы описывают наблюдаемые вызовы и не устанавливают причинность. Неизвестный исход остаётся неизвестным; штатные ответы утилит учитываются отдельно от ошибок вызова.\n")
}

func (u *run) reportFaultCounts(tool string) (crash, total, killed, callerr int) {
	for key, n := range u.counts {
		p := strings.Split(key, "\t")
		if len(p) != 4 || p[2] != tool {
			continue
		}
		if p[3] == "crash" {
			crash += n
		}
		if p[3] == "killed" {
			killed += n
		}
		if p[3] == "call-error" {
			callerr += n
		}
	}
	for key, stats := range u.errorStats {
		p := strings.Split(key, "\t")
		if len(p) == 2 && p[0] == tool && p[1] == "new" {
			total += stats.fail
		}
	}
	return crash, total, killed, callerr
}
