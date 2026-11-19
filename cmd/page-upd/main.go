package main

import (
	"fmt"
	"io"
	"os"
	"time"
)

const usage = `usage: page-upd [--column SELECTOR] [--artifacts DIR/ID] <page.html> [snapshot.html]
Mark changed direct children of the selected column (default: main) with data-upd.
SELECTOR supports a tag, #id or .class. Without snapshot.html, --artifacts DIR/ID
is required; the lexically latest .html file under that directory is used.
`

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, time.Now)) }

func run(args []string, stdout, stderr io.Writer, now func() time.Time) int {
	code, err := execute(args, now)
	if err != nil {
		fmt.Fprintf(stderr, "page-upd: %v\n%s", err, usage)
		return 1
	}
	fmt.Fprintln(stdout, code)
	return 0
}
