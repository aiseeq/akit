package main

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/akit/internal/transcriptdb"
	"github.com/aiseeq/akit/session"
)

type wordDB struct {
	client  *transcriptdb.Client
	through time.Time
	stderr  io.Writer
	fresh   bool
}

var activeWordDB *wordDB

func initWordDB(e env, stderr io.Writer) {
	activeWordDB = nil
	if os.Getenv("TL_BACKEND") != "db" {
		return
	}
	st, err := transcriptdb.CheckHealth(e.home)
	var c *transcriptdb.Client
	if err == nil {
		c, err = transcriptdb.NewFromEnv()
	}
	if err != nil {
		fmt.Fprintf(stderr, "user-word: TL_BACKEND=db: %v; reading files instead\n", err)
		return
	}
	activeWordDB = &wordDB{client: c, through: st.DBThrough, stderr: stderr}
}

func (d *wordDB) query(sql string, params map[string]string) ([][]string, error) {
	rows, err := d.client.Select(context.Background(), "user_word", sql, params)
	if err == nil && !d.fresh {
		fmt.Fprint(d.stderr, transcriptdb.FreshnessLine("user-word", d.through, time.Now()))
		d.fresh = true
	}
	return rows, err
}

// lines — реплики и ответы агента транскрипта path из базы, как readLines из файла. Транскрипта нет в базе - ошибка:
// он читается из файла.
func (d *wordDB) lines(path string) ([]line, error) {
	params := map[string]string{"path": transcriptdb.String(path)}
	rows, err := d.query("SELECT toString(events.line), toString(toUnixTimestamp64Milli(events.ts)), events.role, events.text FROM events "+
		"INNER JOIN (SELECT file_id, ev_lines FROM files FINAL WHERE path = {path:String}) AS f USING (file_id) "+
		"WHERE events.file_id IN (SELECT file_id FROM files FINAL WHERE path = {path:String}) AND events.line <= f.ev_lines "+
		"AND events.role IN ('user','ask','agent') ORDER BY events.line", params)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("transcript %s: no lines in the database", path)
	}
	out := make([]line, 0, len(rows))
	for _, r := range rows {
		if len(r) != 4 {
			return nil, fmt.Errorf("events: %d columns, want 4", len(r))
		}
		no, e := strconv.Atoi(r[0])
		if e != nil {
			return nil, e
		}
		ms, e := strconv.ParseInt(r[1], 10, 64)
		if e != nil {
			return nil, e
		}
		l := line{no: no, t: time.UnixMilli(ms), text: r[3]}
		switch r[2] {
		case "user":
			l.user = true
			l.match = l.text
		case "ask":
			l.user = true
			l.ask = true
			l.match = l.text
			if before, _, ok := strings.Cut(l.text, " ← "); ok {
				l.match = before
			}
		case "agent":
		default:
			continue
		}
		out = append(out, l)
	}
	return out, nil
}

// sessionTranscript — путь вида <папка проектов>/<проект>/<сессия>.jsonl: такие перечисляет transcripts, суб-агенты
// и прочие файлы базы туда не входят.
func sessionTranscript(home, path string) bool {
	project := filepath.Dir(path)
	return filepath.Ext(path) == ".jsonl" && filepath.Dir(project) == session.ProjectsDir(home) && filepath.Base(project) != ""
}

// briefCandidates — транскрипты сессий (как у briefTranscriptInventory: без суб-агентов), где в дни цитат есть
// реплика пользователя с куском слова цитаты: проход цитат --brief читает с диска только их, по порядку путей.
// Кусок - prefilterBest: без первой буквы и без «е», поэтому есть и в lowerUTF8 реплики при любом регистре и «ё».
func (d *wordDB) briefCandidates(home string, fragments map[string][]string, loc *time.Location) ([]string, error) {
	params := map[string]string{}
	var conds []string
	for i, date := range slices.Sorted(maps.Keys(fragments)) {
		day, err := time.ParseInLocation(time.DateOnly, date, loc)
		if err != nil {
			return nil, fmt.Errorf("parse marker day %q: %w", date, err)
		}
		params[fmt.Sprintf("s%d", i)] = transcriptdb.Time(day)
		params[fmt.Sprintf("e%d", i)] = transcriptdb.Time(day.AddDate(0, 0, 1))
		cond := fmt.Sprintf("events.ts >= {s%d:DateTime64(3, 'UTC')} AND events.ts < {e%d:DateTime64(3, 'UTC')}", i, i)
		if terms := briefQuoteTerms(fragments[date]); len(terms) > 0 {
			params[fmt.Sprintf("t%d", i)] = transcriptdb.Strings(terms)
			cond += fmt.Sprintf(" AND multiSearchAny(events.hay, {t%d:Array(String)})", i)
		}
		conds = append(conds, "("+cond+")")
	}
	if len(conds) == 0 {
		return nil, nil
	}
	rows, err := d.query("SELECT DISTINCT files.path FROM events INNER JOIN files FINAL USING (file_id) "+
		"WHERE events.role IN ('user','ask') AND events.kind = 'claude' AND events.line <= files.ev_lines AND ("+
		strings.Join(conds, " OR ")+") ORDER BY files.path", params)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, r := range rows {
		if sessionTranscript(home, r[0]) {
			paths = append(paths, r[0])
		}
	}
	return paths, nil
}

// briefQuoteTerms — куски слов цитат дня для отбора в базе; нет куска хоть у одной цитаты - нет отбора по тексту.
func briefQuoteTerms(fragments []string) []string {
	terms := make([]string, 0, len(fragments))
	for _, fragment := range fragments {
		term, _ := prefilterBest(briefQuoteFragment(fragment))
		if term == "" {
			return nil
		}
		terms = append(terms, term)
	}
	return terms
}

