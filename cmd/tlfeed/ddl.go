package main

import "strings"

// schema creates the tables of one database; every statement is idempotent, so it runs at every start.
// Layout (role, kind, file_id, line) with a time-ordered file id and granule 1024 is the one measured as the best
// on the corpus; raw keeps every line as is, so a file is rebuilt from it byte for byte.
//
//nolint:sql-runtime-ddl the loader owns its schema: idempotent CREATE ... IF NOT EXISTS run at every start
const schema = `
CREATE DATABASE IF NOT EXISTS {db};

CREATE TABLE IF NOT EXISTS {db}.files
(
    file_id UInt64,                       -- (unix seconds of the first event << 22) | low 22 bits of fnv64a(path)
    path String,
    kind LowCardinality(String),          -- claude | sub | codex
    project LowCardinality(String),       -- Claude project folder or worker:<task>
    session String,                       -- short id as tl prints it
    parent String,                        -- the parent session of a subagent
    name String,                          -- agent-name of the session
    size UInt64,                          -- file size at the last pass
    ingested UInt64,                      -- bytes up to the last complete line already in raw
    lines UInt32,                         -- complete lines already in raw
    mtime DateTime('UTC'),
    inode UInt64,
    first_ts DateTime64(3, 'UTC'),
    last_ts DateTime64(3, 'UTC'),
    bad UInt32,                           -- lines the parser could not read
    tail UInt64,                          -- first 8 bytes of sha256 of the last 256 ingested bytes
    gen UInt32,                           -- how many times the file was loaded again after a rewind
    ver UInt64                            -- write order: the latest row of a file wins
)
ENGINE = ReplacingMergeTree(ver)
ORDER BY file_id;

CREATE TABLE IF NOT EXISTS {db}.raw
(
    file_id UInt64,
    line UInt32 CODEC(Delta, ZSTD(1)),
    off UInt64 CODEC(Delta, ZSTD(1)),     -- byte offset of the line in the file
    raw String CODEC(ZSTD(3)),            -- the line without '\n'
    loaded_at DateTime64(3, 'UTC') DEFAULT now64(3) CODEC(Delta, ZSTD(1))
)
ENGINE = MergeTree
ORDER BY (file_id, line)
SETTINGS index_granularity = 1024, index_granularity_bytes = 1048576, non_replicated_deduplication_window = 10000;

CREATE TABLE IF NOT EXISTS {db}.events
(
    ts DateTime64(3, 'UTC') CODEC(Delta, ZSTD(1)),
    file_id UInt64,
    session String CODEC(ZSTD(1)),
    kind LowCardinality(String),
    project LowCardinality(String),
    line UInt32 CODEC(Delta, ZSTD(1)),
    role Enum8('user' = 1, 'ask' = 2, 'peer' = 3, 'agent' = 4, 'think' = 5, 'cmd' = 6, 'out' = 7, 'attach' = 8, 'note' = 9),
    who LowCardinality(String),
    text String CODEC(ZSTD(3)),
    images UInt16,
    error Bool,
    subtype LowCardinality(String),
    model LowCardinality(String),
    reply_id String CODEC(ZSTD(1)),
    request_id String CODEC(ZSTD(1)),
    in_tok UInt64,
    out_tok UInt64,
    cache_read UInt64,
    cache_write UInt64,
    cwd LowCardinality(String),
    head String MATERIALIZED substringUTF8(text, 1, 300) CODEC(ZSTD(3)),
    hay String MATERIALIZED lowerUTF8(concat(who, ' ', text)) CODEC(LZ4),
    INDEX hay_ng hay TYPE text(tokenizer = ngrams(3)),
    INDEX ts_mm ts TYPE minmax GRANULARITY 1
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(ts)
ORDER BY (role, kind, file_id, line)
SETTINGS index_granularity = 1024, index_granularity_bytes = 1048576, non_replicated_deduplication_window = 10000;

CREATE TABLE IF NOT EXISTS {db}.replies
(
    ts DateTime64(3, 'UTC'),
    file_id UInt64,
    session String,
    project LowCardinality(String),
    line UInt32,
    role Enum8('user' = 1, 'ask' = 2, 'peer' = 3, 'agent' = 4, 'think' = 5, 'cmd' = 6, 'out' = 7, 'attach' = 8, 'note' = 9),
    text String,
    hay String
)
ENGINE = MergeTree
ORDER BY ts
SETTINGS non_replicated_deduplication_window = 10000;

CREATE MATERIALIZED VIEW IF NOT EXISTS {db}.replies_mv TO {db}.replies AS
SELECT ts, file_id, session, project, line, role, text, lowerUTF8(text) AS hay
FROM {db}.events
WHERE role IN ('user', 'ask') AND kind = 'claude';

CREATE TABLE IF NOT EXISTS {db}.usage
(
    ts DateTime64(3, 'UTC'),
    session String,
    project LowCardinality(String),
    model LowCardinality(String),
    reply_id String,
    request_id String,
    in_tok UInt64,
    out_tok UInt64,
    cache_read UInt64,
    cache_write UInt64
)
ENGINE = ReplacingMergeTree(out_tok)
PARTITION BY toYYYYMM(ts)
ORDER BY (session, reply_id, request_id)
SETTINGS non_replicated_deduplication_window = 10000;

CREATE MATERIALIZED VIEW IF NOT EXISTS {db}.usage_mv TO {db}.usage AS
SELECT ts, session, project, model, reply_id, request_id, in_tok, out_tok, cache_read, cache_write
FROM {db}.events
WHERE reply_id != '';
`

// statements splits the schema of database db into single statements: the HTTP interface takes one per query.
func statements(db string) []string {
	var out []string
	for s := range strings.SplitSeq(strings.ReplaceAll(schema, "{db}", db), ";\n") {
		var kept []string
		for l := range strings.SplitSeq(s, "\n") {
			if code, _, _ := strings.Cut(l, "--"); strings.TrimSpace(code) != "" {
				kept = append(kept, strings.TrimRight(code, " "))
			}
		}
		if len(kept) > 0 {
			out = append(out, strings.Join(kept, "\n"))
		}
	}
	return out
}
