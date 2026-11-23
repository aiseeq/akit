// Package main provides page-upd, which marks changed direct children of an HTML column.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

var errHelp = errors.New("help")

const usage = `Mark changed direct children of an HTML column with data-upd.

Usage: page-upd [--column SELECTOR] [--artifacts DIR/ID] <page.html> [snapshot.html]
The column defaults to main; SELECTOR supports a tag, #id or .class. Without snapshot.html, --artifacts DIR/ID
is required; the lexically latest .html file under that directory is used.
`

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, time.Now)) }

func run(args []string, stdout, stderr io.Writer, now func() time.Time) int {
	code, err := execute(args, now)
	if err != nil {
		if errors.Is(err, errHelp) {
			fmt.Fprint(stdout, usage)
			return 0
		}
		fmt.Fprintf(stderr, "page-upd: %v\n%s", err, usage)
		return 1
	}
	fmt.Fprintln(stdout, code)
	return 0
}
