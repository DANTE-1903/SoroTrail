package docs

// Structural test for the README's GET /events section: every filter the
// section documents must come with a copy-paste curl example inside that
// same section. It follows the drift-catching idea of
// internal/config/config_drift_test.go — the docs are the source of truth
// and the test only asserts a property of them. Issue #298 asked for an
// example per filter; this test is what keeps them from rotting away.

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// eventsSection returns the README text between the plain "GET /events"
// line and the "GET /events/{id}" line that ends the section.
func eventsSection(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../README.md")
	require.NoErrorf(t, err, "README.md is missing")
	doc := string(b)

	start := strings.Index(doc, "\nGET /events\n")
	require.GreaterOrEqualf(t, start, 0, "README.md must contain a GET /events section")
	rest := doc[start:]
	end := strings.Index(rest, "\nGET /events/{id}")
	require.GreaterOrEqualf(t, end, 0, "the GET /events section must be followed by GET /events/{id}")
	return rest[:end]
}

// TestEventsFilterExamples requires a copy-paste curl example for each
// documented GET /events filter. Table-driven: one row per filter listed
// in the section's query-parameter table, plus topic_contains and
// include_xdr, which the surrounding prose documents in the same section.
func TestEventsFilterExamples(t *testing.T) {
	section := eventsSection(t)

	tests := []struct {
		name  string
		param string
	}{
		{name: "contract_id", param: "contract_id"},
		{name: "type", param: "type"},
		{name: "in_successful_call", param: "in_successful_call"},
		{name: "topic", param: "topic"},
		{name: "topic_contains", param: "topic_contains"},
		{name: "topic0", param: "topic0"},
		{name: "topic1", param: "topic1"},
		{name: "topic2", param: "topic2"},
		{name: "topic3", param: "topic3"},
		{name: "tx_hash", param: "tx_hash"},
		{name: "from_ledger", param: "from_ledger"},
		{name: "to_ledger", param: "to_ledger"},
		{name: "from_time", param: "from_time"},
		{name: "to_time", param: "to_time"},
		{name: "limit", param: "limit"},
		{name: "cursor", param: "cursor"},
		{name: "order", param: "order"},
		{name: "order_by", param: "order_by"},
		{name: "decoded", param: "decoded"},
		{name: "include_xdr", param: "include_xdr"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A copy-paste example is a curl line whose URL carries the
			// parameter as a query-string key (?param= or &param=).
			example := regexp.MustCompile(`curl[^\n]*[?&]` + regexp.QuoteMeta(tt.param) + `=`)
			assert.Regexpf(t, example, section,
				"GET /events filter %q has no copy-paste curl example in README.md", tt.param)
		})
	}
}
