// tlfeed — loads transcripts into ClickHouse as they are written: every complete line of every file tl --all
// --codex reads goes to raw as it is (a file is rebuilt from it byte for byte), its events as tl parses them to
// events, and how far each file is loaded to files. It runs until stopped: every second it loads the new complete
// lines; a line still being written waits. A server that is down is waited for. --once loads what there is and
// exits; --verify and --check compare the tables with the files.
//
// Exit codes: 0 — done or stopped; 1 — a check found a difference or a one-off command failed; 2 — a usage or
// setup error.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"syscall"
	"time"
)

const usage = `tlfeed — load transcripts into ClickHouse as they are written.

Usage:
  tlfeed [flags]                 follow: every second load the new complete lines (until SIGTERM)
  tlfeed --once [flags]          load everything there is, then exit
  tlfeed --materialize [flags]   build the text index of the events after a first load (--once)
  tlfeed --verify N [flags]      rebuild N random files from raw, compare sha256 with the files
  tlfeed --check [flags]         every transcript loaded to its last line, raw and events complete

Sources are the files tl --all --codex reads: Claude Code transcripts with subagents and the
rollouts of worker codex sessions under --home.

One loader per database: follow, --once and --materialize hold a lock (flock) on
tlfeed-<db>-<serverUUID>.lock beside the state file, or in $XDG_RUNTIME_DIR/akit/tlfeed without
one; a second loader exits at once with the pid of the holder. --verify and --check take no lock.

Flags:
  --url URL        ClickHouse HTTP interface (default http://127.0.0.1:18123)
  --db NAME        database (default tr); the tables are created if missing
  --home DIR       where the transcripts are (default $HOME)
  --state FILE     state file for a supervisor, rewritten every --beat; '' — none
                   (default $XDG_STATE_HOME/akit/tlfeed/state.json, ~/.local/state when unset)
  --tick D         pass period (default 1s)
  --idle D         a file that has not grown this long releases its parser (default 1h)
  --sweep D        every known file and every folder inotify does not watch is looked at once per D,
                   changed or not (default 5m); inotify watches the Claude projects and the codex
                   sessions of worker tasks written to within 48h, so a line added to a transcript
                   of an older task is loaded only by the sweep, up to D late
  --rescan D       full listing of the transcripts (default 1h)
  --beat D         state file period, at most 60s (default 10s)
  --no-inotify     find changes by polling only
  -h, --help       this help

State file fields: heartbeat, last_row_at (when a line was last inserted, by the loader's clock),
lag_bytes (complete lines not yet confirmed by the server), files, error (last error or empty), pid.
`

var dbName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type options struct {
	cfg         config
	url         string
	beat        time.Duration
	materialize bool
	verify      int
	check       bool
}

