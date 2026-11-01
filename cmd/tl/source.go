package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/akit/session"
	"github.com/aiseeq/akit/transcript"
)

type source = transcript.Source

// sinceDefault — окно --all без --since: за неделю транскриптов около 2 ГБ, за всё время — больше 10.
const sinceDefault = 7 * 24 * time.Hour

func resolve(opts options, e env) ([]source, error) {
	if opts.all {
		if len(opts.args) > 0 {
			return nil, errors.New("--all and SESSION together; pick one")
		}
		since := opts.since
		if since.IsZero() {
			since = e.now.Add(-sinceDefault)
		}
		return transcript.AllSources(e.home, since, opts.project, opts.codex, !opts.noSubagents)
	}
	args := opts.args
	if len(args) == 0 {
		if e.sessionID == "" {
			return nil, errors.New("no SESSION and not inside a Claude Code session; name one or use --all")
		}
		args = []string{e.sessionID}
	}
	var out []source
	for _, a := range args {
		path, err := find(a, e.home)
		if err != nil {
			return nil, err
		}
		src, err := transcript.SourceAt(path)
		if err != nil {
			return nil, err
		}
		out = append(out, src)
		if opts.subagents {
			subs, err := transcript.SubSources(path)
			if err != nil {
				return nil, err
			}
			out = append(out, subs...)
		}
	}
	return out, nil
}

// find — файл транскрипта по пути, имени живой сессии или id (префиксу id).
func find(arg, home string) (string, error) {
	if strings.HasSuffix(arg, ".jsonl") || strings.Contains(arg, "/") {
		if _, err := os.Stat(arg); err != nil {
			return "", fmt.Errorf("transcript %s: %w", arg, err)
		}
		return arg, nil
	}
	projects := session.ProjectsDir(home)
	// Реестр читается частично, если запись пишется прямо сейчас: его ошибка нужна, только если имя не нашлось.
	alive, aliveErr := session.ForHome(home).Alive()
	if in, ok := alive[arg]; ok {
		t, err := session.Describe(projects, in.SessionID)
		if err != nil {
			return "", fmt.Errorf("session %s: %w", arg, err)
		}
		return t.Path, nil
	}
	if len(arg) < 6 || strings.ContainsAny(arg, "*?[") {
		return "", fmt.Errorf("%q: not a live session name, and too short for a session id (6+ chars)", arg)
	}
	matches, err := filepath.Glob(filepath.Join(projects, "*", arg+"*.jsonl"))
	if err != nil {
		return "", fmt.Errorf("find %s: %w", arg, err)
	}
	codex, err := filepath.Glob(filepath.Join(transcript.WorkerSessions(home), "*", "*", "*", "rollout-*"+arg+"*.jsonl"))
	if err != nil {
		return "", fmt.Errorf("find %s: %w", arg, err)
	}
	matches = append(matches, codex...)
	if len(matches) == 0 {
		subs, err := filepath.Glob(filepath.Join(projects, "*", "*", "subagents", "agent-"+arg+"*.jsonl"))
		if err != nil {
			return "", fmt.Errorf("find %s: %w", arg, err)
		}
		matches = append(matches, subs...)
	}
	switch len(matches) {
	case 0:
		if aliveErr != nil {
			return "", fmt.Errorf("%q: no transcript with this id; live sessions read with error: %w", arg, aliveErr)
		}
		return "", fmt.Errorf("%q: no live session with this name and no transcript with this id", arg)
	case 1:
		return matches[0], nil
	}
	return "", fmt.Errorf("%q matches %d transcripts (%s, …): give more of the id", arg, len(matches), matches[0])
}

var refArg = regexp.MustCompile(`^(.+):([1-9][0-9]{0,8})$`)

// splitRef узнаёт единственный аргумент вида «id:строка»; не он — пустой ref.
func splitRef(args []string) (string, int, error) {
	if len(args) != 1 {
		return "", 0, nil
	}
	m := refArg.FindStringSubmatch(args[0])
	if m == nil {
		return "", 0, nil
	}
	line, err := strconv.Atoi(m[2])
	if err != nil {
		return "", 0, fmt.Errorf("ref %s: %w", args[0], err)
	}
	return m[1], line, nil
}
