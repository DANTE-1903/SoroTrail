# api

## Purpose
The `api` package implements the HTTP/JSON-RPC server layer, exposing queried data, indexed events, and health metrics to external clients and downstream consumers.

## Entry Points & Key Abstractions
- **`Server`**: Initializes routing, middleware (logging, metrics, CORS), and endpoint handlers.
- **`Handlers`**: Request parsers and response formatters translating query parameters into `store` package lookups.

## Non-Obvious Decisions & Invariants
- **Strict Timeout Control**: All public API endpoints enforce strict context timeouts to prevent runaway database queries from exhausting connection pools.
