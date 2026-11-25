package transcript

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aiseeq/akit/transcriptdb"
)

func TestReadReaderFixtures(t *testing.T) {
	fixtures := map[string]string{
		"claude": "{\"type\":\"user\",\"timestamp\":\"2026-10-25T10:00:00Z\",\"message\":{\"role\":\"user\",\"content\":\"" + strings.Repeat("x", 70<<10) + "\"}}\n\nnot-json\n{\"type\":\"assistant\",\"timestamp\":\"bad\",\"message\":{}}\n{\"type\":\"user\",\"timestamp\":\"2026-10-25T10:00:01Z\",\"message\":{\"role\":\"user\",\"content\":\"tail\"}}",
		"codex":  "{\"timestamp\":\"2026-10-25T10:00:00Z\",\"type\":\"session_meta\",\"payload\":{\"id\":\"c1\"}}\n{\"timestamp\":\"2026-10-25T10:00:01Z\",\"type\":\"event_msg\",\"payload\":{\"type\":\"user_message\",\"message\":\"hello\"}}\n\nnot-json\n{\"timestamp\":\"2026-10-25T10:00:02Z\",\"type\":\"event_msg\",\"payload\":{\"type\":\"agent_message\",\"message\":\"" + strings.Repeat("y", 70<<10) + "\"}}",
	}
	for name, input := range fixtures {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), name+".jsonl")
			if err := os.WriteFile(path, []byte(input), 0600); err != nil {
				t.Fatal(err)
			}
			variants := []struct {
				name   string
				file   func(func(Event), func(CodexCommandResult)) (Meta, error)
				reader func(io.Reader, func(Event), func(CodexCommandResult)) (Meta, error)
			}{
				{"Read", func(fn func(Event), _ func(CodexCommandResult)) (Meta, error) { return Read(path, nil, fn) }, func(r io.Reader, fn func(Event), _ func(CodexCommandResult)) (Meta, error) {
					return ReadReader(name, r, nil, fn)
				}},
				{"ReadWithTools", func(fn func(Event), _ func(CodexCommandResult)) (Meta, error) { return ReadWithTools(path, nil, fn) }, func(r io.Reader, fn func(Event), _ func(CodexCommandResult)) (Meta, error) {
					return ReadReaderWithTools(name, r, nil, fn)
				}},
			}
			if name == "codex" {
				variants = append(variants, struct {
					name   string
					file   func(func(Event), func(CodexCommandResult)) (Meta, error)
					reader func(io.Reader, func(Event), func(CodexCommandResult)) (Meta, error)
				}{"ReadWithCodexCommandsDiscardOutput", func(fn func(Event), commandFn func(CodexCommandResult)) (Meta, error) {
					return ReadWithCodexCommandsDiscardOutput(path, fn, commandFn)
				}, func(r io.Reader, fn func(Event), commandFn func(CodexCommandResult)) (Meta, error) {
					return ReadReaderWithCodexCommandsDiscardOutput(name, r, fn, commandFn)
				}})
			}
			for _, v := range variants {
				t.Run(v.name, func(t *testing.T) {
					var fileEvents, streamEvents []Event
					var fileCommands, streamCommands []CodexCommandResult
					fm, err := v.file(func(e Event) { fileEvents = append(fileEvents, e) }, func(c CodexCommandResult) { fileCommands = append(fileCommands, c) })
					if err != nil {
						t.Fatal(err)
					}
					rm, err := v.reader(onlyReader{strings.NewReader(input)}, func(e Event) { streamEvents = append(streamEvents, e) }, func(c CodexCommandResult) { streamCommands = append(streamCommands, c) })
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(fileEvents, streamEvents) || !reflect.DeepEqual(fm, rm) || !reflect.DeepEqual(fileCommands, streamCommands) {
						t.Fatalf("file/reader mismatch\nfile meta: %+v\nreader meta: %+v\nfile events: %d\nreader events: %d\nfile commands: %d\nreader commands: %d", fm, rm, len(fileEvents), len(streamEvents), len(fileCommands), len(streamCommands))
					}
				})
			}
		})
	}
	boom := errors.New("stream broke")
	brokenRecord := []byte(`{"type":"user","timestamp":"2026-10-25T10:00:00Z","message":{"role":"user","content":"` + strings.Repeat("z", 300) + `"}}` + "\n")
	seen := 0
	r, err := ReadReader("broken", &errorReader{data: brokenRecord, err: boom}, nil, func(Event) { seen++ })
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("stream error lost: meta=%+v err=%v", r, err)
	}
	if seen == 0 {
		t.Fatal("stream error happened before the buffered record was parsed")
	}
}

func BenchmarkReadLargeSynthetic(b *testing.B) {
	path := filepath.Join(b.TempDir(), "large.jsonl")
	line := `{"type":"user","timestamp":"2026-10-25T10:00:00Z","message":{"role":"user","content":"` + strings.Repeat("x", 4096) + `"}}` + "\n"
	data := strings.Repeat(line, 1024)
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for range b.N {
		if _, err := Read(path, nil, func(Event) {}); err != nil {
			b.Fatal(err)
		}
	}
}

