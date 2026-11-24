package main

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/akit/transcriptdb"
)

// priorDB — база транскриптов при TL_BACKEND=db. Список реплик и при ней остаётся журналом: в нём реплики из
// транскриптов, которых уже нет, и консолей, которых в базе нет; база заменяет поиск номера строки реплики в
// транскрипте - чтение транскриптов с диска.
type priorDB struct {
	client  *transcriptdb.Client
	through time.Time
	stderr  io.Writer
}

var activePriorDB *priorDB

func priorBackend(home string, stderr io.Writer) *priorDB {
	c, state := transcriptdb.Backend("prior", home, stderr)
	if c == nil {
		return nil
	}
	return &priorDB{client: c, through: state.DBThrough, stderr: stderr}
}

// fresh — строка свежести в stderr, как у tl.
func (d *priorDB) fresh() {
	fmt.Fprint(d.stderr, transcriptdb.FreshnessLine("prior", d.through, time.Now()))
}

// replyLineWindow — сколько строк до первого события с тем же временем просматривается в поиске первой строки с
// этим временем: строки без событий (служебные записи) пишутся вплотную к событию.
const replyLineWindow = 256

// replyWindow — где в транскрипте искать иголку: первая строка события с её временем (last) и окно строк перед ней.
type replyWindow struct {
	fileID string
	path   string
	needle string
	last   int
}

func (w replyWindow) first() int { return max(1, w.last-replyLineWindow) }

// timestampLines — для каждого транскрипта и иголки `"timestamp":"…"` номер первой строки (с 1), где она есть, как
// у transcriptTimestampLines. Сначала по событиям - первая строка с этим временем, затем по сырым строкам в окне
// перед ней - первая строка с иголкой. Чего база не нашла, того в ответе нет: это ищется в файлах.
func (d *priorDB) timestampLines(needlesByPath map[string]map[string]struct{}) (map[string]map[string]int, error) {
	found := make(map[string]map[string]int)
	if len(needlesByPath) == 0 {
		return found, nil
	}
	windows, err := d.replyWindows(needlesByPath)
	if err != nil || len(windows) == 0 {
		return found, err
	}
	first, err := d.firstNeedleLines(windows)
	if err != nil {
		return nil, err
	}
	for _, w := range windows {
		// Строки с событиями в окне учтены по min(e.line); раньше окна иголка могла бы быть только в строке без
		// событий через сотни строк с другим временем. Найдена на нижней границе окна - ряд мог начаться раньше:
		// такая ищется в файле.
		n := first[w.path][w.needle]
		if n == 0 || n == w.first() && n > 1 {
			continue
		}
		if found[w.path] == nil {
			found[w.path] = make(map[string]int)
		}
		found[w.path][w.needle] = n
	}
	return found, nil
}

// replyWindows — по событиям: для каждой иголки, чьё время есть в событиях транскрипта, первая строка с этим временем.
func (d *priorDB) replyWindows(needlesByPath map[string]map[string]struct{}) ([]replyWindow, error) {
	paths := make([]string, 0, len(needlesByPath))
	var stamps []string
	stampNeedle := make(map[string]string)
	for path, needles := range needlesByPath {
		paths = append(paths, path)
		for needle := range needles {
			at, err := time.Parse(time.RFC3339Nano, strings.TrimSuffix(strings.TrimPrefix(needle, `"timestamp":"`), `"`))
			if err != nil {
				return nil, fmt.Errorf("reply time %s: %w", needle, err)
			}
			key := strconv.FormatInt(at.UnixMilli(), 10)
			if _, ok := stampNeedle[key]; !ok {
				stamps = append(stamps, transcriptdb.Time(at))
			}
			stampNeedle[key] = needle
		}
	}
	rows, err := d.client.Select(context.Background(), "reply_events",
		"SELECT toString(f.file_id), f.path, toString(toUnixTimestamp64Milli(e.ts)), toString(min(e.line)) "+
			"FROM events AS e INNER JOIN (SELECT file_id, path, ev_lines FROM files FINAL WHERE path IN {paths:Array(String)}) AS f USING (file_id) "+
			"WHERE e.file_id IN (SELECT file_id FROM files FINAL WHERE path IN {paths:Array(String)}) "+
			"AND e.ts IN {stamps:Array(DateTime64(3, 'UTC'))} AND e.line <= f.ev_lines GROUP BY f.file_id, f.path, e.ts",
		map[string]string{"paths": transcriptdb.Strings(paths), "stamps": transcriptdb.ArrayParam(stamps, transcriptdb.Quote)})
	if err != nil {
		return nil, err
	}
	var windows []replyWindow
	for _, r := range rows {
		if len(r) != 4 {
			return nil, fmt.Errorf("reply events: %d fields in row, want 4", len(r))
		}
		needle, ok := stampNeedle[r[2]]
		if _, want := needlesByPath[r[1]][needle]; !ok || !want {
			continue
		}
		last, err := strconv.Atoi(r[3])
		if err != nil {
			return nil, fmt.Errorf("reply events line: %w", err)
		}
		windows = append(windows, replyWindow{fileID: r[0], path: r[1], needle: needle, last: last})
	}
	return windows, nil
}

// firstNeedleLines — по сырым строкам окон: первая строка каждой иголки в каждом транскрипте. Параметры в условии
// по ключу сортировки raw (file_id, line) дают серверу прочитать только окна.
func (d *priorDB) firstNeedleLines(windows []replyWindow) (map[string]map[string]int, error) {
	params := map[string]string{}
	conds := make([]string, 0, len(windows))
	var needles []string
	seen := make(map[string]bool)
	fileOf := make(map[string]string)
	for i, w := range windows {
		fileOf[w.fileID] = w.path
		conds = append(conds, fmt.Sprintf("(file_id = {f%d:UInt64} AND line BETWEEN {a%d:UInt32} AND {b%d:UInt32})", i, i, i))
		params[fmt.Sprintf("f%d", i)] = w.fileID
		params[fmt.Sprintf("a%d", i)] = strconv.Itoa(w.first())
		params[fmt.Sprintf("b%d", i)] = strconv.Itoa(w.last)
		if !seen[w.needle] {
			seen[w.needle] = true
			needles = append(needles, w.needle)
		}
	}
	params["needles"] = transcriptdb.Strings(needles)
	rows, err := d.client.Select(context.Background(), "reply_lines",
		"SELECT toString(file_id), toString(line), arrayStringConcat(arrayMap(p -> toString(p), multiSearchAllPositions(raw, {needles:Array(String)})), ',') "+
			"FROM raw WHERE ("+strings.Join(conds, " OR ")+") AND multiSearchAny(raw, {needles:Array(String)})", params)
	if err != nil {
		return nil, err
	}
	first := make(map[string]map[string]int)
	for _, r := range rows {
		if len(r) != 3 {
			return nil, fmt.Errorf("reply lines: %d fields in row, want 3", len(r))
		}
		path, ok := fileOf[r[0]]
		if !ok {
			continue
		}
		line, err := strconv.Atoi(r[1])
		if err != nil {
			return nil, fmt.Errorf("reply lines line: %w", err)
		}
		if first[path] == nil {
			first[path] = make(map[string]int)
		}
		for i, pos := range strings.Split(r[2], ",") {
			if pos == "0" || i >= len(needles) {
				continue
			}
			if n := first[path][needles[i]]; n == 0 || line < n {
				first[path][needles[i]] = line
			}
		}
	}
	return first, nil
}
