package main

import (
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/akit/transcript"
)

const (
	eventRetentionDays    = int(transcript.EventRetention / (24 * time.Hour))
	eventUsageRebuildDays = eventRetentionDays - 1
)

// retainedEventKinds is the SQL form of transcript.EventKeptForever.
var retainedEventKinds = "kind IN (" + sqlList(transcript.KeptForeverKinds) + ") AND role IN (" +
	sqlList(transcript.KeptForeverRoles) + ")"

func sqlList[T ~string](values []T) string {
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = "'" + string(v) + "'"
	}
	return strings.Join(quoted, ", ")
}

func eventTTL() string {
	return "toDateTime(ts) + INTERVAL " + strconv.Itoa(eventRetentionDays) + " DAY DELETE WHERE NOT (" + retainedEventKinds + ")"
}

func eventRetained(at time.Time, kind, role string, now time.Time) bool {
	return at.Truncate(time.Second).Add(time.Duration(eventRetentionDays)*24*time.Hour).After(now) || transcript.EventKeptForever(kind, transcript.Role(role))
}

func eventRetentionPredicate(column string) string {
	return "(" + eventWithinRetention(column) + " OR (" + retainedEventKinds + "))"
}

func eventWithinRetention(column string) string {
	return "toDateTime(" + column + ") + INTERVAL " + strconv.Itoa(eventRetentionDays) + " DAY > now()"
}

func eventRetentionPredicateAt(column string, at time.Time) string {
	return strings.Replace(eventRetentionPredicate(column), eventWithinRetention(column), eventWithinRetentionAt(column, at, eventRetentionDays), 1)
}

func eventWithinRetentionAt(column string, at time.Time, days int) string {
	return "toDateTime(" + column + ") + INTERVAL " + strconv.Itoa(days) + " DAY > toDateTime('" + at.UTC().Format("2006-01-02 15:04:05") + "', 'UTC')"
}

