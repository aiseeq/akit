package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"
)

type transferRecord struct {
	At     string   `json:"at"`
	Slug   string   `json:"slug"`
	Stem   string   `json:"stem"`
	Back   string   `json:"back"`
	Key    string   `json:"key"`
	URL    string   `json:"url"`
	Blocks []string `json:"blocks,omitempty"`
}

type transferResult struct {
	Key string `json:"key"`
	URL string `json:"url"`
}

func transferTask(root, dir string, o options, out, errout io.Writer) int {
	t, err := find(dir, o.id)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			rows, readErr := readTransferLedger(dir)
			if readErr != nil {
				return transferError(errout, filepath.Join(dir, ".transferred.jsonl"), readErr.Error())
			}
			stem := strings.TrimSuffix(o.id, ".md")
			for _, row := range rows {
				if row.Slug == o.id || row.Stem == stem {
					fmt.Fprintf(out, "%s уже перенесён: %s %s\n", row.Slug, row.Key, row.URL)
					return 0
				}
			}
		}
		return reportFindError(err, o.id, errout)
	}
	return transferFoundTask(root, t, o, out, errout)
}

func transferFoundTask(root string, t task, o options, out, errout io.Writer) int {
	dir := filepath.Dir(t.path)
	if t.status == "готово" || t.status == "отклонён" || isJournal(t) {
		return transferError(errout, t.path, "ticket status or kind does not allow transfer")
	}
	project := o.mainRoot
	if project == "" {
		project = o.baseRoot
	}
	if project == "" {
		project = root
	}
	commandPath, ok, err := taskCommandPath(project, ".tk-transfer")
	if err != nil {
		return transferError(errout, filepath.Join(filepath.Dir(project), ".tk-transfer"), err.Error())
	}
	if !ok {
		return transferError(errout, filepath.Join(filepath.Dir(project), ".tk-transfer"), "project parent is not a supported root")
	}
	if err := trustedSource(commandPath); err != nil {
		return transferError(errout, commandPath, err.Error())
	}
	stem := strings.TrimSuffix(filepath.Base(t.path), ".md")
	args := []string{filepath.Base(project), stem}
	if o.parent != "" {
		args = append(args, o.parent)
	}
	stdout, _, runErr := runBoundedCommand(commandPath, args, []byte(t.body), 30*time.Second)
	if runErr != nil {
		return transferError(errout, commandPath, runErr.Error())
	}
	result, err := parseTransferResult(stdout)
	if err != nil {
		return transferError(errout, commandPath, err.Error())
	}
	row := transferRecord{At: time.Now().Format(time.RFC3339), Slug: t.slug, Stem: stem, Back: lineValue(t.body, "Заказчик: "), Key: result.Key, URL: result.URL, Blocks: headerLinks(t.body, blockedPrefix)}
	if err := appendTransferRecord(dir, row); err != nil {
		fmt.Fprintf(out, "%s %s\n", result.Key, result.URL)
		return transferError(errout, filepath.Join(dir, ".transferred.jsonl"), err.Error())
	}
	if err := os.Remove(t.path); err != nil {
		fmt.Fprintf(out, "%s %s\n", result.Key, result.URL)
		return transferError(errout, t.path, err.Error())
	}
	fmt.Fprintf(out, "%s перенесён: %s %s\n", t.slug, result.Key, result.URL)
	return 0
}

func transferBlockRelations(t task, o options, key string, errout io.Writer) {
	for _, ref := range headerLinks(t.body, blockedPrefix) {
		dir, id, back, err := reverseTaskRef(t, ref)
		if err != nil {
			fmt.Fprintf(errout, "tk: warning: cannot update blocker %s after transfer: %v\n", ref, err)
			continue
		}
		msg := "заблокированный " + back + " перенесён в " + key
		if _, err = mutateTask(dir, id, func(current task) (string, error) {
			if !slices.Contains(headerLinks(current.body, blocksPrefix), back) {
				return current.body, nil
			}
			return journal(removeHeaderLink(current.body, blocksPrefix, back), "- "+timeStamp()+" note — "+o.by+": "+msg), nil
		}); err != nil {
			fmt.Fprintf(errout, "tk: warning: cannot update %s after transfer: %v\n", ref, err)
		}
	}
}

func parseTransferResult(data []byte) (transferResult, error) {
	var result transferResult
	d := json.NewDecoder(bytes.NewReader(data))
	if err := d.Decode(&result); err != nil {
		return result, fmt.Errorf("invalid transfer JSON: %w", err)
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		return result, errors.New("transfer output has trailing data")
	}
	if strings.TrimSpace(result.Key) == "" {
		return result, errors.New("transfer output is missing key")
	}
	if hasControl(result.Key) || hasControl(result.URL) {
		return result, errors.New("transfer output contains control characters")
	}
	return result, nil
}

func hasControl(s string) bool {
	return strings.ContainsFunc(s, unicode.IsControl)
}

func readTransferLedger(dir string) ([]transferRecord, error) {
	path := filepath.Join(dir, ".transferred.jsonl")
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read transfer ledger: %w", err)
	}
	var rows []transferRecord
	for line := range strings.SplitSeq(strings.TrimSuffix(string(content), "\n"), "\n") {
		if line == "" {
			continue
		}
		var row transferRecord
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, fmt.Errorf("invalid transfer ledger: %w", err)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func appendTransferRecord(dir string, row transferRecord) error {
	path := filepath.Join(dir, ".transferred.jsonl")
	rows, err := readTransferLedger(dir)
	if err != nil {
		return err
	}
	for _, old := range rows {
		if old.Stem == row.Stem {
			return nil
		}
	}
	data, err := json.Marshal(row)
	if err != nil {
		return fmt.Errorf("encode transfer record: %w", err)
	}
	data = append(data, '\n')
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, fileMode)
	if err != nil {
		return fmt.Errorf("open transfer ledger: %w", err)
	}
	n, writeErr := f.Write(data)
	if writeErr == nil && n != len(data) {
		writeErr = io.ErrShortWrite
	}
	closeErr := f.Close()
	return errors.Join(writeErr, closeErr)
}

func transferError(w io.Writer, path, reason string) int {
	fmt.Fprintf(w, "tk: transfer %s: %s\n", path, reason)
	return exitUsage
}
