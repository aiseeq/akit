package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/aiseeq/akit/transcript"
)

// readTail gives the events of the lines past evLines of a file, each with its line as written: the lines the
// loader has not stored yet. The lines before are only followed for the state the parse needs (tool names by
// call id, the mode of a codex rollout), as the loader does. whole — the stored events of the file are not what
// a read of it gives now (a rollout began to write message events after them): fn got all events of the file, and
// the caller drops the stored ones.
func readTail(path string, evLines int, fn func(raw []byte, ev transcript.Event)) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("open transcript: %w", err)
	}
	defer f.Close()
	fl := transcript.NewFollow()
	whole := false
	if isCodexRollout(path) {
		var before [2]bool
		n := 0
		if err := eachLine(f, func(line []byte) error {
			if err := fl.Prescan(line); err != nil {
				return fmt.Errorf("prescan: %w", err)
			}
			if n++; n == evLines {
				before[0], before[1] = fl.MessageEvents()
			}
			return nil
		}); err != nil {
			return false, err
		}
		if user, agent := fl.MessageEvents(); evLines > 0 && before != [2]bool{user, agent} {
			whole = true
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return false, fmt.Errorf("read transcript again: %w", err)
		}
	}
	if !whole {
		if err := fl.SkipThrough(evLines); err != nil {
			return false, fmt.Errorf("skip stored lines: %w", err)
		}
	}
	err = eachLine(f, func(line []byte) error {
		if err := fl.Feed(line, func(ev transcript.Event) { fn(bytes.TrimRight(line, "\r\n"), ev) }); err != nil {
			return fmt.Errorf("parse: %w", err)
		}
		return nil
	})
	return whole, err
}

// eachLine gives the complete lines of r with their '\n'; an incomplete last line is being written and is left.
func eachLine(r io.Reader, fn func(line []byte) error) error {
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			if ferr := fn(line); ferr != nil {
				return ferr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read transcript: %w", err)
		}
	}
}
