// webtext отдаёт агенту текст страницы, которую не открыл WebFetch: прямой запрос под честным именем, при отказе —
// последний снимок Wayback, текст из HTML или PDF — в файл. Маскировки нет: ни чужого UA, ни подделки отпечатка, ни
// прокси и посредников вроде Jina Reader.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const usage = `usage: webtext [--json] [--no-direct] [--out FILE] [--timeout 60s] URL

Text of a web page for an agent, when WebFetch could not open it. Steps:
direct GET under an honest user agent; on a refusal (403, 402, 429, 503, a
bot check page, no text) the latest Wayback snapshot with status 200. HTML and
PDF become text. Internal hosts and URLs with tokens never go to Wayback.

The text goes to a file (default ~/.cache/akit/webtext/<hash>.txt): line 1 the
title, line 2 "source: <where it came from>", a blank line, the text.

  --json        one JSON object instead of the summary line
  --no-direct   skip the direct GET, go to Wayback at once: the site already
                answered 429 (to WebFetch) and must not be asked again
  --out FILE    write the text here
  --timeout D   for all steps together (default 60s)

Exit: 0 text saved, 1 the page could not be opened (steps and a hint printed),
2 usage or write error.
`

const (
	exitClosed = 1
	exitUsage  = 2
)

// result — итог для агента и для хука: что открылось, откуда, где текст; при неудаче — ступени и подсказка.
type result struct {
	OK       bool   `json:"ok"`
	URL      string `json:"url"`
	Source   string `json:"source,omitempty"`   // direct или wayback
	Snapshot string `json:"snapshot,omitempty"` // метка снимка Wayback, ГГГГММДДччммсс
	Status   int    `json:"status,omitempty"`
	Kind     string `json:"kind,omitempty"` // html, pdf или text
	Title    string `json:"title,omitempty"`
	Chars    int    `json:"chars,omitempty"`
	Path     string `json:"path,omitempty"`
	Steps    []step `json:"steps"`
	Hint     string `json:"hint,omitempty"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	return runWith(newFetcher(), args, stdout, stderr)
}

func runWith(f *fetcher, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("webtext", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "")
	noDirect := fs.Bool("no-direct", false, "")
	out := fs.String("out", "", "")
	timeout := fs.Duration("timeout", time.Minute, "")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, usage)
			return 0
		}
		fmt.Fprintf(stderr, "webtext: %v (webtext -h for usage)\n", err)
		return exitUsage
	}
	if fs.NArg() != 1 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	target, err := parseTarget(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "webtext: %v (webtext -h for usage)\n", err)
		return exitUsage
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	res, pg := open(ctx, f, target, !*noDirect)
	if res.OK {
		path, err := save(*out, target, res, pg.text)
		if err != nil {
			fmt.Fprintf(stderr, "webtext: %v\n", err)
			return exitUsage
		}
		res.Path = path
	}
	if err := report(stdout, res, *asJSON); err != nil {
		fmt.Fprintf(stderr, "webtext: %v\n", err)
		return exitUsage
	}
	if !res.OK {
		return exitClosed
	}
	return 0
}

func parseTarget(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("bad URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("not an http(s) URL: %q", raw)
	}
	u.Fragment = ""
	return u, nil
}

// save пишет текст с заголовком и источником; без --out — в кэш под хэшем адреса.
func save(out string, target *url.URL, res result, text string) (string, error) {
	if out == "" {
		dir, err := os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("cache dir: %w", err)
		}
		sum := sha256.Sum256([]byte(target.String()))
		out = filepath.Join(dir, "akit", "webtext", hex.EncodeToString(sum[:8])+".txt")
	}
	out, err := filepath.Abs(out)
	if err != nil {
		return "", fmt.Errorf("text path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
		return "", fmt.Errorf("create dir: %w", err)
	}
	body := res.Title + "\nsource: " + sourceLine(target, res) + "\n\n" + text + "\n"
	if err := os.WriteFile(out, []byte(body), 0o600); err != nil {
		return "", fmt.Errorf("write text: %w", err)
	}
	return out, nil
}

func sourceLine(target *url.URL, res result) string {
	if res.Source == sourceWayback {
		return snapshotURL(res.Snapshot, target.String()) + " (Wayback snapshot " + snapshotDate(res.Snapshot) + ")"
	}
	return target.String()
}

// snapshotDate — метка снимка ГГГГММДДччммсс датой ДД.ММ.ГГГГ.
func snapshotDate(ts string) string {
	if len(ts) < len("20060102") {
		return ts
	}
	return ts[6:8] + "." + ts[4:6] + "." + ts[:4]
}

func report(w io.Writer, res result, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(res); err != nil {
			return fmt.Errorf("write json: %w", err)
		}
		return nil
	}
	var b strings.Builder
	if res.OK {
		from := res.Source
		if res.Snapshot != "" {
			from += " " + res.Snapshot
		}
		fmt.Fprintf(&b, "webtext: %s %s, %d chars «%s» → %s\n", from, res.Kind, res.Chars, res.Title, res.Path)
	} else {
		fmt.Fprintf(&b, "webtext: could not open %s\n", res.URL)
		for _, s := range res.Steps {
			fmt.Fprintf(&b, "  %s: %s\n", s.Step, s.Result)
		}
		fmt.Fprintf(&b, "next: %s\n", res.Hint)
	}
	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}
