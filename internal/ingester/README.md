# ingester

## Purpose
The `ingester` package orchestrates the core data ingestion pipeline, fetching ledgers from Soroban/Stellar nodes, transforming raw blocks into structured events, and coordinating writes to the `store`.

## Entry Points & Key Abstractions
- **`Pipeline`**: Manages the multi-stage worker pool that pulls, decodes, and persists ledger streams.
- **`Worker`**: Individual processing units handling backpressure, retries, and network error recovery.

## Non-Obvious Decisions & Invariants
- **At-Least-Once Delivery**: The pipeline favors re-processing over data loss; workers will block or retry indefinitely upon upstream node or database failures until successful acknowledgment.
