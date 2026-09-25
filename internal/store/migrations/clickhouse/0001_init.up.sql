-- schema_migrations tracks which numbered files under this directory have
-- been applied. ClickHouse has no transactional DDL and no golang-migrate
-- driver here, so the migration runner (see clickhouse_migrate.go) applies
-- each file's statements in order and then records the version itself,
-- rather than relying on a driver to do both atomically. ReplacingMergeTree
-- keyed on version means a race between two instances applying the same
-- version at startup converges to one row instead of duplicating it.
CREATE TABLE IF NOT EXISTS schema_migrations
(
    version    UInt32,
    applied_at DateTime64(3) DEFAULT now64(3)
)
ENGINE = ReplacingMergeTree(applied_at)
ORDER BY version;

-- events mirrors the Postgres `events` table (internal/store/store.go Event)
-- for analytical / high-volume querying. Columns line up 1:1 with the Event
-- struct so scans can be read straight into it.
--
-- Engine: plain MergeTree, not ReplacingMergeTree keyed on id. Event IDs
-- are TOID-based and immutable once ingested; ReplaceEventsInRange already
-- deletes-then-reinserts a ledger range explicitly (see #586), so there is
-- no need to pay ReplacingMergeTree's background-merge deduplication cost
-- (which is also only eventually consistent, and QueryEvents/CountEvents
-- need exact answers without FINAL).
--
-- Ordering key: (network, contract_id, ledger, id). QueryEvents' hottest
-- filters are network + contract_id (almost every call site scopes to one
-- network, and the single-contract lookup is the common case) followed by
-- a ledger range (pagination and time-window queries both walk ledger
-- order); id is appended last purely to make the key unique per row, which
-- ClickHouse needs for predictable merge behaviour. Queries that filter by
-- tx_hash or type alone (GetEventsByTxHash, type-only filters) fall back to
-- the skip indexes below rather than the primary key.
--
-- Partitioning by month of created_at (ingestion time, not ledger time)
-- keeps partitions small and append-mostly, and makes the retention delete
-- in #586 cheap: dropping/pruning whole partitions is far less work for
-- ClickHouse than row-level DELETE across the whole table.
CREATE TABLE IF NOT EXISTS events
(
    id                 String,
    network            LowCardinality(String) DEFAULT 'default',
    contract_id        String,
    ledger             Int64,
    type               LowCardinality(String),
    tx_hash            String,
    tx_index           Int32 DEFAULT 0,
    op_index           Int32 DEFAULT 0,
    in_successful_call UInt8 DEFAULT 1,
    topics             String DEFAULT '[]',
    value              Nullable(String),
    raw_topic_xdr      Array(String) DEFAULT [],
    raw_value_xdr      String DEFAULT '',
    created_at         DateTime64(3) DEFAULT now64(3),

    -- Skip indexes for filters that don't align with the ordering key.
    -- These are cheap bloom-filter-style indexes over each granule rather
    -- than a full secondary index, which fits ClickHouse's read pattern of
    -- "skip whole granules that can't match" instead of point lookups.
    INDEX idx_events_tx_hash tx_hash TYPE bloom_filter GRANULARITY 4,
    INDEX idx_events_type type TYPE set(16) GRANULARITY 4
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(created_at)
ORDER BY (network, contract_id, ledger, id);

-- ingestion_state is a small, frequently-overwritten singleton-per-network
-- row (see Store.GetIngestionState/SaveIngestionState). ReplacingMergeTree
-- keyed on updated_at lets SaveIngestionState just INSERT a new row per
-- call instead of needing an UPDATE (which MergeTree engines don't support
-- well); readers select with argMax(..., updated_at) or FINAL to get the
-- latest row per network without waiting for a background merge.
CREATE TABLE IF NOT EXISTS ingestion_state
(
    network              LowCardinality(String),
    last_ingested_ledger Int64 DEFAULT 0,
    last_cursor          String DEFAULT '',
    last_successful_poll Nullable(DateTime64(3)),
    updated_at           DateTime64(3) DEFAULT now64(3)
)
ENGINE = ReplacingMergeTree(updated_at)
ORDER BY network;

-- address_refs denormalizes the address -> event relationship that lives
-- inside each event's JSON topics on the Postgres side (see
-- Store.UpsertAddressRefs/QueryAddressEvents). ClickHouse has no jsonb
-- containment operator, so address lookups need their own indexed table
-- rather than scanning and parsing `events.topics` per query.
--
-- Ordering by (network, address, ledger) makes "all events for this
-- address, most recent first" — the QueryAddressEvents/GetAddressSummary
-- access pattern — a single contiguous range scan.
CREATE TABLE IF NOT EXISTS address_refs
(
    network     LowCardinality(String) DEFAULT 'default',
    address     String,
    event_id    String,
    contract_id String,
    ledger      Int64,
    created_at  DateTime64(3) DEFAULT now64(3)
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(created_at)
ORDER BY (network, address, ledger);

-- audit_state mirrors the Postgres singleton row (one per network) tracking
-- how far the auditor has verified stored data against the RPC (see
-- Store.GetAuditState/SaveAuditState, and Stats' verified_through_ledger).
-- ReplacingMergeTree keyed on updated_at gives the same "just INSERT, read
-- back the latest" pattern as ingestion_state above, since ClickHouse has
-- no efficient row-level UPDATE.
CREATE TABLE IF NOT EXISTS audit_state
(
    network                 LowCardinality(String) DEFAULT 'default',
    verified_through_ledger Int64 DEFAULT 0,
    updated_at              DateTime64(3) DEFAULT now64(3)
)
ENGINE = ReplacingMergeTree(updated_at)
ORDER BY network;

-- watched_contracts is the append/remove list behind
-- Store.ListWatchedContracts (used by Stats' watched_contracts counter and
-- by ingestion to decide what to index). Removal is a logical delete
-- (removed_at set) rather than a DELETE statement: ClickHouse row deletes
-- are a heavyweight mutation, and this table is small and read via FINAL,
-- so a soft-delete flag is far cheaper for a set that is added to and
-- removed from one row at a time.
CREATE TABLE IF NOT EXISTS watched_contracts
(
    contract_id String,
    added_at    DateTime64(3) DEFAULT now64(3),
    removed_at  Nullable(DateTime64(3)) DEFAULT NULL
)
ENGINE = ReplacingMergeTree(added_at)
ORDER BY contract_id;
