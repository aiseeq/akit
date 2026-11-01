package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (u *run) writeReport() error {
	from, err := time.Parse(time.RFC3339, u.start)
	if err != nil {
		return fmt.Errorf("parse report u.start: %w", err)
	}
	to, err := time.Parse(time.RFC3339, u.end)
	if err != nil {
		return fmt.Errorf("parse report u.end: %w", err)
	}
	lastFrom := to.Add(-24 * time.Hour)
	type tally struct{ window, recent, old int }
	rows := make(map[string]*tally)
	for key, n := range u.counts {
		p := strings.Split(key, "\t")
		if len(p) != 4 || p[3] != "calls" {
			continue
		}
		r := rows[p[2]]
		if r == nil {
			r = &tally{}
			rows[p[2]] = r
		}
		r.window += n
		day, err := time.ParseInLocation("2006-01-02", p[0], time.Local)
		if err == nil && !day.Before(lastFrom.In(time.Local).Truncate(24*time.Hour)) {
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
	tools := u.utilityNames
	var b strings.Builder
	fmt.Fprintf(&b, "# Замер внедрения akit\n\nОкно: %s — %s (%s). Последние сутки показаны отдельно; время событий сгруппировано в местном часовом поясе.\n\n", from.In(time.Local).Format("2006-01-02 15:04 MST"), to.In(time.Local).Format("2006-01-02 15:04 MST"), time.Local)
	b.WriteString("| утилита | последние сутки / окно | доля от старого способа | crash подтверждено/всего | killed | call-error |\n|---|---:|---:|---:|---:|---:|\n")
	for _, tool := range tools {
		r := rows[tool]
		if r == nil {
			r = &tally{}
		}
		share := "нет пары"
		if r.window+r.old > 0 {
			share = fmt.Sprintf("%.1f%%", 100*float64(r.window)/float64(r.window+r.old))
		}
		crash, callerr := u.reportFaultCounts(tool)
		fmt.Fprintf(&b, "| %s | %d / %d | %s | %d/%d | 0 | %d |\n", tool, r.recent, r.window, share, crash, crash, callerr)
	}
	b.WriteString("\nДоля показывает вызовы утилиты среди вызовов утилиты и распознанного прежнего способа. Для инструментов без сопоставимого старого способа доля не рассчитывается. Счёт относится только к событиям внутри указанного окна.\n\n")
	b.WriteString("Исходы вызовов и признаки ошибок собраны в [errors.tsv](errors.tsv); повторы — в [repeats.tsv](repeats.tsv). Пары записей tk и ручных исправлений приведены в [tk-pair.tsv](tk-pair.tsv). Пустые поиски tl — в [tl-empty.tsv](tl-empty.tsv), прокси объёма ввода при правках — в [sub-cost.tsv](sub-cost.tsv). Примеры событий находятся в examples.tsv.\n\n")
	b.WriteString("Таблицы описывают наблюдаемые вызовы и не устанавливают причинность. Неизвестный исход остаётся неизвестным; штатные ответы утилит учитываются отдельно от ошибок вызова.\n")
	if err := os.WriteFile(filepath.Join(u.outputDir, "report.md"), []byte(b.String()), fileMode); err != nil {
		return fmt.Errorf("report.md: %w", err)
	}
	return nil
}

func (u *run) reportFaultCounts(tool string) (crash, callerr int) {
	for key, n := range u.counts {
		p := strings.Split(key, "\t")
		if len(p) != 4 || p[2] != tool {
			continue
		}
		if p[3] == "crash" {
			crash += n
		}
		if p[3] == "call-error" {
			callerr += n
		}
	}
	return crash, callerr
}
