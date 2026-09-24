# spec

## Purpose
The `spec` package handles contract specification enrichment, parsing Soroban contract binary interfaces (SCMeters, function signatures, event topics) to decode and describe contract events more precisely.

## Entry Points & Key Abstractions
- **`Enricher`**: Interface and implementation for looking up and attaching contract specifications to raw event streams.

## Non-Obvious Decisions & Invariants
- **Lossless Fallback**: Unknown or unparseable contract types fall back to lossless generic representations (`{"unknown": ...}`) rather than failing ingestion, ensuring pipeline stability when encountering newer or custom contract specs.