func parse(args []string) (options, error) {
	home, homeErr := os.UserHomeDir()
	var o options
	fs := flag.NewFlagSet("tlfeed", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.url, "url", "http://127.0.0.1:18123", "")
	fs.StringVar(&o.cfg.db, "db", "tr", "")
	fs.StringVar(&o.cfg.home, "home", home, "")
	fs.StringVar(&o.cfg.state, "state", "", "")
	fs.BoolVar(&o.cfg.once, "once", false, "")
	fs.BoolVar(&o.materialize, "materialize", false, "")
	fs.IntVar(&o.verify, "verify", 0, "")
	fs.BoolVar(&o.check, "check", false, "")
	fs.DurationVar(&o.cfg.tick, "tick", time.Second, "")
	fs.DurationVar(&o.cfg.idle, "idle", time.Hour, "")
	fs.DurationVar(&o.cfg.sweep, "sweep", 5*time.Minute, "")
	fs.DurationVar(&o.cfg.rescan, "rescan", time.Hour, "")
	fs.DurationVar(&o.beat, "beat", 10*time.Second, "")
	noInotify := fs.Bool("no-inotify", false, "")
	if err := fs.Parse(args); err != nil {
		return o, fmt.Errorf("arguments: %w", err)
	}
	o.cfg.inotify = !*noInotify
	o.cfg.batch = 32 << 20
	switch {
	case fs.NArg() > 0:
		return o, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	case !dbName.MatchString(o.cfg.db):
		return o, fmt.Errorf("bad --db %q: letters, digits and _", o.cfg.db)
	case o.cfg.home == "":
		return o, fmt.Errorf("no --home and no home folder: %w", homeErr)
	case o.cfg.tick <= 0 || o.cfg.idle <= 0 || o.cfg.sweep <= 0 || o.cfg.rescan <= 0:
		return o, fmt.Errorf("--tick, --idle, --sweep and --rescan must be above 0")
	case o.beat <= 0 || o.beat > time.Minute:
		return o, fmt.Errorf("--beat must be above 0 and at most 60s")
	case o.verify < 0:
		return o, fmt.Errorf("--verify N: N must be above 0")
	}
	modes := 0
	for _, on := range []bool{o.cfg.once, o.materialize, o.verify > 0, o.check} {
		if on {
			modes++
		}
	}
	if modes > 1 {
		return o, fmt.Errorf("--once, --materialize, --verify and --check do not go together")
	}
	o.cfg.home = filepath.Clean(o.cfg.home)
	stateGiven := false
	fs.Visit(func(f *flag.Flag) { stateGiven = stateGiven || f.Name == "state" })
	if !stateGiven { // --state '' keeps no state file
		o.cfg.state = defaultStatePath(o.cfg.home)
	}
	o.cfg.lock = lockDir(o.cfg.state)
	return o, nil
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "-help" {
			fmt.Fprint(stdout, usage)
			return 0
		}
	}
	o, err := parse(args)
	if err != nil {
		fmt.Fprintf(stderr, "tlfeed: %v (tlfeed -h)\n", err)
		return 2
	}
	srv, err := newServer(o.url)
	if err != nil {
		fmt.Fprintf(stderr, "tlfeed: %v\n", err)
		return 2
	}
	c := &checker{srv: srv, db: o.cfg.db, home: o.cfg.home, out: stdout}
	switch {
	case o.materialize:
		id, err := serverID(context.Background(), srv)
		if err != nil {
			return result(err, false, stderr)
		}
		held, err := takeLock(lockPath(o.cfg.lock, id, o.cfg.db))
		if err != nil {
			return result(err, false, stderr)
		}
		defer held.Close()
		return result(c.materialize(), true, stderr)
	case o.verify > 0:
		ok, err := c.verify(o.verify)
		return result(err, ok, stderr)
	case o.check:
		ok, err := c.check()
		return result(err, ok, stderr)
	}
	return load(o, srv, stderr)
}

func result(err error, ok bool, stderr io.Writer) int {
	if err != nil {
		fmt.Fprintf(stderr, "tlfeed: %v\n", err)
		return 1
	}
	if !ok {
		return 1
	}
	return 0
}

// load runs the loader (follow or --once) with the state file and signal handling.
func load(o options, srv *server, stderr io.Writer) int {
	if o.cfg.state != "" {
		if err := os.MkdirAll(filepath.Dir(o.cfg.state), 0o700); err != nil {
			fmt.Fprintf(stderr, "tlfeed: state folder: %v\n", err)
			return 2
		}
	}
	stop := make(chan struct{})
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigs)
	go func() {
		<-sigs
		close(stop)
	}()
	st := newStatus(o.cfg.state)
	l := newLoader(o.cfg, srv, stop, stderr, st)
	l.dirs.w = noWatch{}
	if o.cfg.inotify && !o.cfg.once {
		if w, err := newWatcher(l.logf); err == nil {
			l.dirs.w = w
		} else {
			l.logf("inotify: %v; polling only", err)
		}
	}
	defer l.dirs.w.close()
	finished, beatDone := make(chan struct{}), make(chan struct{})
	if err := st.write(time.Now()); err != nil {
		fmt.Fprintf(stderr, "tlfeed: %v\n", err)
		return 2
	}
	go st.beat(o.beat, finished, beatDone, time.Now, l.logf)
	start := time.Now()
	var err error
	if o.cfg.once {
		err = l.once()
	} else {
		err = l.follow()
	}
	close(finished)
	<-beatDone
	if o.cfg.once && err == nil {
		lag, n := l.lag()
		l.logf("once: %d files, %d lines (%d bytes) and %d events inserted in %s; lag %d bytes; next: tlfeed --materialize",
			n, l.rawRows, l.rawBytes, l.evRows, time.Since(start).Round(time.Second), lag)
	}
	if err != nil && !errors.Is(err, errStopped) {
		l.logf("%v", err) // only the setup can fail here: the server is waited for
		return 2
	}
	return 0
}
