package main

import (
	"fmt"
	"os"
	"path/filepath"
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
	if err := os.WriteFile(filepath.Join(u.outputDir, "report.md"), []byte(b.String()), fileMode); err != nil {
		return fmt.Errorf("report.md: %w", err)
	}
	return nil
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
		if r.old > 0 {
			share = fmt.Sprintf("%.1f%%", 100*float64(r.window)/float64(r.window+r.old))
		}
		crash, total, killed, callerr := u.reportFaultCounts(tool)
		benefit := "не замерено"
		if tool == "webtext" {
			benefit = fmt.Sprintf("открыто %d/%d; пропущено %d", u.hookSummary.opened, u.hookSummary.total, u.hookSummary.skipped)
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