// briefRecordDays — окна дней пометок, где в транскриптах сессий есть записи: заметка «не найдено» зависит от них.
func (d *wordDB) briefRecordDays(recordWindows map[string]markerRecordInterval) (map[string]bool, error) {
	recordDays := make(map[string]bool)
	if len(recordWindows) == 0 {
		return recordDays, nil
	}
	keys := slices.Sorted(maps.Keys(recordWindows))
	params := map[string]string{}
	columns := make([]string, 0, len(keys))
	var lo, hi time.Time
	for i, key := range keys {
		window := recordWindows[key]
		params[fmt.Sprintf("s%d", i)] = transcriptdb.Time(window.start)
		params[fmt.Sprintf("e%d", i)] = transcriptdb.Time(window.end)
		columns = append(columns, fmt.Sprintf("toString(countIf(ts >= {s%d:DateTime64(3, 'UTC')} AND ts < {e%d:DateTime64(3, 'UTC')}))", i, i))
		if lo.IsZero() || window.start.Before(lo) {
			lo = window.start
		}
		if window.end.After(hi) {
			hi = window.end
		}
	}
	params["lo"], params["hi"] = transcriptdb.Time(lo), transcriptdb.Time(hi)
	rows, err := d.query("SELECT "+strings.Join(columns, ", ")+" FROM events WHERE kind = 'claude' "+
		"AND ts >= {lo:DateTime64(3, 'UTC')} AND ts < {hi:DateTime64(3, 'UTC')}", params)
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 || len(rows[0]) != len(keys) {
		return nil, fmt.Errorf("record days: %d rows", len(rows))
	}
	for i, key := range keys {
		if rows[0][i] != "0" {
			recordDays[key] = true
		}
	}
	return recordDays, nil
}

// dbFindHits — findHitsUsing по базе: реплики пользователя с куском из транскриптов сессий, которые выбрал бы
// transcripts (путь вида проекты/*/*.jsonl, --project - подстрокой пути, файл есть на диске), в том же порядке.
func dbFindHits(opts options, e env) ([]hit, []line, error) {
	params := map[string]string{}
	filter := ""
	// Кусок prefilterBest есть в lowerUTF8 реплики при любом регистре и «ё»; без него - все реплики окна.
	if term, _ := prefilterBest(opts.frag); term != "" {
		filter += " AND multiSearchAny(events.hay, {terms:Array(String)})"
		params["terms"] = transcriptdb.Strings([]string{term})
	}
	if opts.since != "" {
		since, err := time.ParseInLocation(time.DateOnly, opts.since, e.loc)
		if err != nil {
			return nil, nil, fmt.Errorf("--since: %w", err)
		}
		filter += " AND events.ts >= {since:DateTime64(3, 'UTC')}"
		params["since"] = transcriptdb.Time(since)
	}
	rows, err := activeWordDB.query("SELECT files.path, toString(events.line), toString(toUnixTimestamp64Milli(events.ts)), events.role, events.text "+
		"FROM events INNER JOIN files FINAL USING (file_id) WHERE events.line <= files.ev_lines AND events.kind = 'claude' "+
		"AND events.role IN ('user','ask')"+filter+" ORDER BY events.ts, files.path, events.line", params)
	if err != nil {
		return nil, nil, err
	}
	onDisk := make(map[string]bool)
	var hits []hit
	seen := map[string]bool{}
	for _, r := range rows {
		if len(r) != 5 {
			return nil, nil, fmt.Errorf("events: %d columns, want 5", len(r))
		}
		path := r[0]
		if !sessionTranscript(e.home, path) ||
			opts.project != "" && !strings.Contains(path, opts.project) && !strings.Contains(path, notAlnum.ReplaceAllString(opts.project, "-")) {
			continue
		}
		l, err := userEventLine(r[1], r[2], r[3], r[4])
		if err != nil {
			return nil, nil, err
		}
		if !hasFragment(searchableText(l), opts.frag) || opts.since != "" && l.t.In(e.loc).Format(time.DateOnly) < opts.since {
			continue
		}
		key := l.t.String() + "\x00" + l.text
		if seen[key] || !fileOnDisk(onDisk, path) {
			continue
		}
		seen[key] = true
		hits = append(hits, hit{path: path, line: l})
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].t.Before(hits[j].t) })
	return hits, nil, nil
}

// userEventLine — строка реплики user или ask из полей events: номер строки, время в миллисекундах, роль, текст.
func userEventLine(no, ms, role, text string) (line, error) {
	n, err := strconv.Atoi(no)
	if err != nil {
		return line{}, fmt.Errorf("events line: %w", err)
	}
	at, err := strconv.ParseInt(ms, 10, 64)
	if err != nil {
		return line{}, fmt.Errorf("events time: %w", err)
	}
	l := line{no: n, t: time.UnixMilli(at), user: true, ask: role == "ask", text: text, match: text}
	if l.ask {
		if answer, _, ok := strings.Cut(text, " ← "); ok {
			l.match = answer
		}
	}
	return l, nil
}

// fileOnDisk — есть ли файл: транскрипт, удалённый с диска, в базе остаётся, а файлы его уже не видят.
func fileOnDisk(known map[string]bool, path string) bool {
	exists, ok := known[path]
	if !ok {
		_, err := os.Stat(path)
		exists = err == nil
		known[path] = exists
	}
	return exists
}

func setWordFreshnessFailure(err error) {
	if activeWordDB != nil {
		fmt.Fprintf(activeWordDB.stderr, "user-word: TL_BACKEND=db: %v; reading files instead\n", err)
		activeWordDB = nil
	}
}
