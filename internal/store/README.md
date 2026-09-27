# store

## Purpose
The `store` package provides persistence layers and database abstractions for storing indexed ledgers, transactions, events, and contract states, acting as the primary persistence engine for SoroTrail.

## Entry Points & Key Abstractions
- **`Store`**: Core persistence interface defining methods for writing and querying historical chain data.
- **`PostgresStore`**: Production PostgreSQL implementation supporting transactions, connection pooling, and optimized indexing for event streams.
- **`SQLite`** and **`ClickHouse`**: alternative backends selected via `DATABASE_URL`.

## Non-Obvious Decisions & Invariants
- **Idempotency**: All write operations are designed to be fully idempotent, safely handling duplicate ingestion of ledgers or blocks during recovery or replay scenarios.
- **Cross-Links**: Refer to the architecture document for details on schema partitioning and migration strategies.
- **Honest gaps**: a backend that does not implement an operation returns an error wrapping `ErrUnsupported` instead of a zero value. A silent empty result reads as success, which is how an unimplemented feature becomes a wrong 200; `ErrUnsupported` makes the gap loud and mappable.

## Backend capability matrix
The shared conformance suite in `conformance_test.go` runs the same assertions against every registered backend. A backend is registered in `conformanceBackends`; server-backed backends are skipped cleanly when their URL environment variable is unset.

| Capability | Postgres | SQLite | ClickHouse |
| --- | --- | --- | --- |
| Events, queries, pagination | yes | yes | declared unsupported |
| Ingestion/audit state, watched contracts | yes | yes | declared unsupported |
| Per-contract cursors | yes | `ErrUnsupported` | declared unsupported |
| Contract inventory (`ListContracts`, summaries, metadata) | yes | `ErrUnsupported` | declared unsupported |
| API keys | yes | `ErrUnsupported` | `ErrUnsupported` |
| Server required for tests | `TEST_DATABASE_URL` | no | `TEST_CLICKHOUSE_URL` |

SQLite deliberately stays a single-node backend: per-contract resume positions and the contract inventory endpoints are Postgres-only, and both now refuse explicitly rather than returning an empty result.