// schema creates the tables of one database; every statement is idempotent, so it runs at every start. TTL is
// applied when events is created; an existing table keeps its current TTL until explicitly altered.
// Layout (role, kind, file_id, line) with a time-ordered file id and granule 1024 is the one measured as the best
// on the corpus; raw keeps every line as is, so a file is rebuilt from it byte for byte.
// files is rewritten for the newest file ids on every pass, so its newest part takes a merge per insert; the merge cap
// bounds that part: once it passes 512 KiB it stays as is and a new small one starts. Old versions left in capped
// parts are a few dozen rows, FINAL drops them.
//
//nolint:sql-runtime-ddl the loader owns its schema: idempotent CREATE ... IF NOT EXISTS run at every start
const schema = `
CREATE DATABASE IF NOT EXISTS {db};

CREATE TABLE IF NOT EXISTS {db}.files
(
    file_id UInt64,                       -- (unix seconds of the first event << 22) | low 22 bits of fnv64a(path)
    path String,
    kind LowCardinality(String),          -- claude | sub | codex | codex-ext
    project LowCardinality(String),       -- Claude project folder, worker:<task>, or the cwd of codex-ext as one
    session String,                       -- short id as tl prints it
    parent String,                        -- the parent session of a subagent
    name String,                          -- agent-name of the session
    description String,                   -- description from the subagent metadata file
    cwd String,                            -- working directory from a codex rollout header
    size UInt64,                          -- file size at the last pass
    ingested UInt64,                      -- bytes up to the last complete line already in raw
    lines UInt32,                         -- complete lines already in raw
    ev_lines UInt32,                      -- lines whose events are already in events
    mtime DateTime64(3, 'UTC'),
    inode UInt64,
    first_ts DateTime64(3, 'UTC'),
    last_ts DateTime64(3, 'UTC'),
    bad UInt32,                           -- lines the parser could not read
    tail UInt64,                          -- first 8 bytes of sha256 of the last 256 ingested bytes
    gen UInt64,                           -- generation: new at every delete of rows to load them again, never the same for two loaders
    dirty UInt8,                          -- 1: rows past lines and ev_lines may be in raw and events; size is 0 then
    ver UInt64                            -- write order: the latest row of a file wins
)
ENGINE = ReplacingMergeTree(ver)
ORDER BY file_id
SETTINGS max_bytes_to_merge_at_max_space_in_pool = 524288;

ALTER TABLE {db}.files MODIFY COLUMN gen UInt64;
ALTER TABLE {db}.files MODIFY COLUMN mtime DateTime64(3, 'UTC');
ALTER TABLE {db}.files ADD COLUMN IF NOT EXISTS description String AFTER name;
ALTER TABLE {db}.files ADD COLUMN IF NOT EXISTS cwd String AFTER description;
ALTER TABLE {db}.files MODIFY SETTING max_bytes_to_merge_at_max_space_in_pool = 524288;

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
    seq UInt16 DEFAULT 0,
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
    cache_write_5m UInt64 DEFAULT 0,
    cache_write_1h UInt64 DEFAULT 0,
    fast Bool DEFAULT false,
    geo LowCardinality(String) DEFAULT '',
    cwd LowCardinality(String),
    usage_session String DEFAULT '',
    -- characters of transcript.Short(text, 0); 0 on rows loaded before the column, until they are recounted
    flat_len UInt32 DEFAULT 0,
    head String MATERIALIZED substringUTF8(text, 1, 300) CODEC(ZSTD(3)),
    hay String MATERIALIZED lowerUTF8(concat(who, ' ', text)) CODEC(LZ4),
    INDEX hay_ng hay TYPE text(tokenizer = ngrams(3)),
    INDEX ts_mm ts TYPE minmax GRANULARITY 1
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(ts)
ORDER BY (role, kind, file_id, line)
TTL {event_ttl}
-- 128 KiB granules: a search over two days reads half as many bytes of text as with 1 MiB granules
SETTINGS index_granularity = 1024, index_granularity_bytes = 131072, non_replicated_deduplication_window = 10000, merge_with_ttl_timeout = 604800;

ALTER TABLE {db}.events ADD COLUMN IF NOT EXISTS cache_write_5m UInt64 DEFAULT 0 AFTER cache_write;
ALTER TABLE {db}.events ADD COLUMN IF NOT EXISTS seq UInt16 DEFAULT 0 AFTER line;
ALTER TABLE {db}.events ADD COLUMN IF NOT EXISTS cache_write_1h UInt64 DEFAULT 0 AFTER cache_write_5m;
ALTER TABLE {db}.events ADD COLUMN IF NOT EXISTS fast Bool DEFAULT false AFTER cache_write_1h;
ALTER TABLE {db}.events ADD COLUMN IF NOT EXISTS geo LowCardinality(String) DEFAULT '' AFTER fast;
ALTER TABLE {db}.events ADD COLUMN IF NOT EXISTS usage_session String DEFAULT '' AFTER cwd;
ALTER TABLE {db}.events ADD COLUMN IF NOT EXISTS flat_len UInt32 DEFAULT 0 AFTER usage_session;

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
    file_id UInt64,
    ts DateTime64(3, 'UTC'),
    session String,
    project LowCardinality(String),
    model LowCardinality(String),
    reply_id String,
    request_id String,
    in_tok UInt64,
    out_tok UInt64,
    cache_read UInt64,
    cache_write UInt64,
    cache_write_5m UInt64,
    cache_write_1h UInt64,
    fast Bool,
    geo LowCardinality(String)
)
ENGINE = ReplacingMergeTree(out_tok)
PARTITION BY toYYYYMM(ts)
ORDER BY (session, reply_id, request_id)
SETTINGS non_replicated_deduplication_window = 10000, max_bytes_to_merge_at_max_space_in_pool = 524288;

ALTER TABLE {db}.usage ADD COLUMN IF NOT EXISTS cache_write_5m UInt64 DEFAULT 0 AFTER cache_write;
ALTER TABLE {db}.usage ADD COLUMN IF NOT EXISTS cache_write_1h UInt64 DEFAULT 0 AFTER cache_write_5m;
ALTER TABLE {db}.usage ADD COLUMN IF NOT EXISTS fast Bool DEFAULT false AFTER cache_write_1h;
ALTER TABLE {db}.usage ADD COLUMN IF NOT EXISTS geo LowCardinality(String) DEFAULT '' AFTER fast;
ALTER TABLE {db}.usage MODIFY SETTING max_bytes_to_merge_at_max_space_in_pool = 524288;

-- Refresh the view definition so new event fields and Codex token counts reach usage.
CREATE MATERIALIZED VIEW IF NOT EXISTS {db}.usage_mv TO {db}.usage AS
SELECT file_id, ts, usage_session AS session, project, model, reply_id, request_id, in_tok, out_tok, cache_read, cache_write, cache_write_5m, cache_write_1h, fast, geo
FROM {db}.events
WHERE reply_id != '' OR request_id != '';
ALTER TABLE {db}.usage_mv MODIFY QUERY
SELECT file_id, ts, usage_session AS session, project, model, reply_id, request_id, in_tok, out_tok, cache_read, cache_write, cache_write_5m, cache_write_1h, fast, geo
FROM {db}.events
WHERE reply_id != '' OR request_id != '';
`

// statements splits the schema of database db into single statements: the HTTP interface takes one per query.
func statements(db string) []string {
	var out []string
	ddl := strings.NewReplacer("{db}", db, "{event_ttl}", eventTTL()).Replace(schema)
	for s := range strings.SplitSeq(ddl, ";\n") {
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
