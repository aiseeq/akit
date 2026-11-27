package main

import (
	"context"
	"encoding/json"
	"errors"
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

	"github.com/aiseeq/akit/session"
	"github.com/aiseeq/akit/transcriptdb"
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
	if c, st := transcriptdb.Backend("user-word", e.home, stderr); c != nil {
		activeWordDB = &wordDB{client: c, through: st.DBThrough, stderr: stderr}
	}
}

func (d *wordDB) query(name, sql string, params map[string]string) ([][]string, error) {
	rows, err := d.client.Select(context.Background(), name, sql, params)
	if err == nil && !d.fresh {
		fmt.Fprint(d.stderr, transcriptdb.FreshnessLine("user-word", d.through, time.Now()))
		d.fresh = true
	}
	return rows, err
}

// errReadFile — транскрипт читается из файла, а запуск остаётся на базе: в базе нет того, что даёт файл.
var errReadFile = errors.New("the database lacks what the file gives")

// lines — реплики и ответы агента транскрипта path из базы, как readLines из файла. Транскрипта нет в базе - ошибка:
// он читается из файла; в нём есть битые строки - тоже из файла (предупреждение о них - с ошибкой разбора первой).
func (d *wordDB) lines(path string) ([]line, error) {
	params := map[string]string{"path": transcriptdb.String(path)}
	rows, err := d.query("lines", "SELECT toString(events.file_id), toString(events.line), toString(toUnixTimestamp64Milli(events.ts)), events.role, events.text, "+
		"toString(events.images), toString(f.bad) FROM events "+
		"INNER JOIN (SELECT file_id, ev_lines, bad FROM files FINAL WHERE path = {path:String}) AS f USING (file_id) "+
		"WHERE events.file_id IN (SELECT file_id FROM files FINAL WHERE path = {path:String}) AND events.line <= f.ev_lines "+
		"AND events.role IN ('user','ask','agent') ORDER BY events.line", params)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("transcript %s: no lines in the database", path)
	}
	out := make([]line, 0, len(rows))
	var asks []rawKey
	var askAt []int
	for _, r := range rows {
		if len(r) != 7 {
			return nil, fmt.Errorf("events: %d columns, want 7", len(r))
		}
		if r[6] != "0" {
			return nil, errReadFile
		}
		l, key, ok, err := eventLine(r[0], r[1], r[2], r[3], r[4], r[5])
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if l.ask {
			asks, askAt = append(asks, key), append(askAt, len(out))
		}
		out = append(out, l)
	}
	raws, err := d.rawLines(asks)
	if err != nil {
		return nil, err
	}
	for i, key := range asks {
		out[askAt[i]].match = askAnswer(raws[key])
	}
	return out, nil
}

// rawKey — строка файла в raw.
type rawKey struct {
	file uint64
	line int
}

// eventLine — строка readLines из полей events: id файла, номер строки, время в миллисекундах, роль, текст, число
// картинок. В базе текст для показа - с «[image]» на картинку, файл даёт слова без них. Ответ окна вопроса (match)
// в базе отдельно не хранится: его даёт сырая строка (askAnswer). Роль не user, ask или agent - ok false.
func eventLine(file, no, ms, role, text, images string) (line, rawKey, bool, error) {
	id, err := strconv.ParseUint(file, 10, 64)
	if err != nil {
		return line{}, rawKey{}, false, fmt.Errorf("events file: %w", err)
	}
	n, err := strconv.Atoi(no)
	if err != nil {
		return line{}, rawKey{}, false, fmt.Errorf("events line: %w", err)
	}
	at, err := strconv.ParseInt(ms, 10, 64)
	if err != nil {
		return line{}, rawKey{}, false, fmt.Errorf("events time: %w", err)
	}
	count, err := strconv.Atoi(images)
	if err != nil {
		return line{}, rawKey{}, false, fmt.Errorf("events images: %w", err)
	}
	l := line{no: n, t: time.UnixMilli(at), text: withoutImages(text, count)}
	switch role {
	case "user":
		l.user, l.match = true, l.text
	case "ask":
		l.user, l.ask = true, true
	case "agent":
	default:
		return line{}, rawKey{}, false, nil
	}
	return l, rawKey{file: id, line: n}, true, nil
}

// imageMark — отметка картинки, которую transcript.Event.Shown добавляет к словам события.
const imageMark = "[image]"

// withoutImages — слова события по тексту для показа (Shown) и числу картинок: без отметок картинок.
func withoutImages(shown string, images int) string {
	if images <= 0 {
		return shown
	}
	marks := strings.TrimSpace(strings.Repeat(imageMark+" ", images))
	if shown == marks {
		return ""
	}
	return strings.TrimSuffix(shown, "\n"+marks)
}

// askAnswer — ответы окна вопроса по сырой записи с его выводом, как Answer события ask у transcript: по строке на
// вопрос, в порядке вопросов, без их текста. Нет ответов - пусто.
func askAnswer(raw []byte) string {
	var rec struct {
		Result struct {
			Answers map[string]string `json:"answers"`
		} `json:"toolUseResult"`
	}
	if json.Unmarshal(raw, &rec) != nil {
		return ""
	}
	answers := make([]string, 0, len(rec.Result.Answers))
	for _, q := range slices.Sorted(maps.Keys(rec.Result.Answers)) {
		answers = append(answers, rec.Result.Answers[q])
	}
	return strings.Join(answers, "\n")
}

