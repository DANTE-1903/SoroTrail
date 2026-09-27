# Troubleshooting

Common SoroTrail errors, what each one actually means, and the fix. Every
entry follows the same shape:

- **Symptom** — the error string or observable behaviour.
- **Cause** — what produces it.
- **Fix** — what to do about it.

This guide is organised by where the error comes from. For incident
procedures (stalled ingestion, restoring from backup, paging runbooks) see
the [Operator Runbook](runbook.md); for re-decoding stored events see
[Decoder replay](replay.md).

---

## Table of contents

- [Startup and configuration errors](#startup-and-configuration-errors)
- [RPC errors](#rpc-errors)
- [Database errors](#database-errors)
- [API errors](#api-errors)
- [Replay errors](#replay-errors)
- [First things to check](#first-things-to-check)

---

## Startup and configuration errors

Configuration is validated once, at startup, and a failure exits non-zero
before anything connects. Every message names the offending variable.

### `DATABASE_URL is required`

- **Symptom** — the process exits immediately with this message.
- **Cause** — `DATABASE_URL` is unset or empty. It has no default: there is
  no sensible one, and guessing `localhost` would silently index into the
  wrong database.
- **Fix** — set it, e.g.
  `DATABASE_URL=postgres://sorotrail:secret@localhost:5432/sorotrail?sslmode=disable`.
  In Docker Compose the host is the service name (`db`), not `localhost`.
  The value may also be supplied out of a file with `DATABASE_URL_FILE`,
  which is what the Compose secrets wiring uses.

### `RPC_URL "..." is not a valid URL`

- **Symptom** — startup fails naming `RPC_URL` (or `RPC_URLS[n]`).
- **Cause** — the value does not parse as an absolute URL — most often a
  missing scheme (`soroban-rpc:8000` rather than `http://soroban-rpc:8000`),
  or a stray quote carried in from a shell export.
- **Fix** — include the scheme. For `RPC_URLS`, the value is a
  comma-separated list and **every** entry is validated, so one malformed
  fallback endpoint fails the whole startup.

### `NETWORK must be one of testnet, mainnet, futurenet; got "..."`

- **Symptom** — startup fails on the network name.
- **Cause** — `NETWORK` is free text in the environment but a closed set in
  the config. `pubnet` and `public` are common mistakes.
- **Fix** — use `mainnet` for the public network. The value is stored on
  every row, so changing it after ingestion has started partitions your data
  into two networks — see [Multi-tenancy](multi-tenancy.md) before switching.

### `POLL_INTERVAL_MIN (...) must be <= POLL_INTERVAL_MAX (...)`

- **Symptom** — startup fails on the adaptive-polling bounds.
- **Cause** — the adaptive poller narrows and widens its interval between
  these two bounds; inverted bounds have no valid interval to pick.
- **Fix** — set `POLL_INTERVAL_MIN` below `POLL_INTERVAL_MAX`, or leave both
  unset and let `POLL_INTERVAL` stand alone.

---

## RPC errors

### `rpc: all providers down, backing off`

- **Symptom** — repeated in the logs; ingestion makes no progress.
- **Cause** — every endpoint in `RPC_URLS` failed its last attempt. This is
  the failover layer reporting that it has nowhere left to fail over to, not
  a single bad request.
- **Fix** — check the endpoints by hand first:

  ```sh
  curl -s -X POST "$RPC_URL" \
    -H 'content-type: application/json' \
    -d '{"jsonrpc":"2.0","id":1,"method":"getHealth"}' | jq .
  ```

  A connection refused points at DNS/network or a dead container; an HTTP
  401/403 points at a provider API key that expired. Add a second provider
  to `RPC_URLS` so a single outage stops being an ingestion outage.

### `circuit breaker open: RPC endpoint unhealthy, retry after probe timeout`

- **Symptom** — requests fail instantly, without a network round trip.
- **Cause** — consecutive failures tripped the circuit breaker, which now
  fails fast instead of hammering a struggling endpoint. It closes again on
  its own after a successful probe.
- **Fix** — nothing, if the endpoint is coming back: the breaker re-probes
  and recovers. If it stays open, the endpoint is genuinely down — treat it
  as the case above.

### `getEvents returned HTTP 429` / sustained rate limiting

- **Symptom** — `429` in the logs, ingestion progressing in fits.
- **Cause** — the request rate is above what the provider allows. The
  auditor has its own budget on top of the ingester's, so an aggressive
  audit rate can push a healthy ingest rate over the limit.
- **Fix** — lower `RPC_RATE_LIMIT` (or `RPC_RATE_LIMIT_RPS` when several
  providers are configured through `RPC_URLS`) to the provider's documented
  limit, and re-check `AUDIT_MAX_RPS`. Retries are already exponential with
  jitter; more retries do not buy throughput against a limiter.

### `startLedger ... is outside of retention window`

- **Symptom** — a `getEvents` call fails naming the start ledger; the
  ingester logs a re-clamp and continues.
- **Cause** — the resume point aged out of the RPC's retention window
  (typically ~24 h of ledgers). SoroTrail detects this and re-clamps to the
  oldest retained ledger by itself.
- **Fix** — nothing for the running process. But the ledgers between the old
  cursor and the oldest retained ledger were **never ingested**: backfill
  them from an archive source, see [Backfill](backfill.md). If this recurs,
  the indexer is being stopped for longer than the retention window.

### `rpc: failover occurred, discard cursor and re-anchor from ledger position`

- **Symptom** — logged once at a provider switch.
- **Cause** — pagination cursors are provider-specific. After failing over,
  the old cursor is meaningless, so it is dropped and the position is
  re-anchored on ledger number instead.
- **Fix** — expected behaviour, not an error. Events near the switch may be
  re-fetched; ingestion is idempotent, so they upsert without duplicating.

---

## Database errors

### `failed to connect to host=... : connection refused`

- **Symptom** — startup or a later reconnection fails; `/health` reports the
  database check as failing.
- **Cause** — Postgres is not reachable at the configured host/port.
- **Fix** — confirm the server is up (`pg_isready -d "$DATABASE_URL"`), and
  that the host is right for where SoroTrail runs: inside Compose it is the
  service name, not `localhost`.

### `FATAL: sorry, too many clients already` / pool timeouts

- **Symptom** — queries fail under load; API latency spikes then errors.
- **Cause** — the sum of `DB_MAX_CONNS` across replicas exceeds Postgres's
  `max_connections`. Each SoroTrail process holds its own pool, and replay
  takes one additional connection for its advisory lock.
- **Fix** — size the pools so
  `replicas × DB_MAX_CONNS + head-room ≤ max_connections`, or put PgBouncer
  in front. See [Performance](performance.md) for pool sizing.

### `Dirty database version N. Fix and force version.`

- **Symptom** — startup fails during migrations.
- **Cause** — a migration was interrupted part-way (a killed container, a
  lost connection), leaving the schema-version row marked dirty. The
  migration runner refuses to guess whether the statements applied.
- **Fix** — inspect what actually landed before forcing anything:

  ```sh
  sorotrail migrate-status
  sorotrail schema-inspect
  ```

  Then finish or roll back the partial migration by hand and clear the dirty
  flag. The full procedure is in the runbook under
  [Dirty migration recovery](runbook.md#dirty-migration-recovery).

### `ensuring event partitions for ledger range [a,b]: ...`

- **Symptom** — ingestion or backfill fails when writing a batch.
- **Cause** — the events table is partitioned by ledger span, and the
  partition for the incoming range could not be created — usually
  insufficient privileges (the role needs `CREATE` on the schema) or a
  concurrent `DDL` lock.
- **Fix** — grant the role the ability to create tables in the schema, or
  pre-create partitions. Do not shrink `PARTITION_LEDGER_SPAN` on an
  existing database: it changes the partition boundaries, not just future
  ones. See [Indexes](indexes.md) and [Archival](archival.md).

### Queries suddenly slow, disk growing

- **Symptom** — API p99 climbs over days; no errors.
- **Cause** — dead tuples from pruning, or a GIN index whose pending list
  never gets flushed, or partitions no longer being pruned by the planner
  because a query lost its ledger bound.
- **Fix** — check autovacuum on the events partitions first, then compare
  the query plan against the shapes in [Performance](performance.md). A
  query with no `from_ledger` / `to_ledger` bound scans every partition.

---

## API errors

### `400 invalid cursor for order_by=...`

- **Symptom** — pagination fails mid-walk.
- **Cause** — the cursor encodes the ordering it was minted under. Replaying
  it against a different `order_by` would skip or repeat rows, so it is
  refused.
- **Fix** — restart the walk with the ordering you want, and keep
  `order_by` / `order` constant for its duration.

### `404 event ... not found` for an event you know exists

- **Symptom** — a single-event lookup 404s while the list endpoint shows it.
- **Cause** — in multi-tenant mode every read is scoped to the caller's
  granted contracts, and an ungranted event is reported as *absent* rather
  than *forbidden*, so the endpoint cannot be used as an existence oracle.
- **Fix** — grant the contract to the tenant. See
  [Multi-tenancy](multi-tenancy.md).

### Responses contain `"decoded": false` and a `decode_error`

- **Symptom** — `?decoded=true` returns events flagged undecoded.
- **Cause** — no contract spec is available for that contract, or the
  spec-declared fields did not decode. Enrichment never fails the request.
- **Fix** — upload the contract spec, or drop the parameter. To see the
  stored values with nothing derived layered on top, use `?decoded=false`.

---

## Replay errors

### `another replay is already running against this database`

- **Symptom** — `sorotrail replay` exits immediately with this message.
- **Cause** — the Postgres advisory lock that serialises replays is held by
  another run. It is a `try` lock, so a second run fails fast rather than
  queueing.
- **Fix** — wait for the other run, or find it:

  ```sql
  SELECT pid, application_name, state
    FROM pg_locks JOIN pg_stat_activity USING (pid)
   WHERE locktype = 'advisory';
  ```

  The lock is session-level, so a killed replay releases it automatically
  when its connection drops — there is never a stale lock to clean up.

### Replay reports a non-zero `rows failed` count

- **Symptom** — the summary ends with `rows failed: N`.
- **Cause** — the stored XDR for those rows could not be decoded at all.
  They are logged with their event ID and left untouched so one malformed
  row cannot wedge the run.
- **Fix** — pull one of the offending rows' raw XDR
  (`GET /events/{id}/raw`) and decode it by hand. A non-zero count always
  deserves investigation; `rows skipped` does not (see
  [replay.md](replay.md#rows-without-raw-xdr)).

### Exit code 2 from a replay

- **Symptom** — the command exits `2` without an error message.
- **Cause** — the run was interrupted (Ctrl-C, SIGTERM) and stopped cleanly
  between batches.
- **Fix** — re-run the identical command; it resumes from the last committed
  batch.

---

## First things to check

When the symptom does not match anything above, these four answers narrow it
down fast:

```sh
# 1. What does the process think its own health is?
curl -s localhost:8080/health | jq .

# 2. How far behind is ingestion, and what are the error counters?
curl -s localhost:8080/stats | jq .

# 3. Is the RPC endpoint answering at all?
curl -s -X POST "$RPC_URL" -H 'content-type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"getHealth"}' | jq .

# 4. Is the database reachable, and is the schema current?
pg_isready -d "$DATABASE_URL" && sorotrail migrate-status
```

A healthy process with a growing lag is an RPC or throughput problem; an
unhealthy process is nearly always configuration or the database.
