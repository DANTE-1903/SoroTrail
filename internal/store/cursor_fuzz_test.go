package store

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fuzzAnchorEvent builds the synthetic event the round-trip seeds are
// derived from. It is independent of the integration-gated test fixtures so
// the fuzz corpus is available in every test run.
func fuzzAnchorEvent() Event {
	return Event{
		ID:        "0000000000000424242-0000000001",
		Ledger:    424242,
		CreatedAt: time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC),
	}
}

// FuzzDecodeCompositeCursor verifies that the keyset cursor decoder never
// panics, no matter what a client sends as a cursor. Every input must
// produce either a decoded (sortValue, id) pair or a typed ErrInvalidCursor
// error, never a crash. A decode failure is an expected result for
// arbitrary input and is intentionally not treated as a test failure.
//
// The decoder parses base64url of "<sort value>|<id>" where the sort value
// is either a ledger number or an RFC3339Nano timestamp, so the seeds below
// cover each stage of that pipeline: valid composite cursors from real
// events (via a round trip through the encoder), base64 that decodes but
// has no separator, cursors with empty components, and raw text that is
// not base64 at all.
func FuzzDecodeCompositeCursor(f *testing.F) {
	anchor := fuzzAnchorEvent()

	// Round-trip seeds: exactly the cursors the store itself hands out.
	f.Add(EncodeCursor(OrderByID, anchor))
	f.Add(EncodeCursor(OrderByLedger, anchor))
	f.Add(EncodeCursor(OrderByCreatedAt, anchor))

	// Structural seeds: each stage a malicious input might stop at.
	f.Add("")
	f.Add("AAAA")                                              // decodes, but has no separator
	f.Add(base64.RawURLEncoding.EncodeToString([]byte("ab|"))) // empty id
	f.Add(base64.RawURLEncoding.EncodeToString([]byte("|ab"))) // empty sort value
	f.Add(base64.RawURLEncoding.EncodeToString([]byte("|")))   // separator only
	f.Add("not base64!!")
	f.Add("|")
	f.Add(strings.Repeat("A", 200)) // longer than any real cursor
	// Decodes successfully into components carrying bytes no real cursor
	// would: binary junk, NUL bytes, a UTF-8 replacement sequence. None of
	// them may panic or change the error contract.
	f.Add(base64.RawURLEncoding.EncodeToString([]byte("\xd3M4\x00")))
	f.Add(base64.RawURLEncoding.EncodeToString([]byte("\xff\xfe|\x00")))
	f.Add(base64.RawURLEncoding.EncodeToString([]byte("2026-09-24T12:00:00Z|" + strings.Repeat("effort", 40))))

	f.Fuzz(func(t *testing.T, input string) {
		sortValue, id, err := decodeCompositeCursor(input)

		switch err {
		case nil:
			// The decoder's only success guarantees: both components are
			// non-empty (it rejects empty ones), and because it splits on
			// the LAST "|" the id component never carries a separator. The
			// components may otherwise contain arbitrary bytes — that is
			// fine, downstream they only ever flow into parameterized SQL.
			if sortValue == "" || id == "" {
				t.Fatalf("decode succeeded with an empty component: sortValue=%q id=%q", sortValue, id)
			}
			if strings.Contains(id, "|") {
				t.Fatalf("decoded id %q carries a separator", id)
			}
		default:
			// Failure is fine, but it must be the typed error the handler
			// maps to 400 — not a wrapped panic value or something else.
			if !errors.Is(err, ErrInvalidCursor) {
				t.Fatalf("non-ErrInvalidCursor error for input %q: %v", input, err)
			}
		}
	})
}

// TestDecodeCursor_MalformedSeeds is the always-on (non-fuzzing) companion
// to FuzzDecodeCompositeCursor: it runs the seed corpus as a plain unit
// test so malformed-cursor coverage does not depend on `go test -fuzz`
// being run.
func TestDecodeCursor_MalformedSeeds(t *testing.T) {
	raw := func(s string) string {
		return base64.RawURLEncoding.EncodeToString([]byte(s))
	}

	tests := []struct {
		name   string
		cursor string
	}{
		{name: "empty", cursor: ""},
		{name: "not base64", cursor: "not base64!!"},
		{name: "base64 without separator", cursor: "AAAA"},
		{name: "missing id component", cursor: raw("4242|")},
		{name: "missing sort value component", cursor: raw("|ab")},
		{name: "separator only", cursor: raw("|")},
		{name: "raw separator no base64", cursor: "|"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := decodeCompositeCursor(tt.cursor)
			assert.ErrorIs(t, err, ErrInvalidCursor)
		})
	}

	t.Run("valid ledger cursor still decodes", func(t *testing.T) {
		v, id, err := decodeCompositeCursor(raw("4242|0000000000000424242-0000000001"))
		require.NoError(t, err)
		assert.Equal(t, "4242", v)
		assert.Equal(t, "0000000000000424242-0000000001", id)
	})
}
