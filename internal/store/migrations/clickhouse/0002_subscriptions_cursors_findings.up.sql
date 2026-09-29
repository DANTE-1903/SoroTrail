-- Migration 0002 fills the tables the second half of the Store interface
-- needs: subscriptions and their delivery history, the dead-letter queue,
-- per-contract ingest cursors, and audit findings. Each table's engine is
-- chosen to fit how the row is actually written, not to mirror Postgres's
-- ON CONFLICT: ClickHouse has no row-level upsert, so anything that is
-- updated in place becomes a ReplacingMergeTree the writer appends a new
-- version to and reads back with FINAL.

-- subscriptions is the webhook-registration table. Rows are created,
-- edited (url/filters/secret/enabled), and stamped by the failure
-- counter, so every mutation is an append of a new version keyed by id.
-- ReplacingMergeTree(updated_at) ORDER BY id means a reader using FINAL
-- sees the newest version of each subscription without waiting for a
-- background merge to collapse the older ones.
--
-- id is assigned by the writer (a nanosecond timestamp) rather than a
-- sequence: ClickHouse's HTTP interface has no "RETURNING id", and a
-- MergeTree has no auto-increment. tenant_id is Nullable to match the
-- Postgres column, where a NULL means an operator-owned subscription.
CREATE TABLE IF NOT EXISTS subscriptions
(
    id            UInt64,
    url           String,
    filters       String DEFAULT '{}',
    secret        String DEFAULT '',
    enabled       UInt8 DEFAULT 1,
    failure_count UInt32 DEFAULT 0,
    tenant_id     Nullable(Int64),
    created_at    DateTime64(3) DEFAULT now64(3),
    updated_at    DateTime64(3) DEFAULT now64(3)
)
ENGINE = ReplacingMergeTree(updated_at)
ORDER BY id;

-- delivery_attempts is an append-only log: one row per POST to a
-- subscriber, written once and never edited. Plain MergeTree is the
-- right engine, and partitioning by month keeps the retention window
-- cheap to drop. The ordering key puts a subscription's history
-- together and newest-last, so ListDeliveryAttempts' "ORDER BY
-- created_at DESC" is a backward scan of one contiguous range.
CREATE TABLE IF NOT EXISTS delivery_attempts
(
    id              UInt64,
    subscription_id UInt64,
    event_id        String,
    status          LowCardinality(String),
    response_code   Int32 DEFAULT 0,
    duration_ms     Int64 DEFAULT 0,
    error           String DEFAULT '',
    created_at      DateTime64(3) DEFAULT now64(3)
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(created_at)
ORDER BY (subscription_id, created_at, id);

-- dead_letters is a working queue: the same event can be dead-lettered
-- more than once as the ingester retries, and a retry must increment the
-- attempt counter rather than create a second row. Postgres gets that
-- from UNIQUE(event_id) plus ON CONFLICT. Here the natural key is
-- event_id and ReplacingMergeTree(last_attempt) ORDER BY event_id keeps
-- the newest attempt per event. The row id is stable across retries
-- because the writer reads the existing id (FINAL) before inserting the
-- incremented version, so it can still be addressed by id for GET and
-- DELETE just as on Postgres.
--
-- Attempts and last_attempt are the only fields a retry updates. The
-- raw payload is overwritten with the newest attempt, matching
-- Postgres's ON CONFLICT DO UPDATE.
CREATE TABLE IF NOT EXISTS dead_letters
(
    id           UInt64,
    event_id     String,
    contract_id  String,
    ledger       Int64,
    type         LowCardinality(String),
    tx_hash      String DEFAULT '',
    topic_xdr    Array(String) DEFAULT [],
    value_xdr    String DEFAULT '',
    error        String DEFAULT '',
    attempts     UInt32 DEFAULT 1,
    last_attempt DateTime64(3) DEFAULT now64(3),
    created_at   DateTime64(3) DEFAULT now64(3)
)
ENGINE = ReplacingMergeTree(last_attempt)
ORDER BY event_id;

-- contract_cursors is one resume position per watched contract, upserted
-- on every ingest cycle. ReplacingMergeTree(updated_at) ORDER BY
-- contract_id is the same "append a version, read with FINAL" pattern as
-- subscriptions, chosen over CollapsingMergeTree because the reader
-- wants the latest row and never needs the history.
CREATE TABLE IF NOT EXISTS contract_cursors
(
    contract_id          String,
    last_ingested_ledger Int64 DEFAULT 0,
    last_cursor          String DEFAULT '',
    updated_at           DateTime64(3) DEFAULT now64(3)
)
ENGINE = ReplacingMergeTree(updated_at)
ORDER BY contract_id;

-- audit_findings is a small, mutable work queue: a finding is created,
-- then repeatedly updated with its status, attempt count and last error
-- as the auditor retries. Keyed by the writer-assigned id, versioned by
-- updated_at, so an update appends a new version and FINAL returns the
-- current state. updated_at is separate from created_at so an update
-- never rewrites (and so never loses) the original detection time.
CREATE TABLE IF NOT EXISTS audit_findings
(
    id                UInt64,
    network           LowCardinality(String) DEFAULT 'default',
    from_ledger       Int64 DEFAULT 0,
    to_ledger         Int64 DEFAULT 0,
    expected_count    UInt32 DEFAULT 0,
    actual_count      UInt32 DEFAULT 0,
    missing_ids       Array(String) DEFAULT [],
    status            LowCardinality(String) DEFAULT 'open',
    attempts          UInt32 DEFAULT 0,
    last_attempted_at Nullable(DateTime64(3)) DEFAULT NULL,
    last_error        String DEFAULT '',
    created_at        DateTime64(3) DEFAULT now64(3),
    updated_at        DateTime64(3) DEFAULT now64(3)
)
ENGINE = ReplacingMergeTree(updated_at)
ORDER BY id;
