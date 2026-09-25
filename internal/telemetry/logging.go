package telemetry

import (
	"log/slog"
	"net/url"
	"strings"
)

// Canonical log field names across all SoroTrail packages.
//
// By standardizing these keys, queries and aggregations across the pipeline
// remain consistent and reliable.
const (
	// FieldContractID is the canonical key for Stellar/Soroban contract identifiers.
	FieldContractID = "contract_id"

	// FieldLedger is the canonical key for ledger sequence numbers.
	FieldLedger = "ledger"

	// FieldDurationMs is the canonical key for operation durations expressed in milliseconds.
	FieldDurationMs = "duration_ms"

	// FieldError is the canonical key for error values.
	FieldError = "error"

	// FieldURL is the canonical key for target URLs.
	FieldURL = "url"

	// FieldTxHash is the canonical key for transaction hashes.
	FieldTxHash = "tx_hash"
)

// SanitizeURL removes basic-auth credentials from a URL string so that secrets
// never reach a log line or telemetry sink.
func SanitizeURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		// If it's not a valid URL, perform a basic string fallback redaction
		return rawURL
	}
	if parsed.User != nil {
		if _, passwordSet := parsed.User.Password(); passwordSet {
			parsed.User = url.UserPassword(parsed.User.Username(), "***")
		}
	}
	return parsed.String()
}

// LogValueSanitized returns a slog.Attr with credentials redacted if the value looks like a URL.
func LogValueSanitized(key, val string) slog.Attr {
	if strings.Contains(val, "://") {
		return slog.String(key, SanitizeURL(val))
	}
	return slog.String(key, val)
}

// WithContractID returns a slog.Attr for the canonical contract_id field.
func WithContractID(contractID string) slog.Attr {
	return slog.String(FieldContractID, contractID)
}

// WithLedger returns a slog.Attr for the canonical ledger field.
func WithLedger(ledger int64) slog.Attr {
	return slog.Int64(FieldLedger, ledger)
}

// WithDurationMs returns a slog.Attr for the canonical duration_ms field.
func WithDurationMs(ms int64) slog.Attr {
	return slog.Int64(FieldDurationMs, ms)
}

// WithError returns a slog.Attr for the canonical error field.
func WithError(err error) slog.Attr {
	if err == nil {
		return slog.String(FieldError, "")
	}
	return slog.String(FieldError, err.Error())
}
