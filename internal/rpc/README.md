# rpc

## Purpose
The `rpc` package handles client communication with external Stellar/Soroban JSON-RPC nodes, encapsulating request formatting, response parsing, and connection management.

## Entry Points & Key Abstractions
- **`Client`**: Concrete implementation wrapping underlying HTTP/WebSocket transports to query node state and ledger streams.

## Non-Obvious Decisions & Invariants
- **Resilient Polling**: Implements exponential backoff and jitter for transient network failures, maintaining connection stability during node sync lags or outages.
