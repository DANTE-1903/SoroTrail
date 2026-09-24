# store

## Purpose
The `store` package provides persistence layers and database abstractions for storing indexed ledgers, transactions, events, and contract states, acting as the primary persistence engine for SoroTrail.

## Entry Points & Key Abstractions
- **`Store`**: Core persistence interface defining methods for writing and querying historical chain data.
- **`PostgresStore`**: Production PostgreSQL implementation supporting transactions, connection pooling, and optimized indexing for event streams.

## Non-Obvious Decisions & Invariants
- **Idempotency**: All write operations are designed to be fully idempotent, safely handling duplicate ingestion of ledgers or blocks during recovery or replay scenarios.
- **Cross-Links**: Refer to the architecture document for details on schema partitioning and migration strategies.