type errorReader struct {
	data []byte
	err  error
}

type onlyReader struct{ io.Reader }

func (r *errorReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func TestTranscriptLiveReader(t *testing.T) {
	if os.Getenv("TRANSCRIPT_READER_LIVE") != "1" {
		t.Skip("set TRANSCRIPT_READER_LIVE=1 to compare live transcript sources")
	}
	list := os.Getenv("TRANSCRIPT_READER_LIVE_FILES")
	if list == "" {
		t.Fatal("TRANSCRIPT_READER_LIVE_FILES is required")
	}
	raw, err := os.ReadFile(list)
	if err != nil {
		t.Fatal(err)
	}
	paths := strings.Fields(string(raw))
	if len(paths) == 0 {
		t.Fatal("TRANSCRIPT_READER_LIVE_FILES contains no paths")
	}
	db, err := transcriptdb.NewFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("file unavailable: %v", err)
			}
			files, err := db.FilesByPath(context.Background(), path)
			if err != nil {
				t.Fatalf("database lookup: %v", err)
			}
			if len(files) != 1 {
				t.Fatalf("database row count=%d", len(files))
			}
			f := files[0]
			run := func(read func(func(Event)) (Meta, error)) ([]Event, Meta, error) {
				var ev []Event
				m, e := read(func(x Event) { ev = append(ev, x) })
				return ev, m, e
			}
			variants := []struct {
				name string
				read func(io.Reader, string, func(Event)) (Meta, error)
				file func(string, func(Event)) (Meta, error)
			}{
				{"Read", func(r io.Reader, n string, fn func(Event)) (Meta, error) { return ReadReader(n, r, nil, fn) }, func(p string, fn func(Event)) (Meta, error) { return Read(p, nil, fn) }},
				{"ReadWithTools", func(r io.Reader, n string, fn func(Event)) (Meta, error) { return ReadReaderWithTools(n, r, nil, fn) }, func(p string, fn func(Event)) (Meta, error) { return ReadWithTools(p, nil, fn) }},
			}
			if f.Kind == "codex" {
				variants = append(variants, struct {
					name string
					read func(io.Reader, string, func(Event)) (Meta, error)
					file func(string, func(Event)) (Meta, error)
				}{"ReadWithCodexCommandsDiscardOutput", func(r io.Reader, n string, fn func(Event)) (Meta, error) {
					return ReadReaderWithCodexCommandsDiscardOutput(n, r, fn, nil)
				}, func(p string, fn func(Event)) (Meta, error) { return ReadWithCodexCommandsDiscardOutput(p, fn, nil) }})
			}
			for _, v := range variants {
				t.Run(v.name, func(t *testing.T) {
					a, am, e := run(func(fn func(Event)) (Meta, error) { return v.file(path, fn) })
					if e != nil {
						t.Fatalf("file read: %v", e)
					}
					file, e := os.Open(path)
					if e != nil {
						t.Fatal(e)
					}
					b, bm, e := run(func(fn func(Event)) (Meta, error) { return v.read(file, path, fn) })
					_ = file.Close()
					if e != nil {
						t.Fatalf("reader read: %v", e)
					}
					if !reflect.DeepEqual(a, b) || !reflect.DeepEqual(am, bm) {
						t.Fatalf("file/reader mismatch: meta %+v / %+v, events %d / %d", am, bm, len(a), len(b))
					}
					if f.Ingested > uint64(info.Size()) {
						t.Fatalf("database ingested %d bytes, disk has %d", f.Ingested, info.Size())
					}
					disk, e := os.Open(path)
					if e != nil {
						t.Fatal(e)
					}
					trimmed := filepath.Join(t.TempDir(), "loaded.jsonl")
					out, e := os.Create(trimmed)
					if e != nil {
						t.Fatal(e)
					}
					copied, copyErr := io.CopyN(out, disk, int64(f.Ingested))
					_ = disk.Close()
					if copyErr != nil || uint64(copied) != f.Ingested {
						t.Fatalf("copy loaded file prefix: copied %d of %d bytes: %v", copied, f.Ingested, copyErr)
					}
					_ = out.Close()
					dbread, e := db.Open(context.Background(), f)
					if e != nil {
						t.Fatalf("database open: %v", e)
					}
					loaded := io.LimitReader(dbread, int64(f.Ingested))
					c, cm, e := run(func(fn func(Event)) (Meta, error) { return v.read(loaded, path, fn) })
					_ = dbread.Close()
					if e != nil {
						t.Fatalf("database stream read: %v", e)
					}
					d, dm, e := run(func(fn func(Event)) (Meta, error) { return v.file(trimmed, fn) })
					if e != nil {
						t.Fatalf("loaded prefix file read: %v", e)
					}
					if !reflect.DeepEqual(c, d) || !reflect.DeepEqual(cm, dm) {
						t.Fatalf("database/prefix mismatch: meta %+v / %+v, events %d / %d", cm, dm, len(c), len(d))
					}
				})
			}
		})
	}
}

var _ io.Reader = (*errorReader)(nil)