// rawLines — сырые строки файлов по ключам. Строки нет - ошибка: без неё ответ базы не повторить.
func (d *wordDB) rawLines(keys []rawKey) (map[rawKey][]byte, error) {
	out := make(map[rawKey][]byte, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	rows, err := d.query("raw", "SELECT toString(file_id), toString(line), raw FROM raw WHERE (file_id, line) IN {keys:Array(Tuple(UInt64, UInt32))}",
		map[string]string{"keys": transcriptdb.ArrayParam(keys, func(k rawKey) string {
			return "(" + strconv.FormatUint(k.file, 10) + "," + strconv.Itoa(k.line) + ")"
		})})
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if len(r) != 3 {
			return nil, fmt.Errorf("raw: %d columns, want 3", len(r))
		}
		id, err1 := strconv.ParseUint(r[0], 10, 64)
		n, err2 := strconv.Atoi(r[1])
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("raw key %q %q", r[0], r[1])
		}
		out[rawKey{file: id, line: n}] = []byte(r[2])
	}
	for _, k := range keys {
		if _, ok := out[k]; !ok {
			return nil, fmt.Errorf("raw: no line %d of file %d", k.line, k.file)
		}
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
	rows, err := d.query("brief_candidates", "SELECT DISTINCT files.path FROM events INNER JOIN files FINAL USING (file_id) "+
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
	rows, err := d.query("brief_record_days", "SELECT "+strings.Join(columns, ", ")+" FROM events WHERE kind = 'claude' "+
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
	rows, err := activeWordDB.query("search", "SELECT files.path, toString(events.file_id), toString(events.line), toString(toUnixTimestamp64Milli(events.ts)), "+
		"events.role, events.text, toString(events.images) "+
		"FROM events INNER JOIN files FINAL USING (file_id) WHERE events.line <= files.ev_lines AND events.kind = 'claude' "+
		"AND events.role IN ('user','ask')"+filter+" ORDER BY events.ts, files.path, events.line", params)
	if err != nil {
		return nil, nil, err
	}
	keep := hitKeep(opts.frag)
	candidates, need, err := searchCandidates(rows, opts, e, keep)
	if err != nil {
		return nil, nil, err
	}
	raws, err := activeWordDB.rawLines(need)
	if err != nil {
		return nil, nil, err
	}
	onDisk := make(map[string]bool)
	var hits []hit
	seen := map[string]bool{}
	for i := range candidates {
		c := &candidates[i]
		if c.raw && !c.keptRaw(raws[c.key], keep, opts.frag) {
			continue
		}
		key := c.t.String() + "\x00" + c.text
		if seen[key] || !fileOnDisk(onDisk, c.path) {
			continue
		}
		seen[key] = true
		hits = append(hits, c.hit)
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].t.Before(hits[j].t) })
	return hits, nil, nil
}

// searchCandidate — реплика из базы, которая может стать находкой; raw - решает её сырая строка.
type searchCandidate struct {
	hit
	key rawKey
	raw bool
}

// searchCandidates — реплики строк базы, которые стали бы находками в файлах, и сырые строки, нужные для решения.
// Файлы отсеивают сырые строки до разбора (findHitsUsing, keep): короткое слово проходит только целым словом в
// сырой строке, и реплика, где оно лишь внутри другого слова или после «\n», находкой не становится. Текст реплики
// в JSON-строке проходит отсев так же, как её сырая строка; не прошёл - решает сама сырая строка. Ответ окна вопроса
// есть только в сырой строке.
func searchCandidates(rows [][]string, opts options, e env, keep func([]byte) bool) ([]searchCandidate, []rawKey, error) {
	var candidates []searchCandidate
	var need []rawKey
	for _, r := range rows {
		if len(r) != 7 {
			return nil, nil, fmt.Errorf("events: %d columns, want 7", len(r))
		}
		path := r[0]
		if !sessionTranscript(e.home, path) || !inProject(path, opts.project) {
			continue
		}
		l, key, ok, err := eventLine(r[1], r[2], r[3], r[4], r[5], r[6])
		if err != nil {
			return nil, nil, err
		}
		if !ok || !l.user || opts.since != "" && l.t.In(e.loc).Format(time.DateOnly) < opts.since ||
			!l.ask && !hasFragment(l.text, opts.frag) {
			continue
		}
		c := searchCandidate{hit: hit{path: path, line: l}, key: key, raw: l.ask || !keep(jsonEscaped(l.text))}
		if c.raw {
			need = append(need, key)
		}
		candidates = append(candidates, c)
	}
	return candidates, need, nil
}

// keptRaw — сырая строка проходит отсев файлов; у ответа окна вопроса из неё берутся ответы, и кусок должен быть в них.
func (c *searchCandidate) keptRaw(raw []byte, keep func([]byte) bool, frag string) bool {
	if !keep(raw) {
		return false
	}
	if c.ask {
		c.match = askAnswer(raw)
		return hasFragment(c.match, frag)
	}
	return true
}

// inProject — путь в проекте --project, как у transcripts: подстрока пути или папки проекта.
func inProject(path, project string) bool {
	return project == "" || strings.Contains(path, project) || strings.Contains(path, notAlnum.ReplaceAllString(project, "-"))
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
		fmt.Fprintf(activeWordDB.stderr, "user-word: database: %v; reading files instead\n", err)
		activeWordDB = nil
	}
}
