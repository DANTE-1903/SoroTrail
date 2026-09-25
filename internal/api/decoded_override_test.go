package api

// Tests for the ?decoded= parameter's three modes, in particular the
// ?decoded=false opt-out that returns the stored columns untouched: no
// spec-driven enrichment and no additive sep41_event envelope.

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorotrail/internal/store"
)

// sep41TransferEvent is an event whose topics/value match the SEP-41
// transfer shape, so the default rendering attaches a sep41_event envelope
// to it and ?decoded=false does not.
func sep41TransferEvent(id string) store.Event {
	return store.Event{
		ID:               id,
		ContractID:       testContract,
		Ledger:           43,
		Type:             "contract",
		TxHash:           "f0e9d8c7b6a59876543f2e1d0c9b8a796857463728193afbccddeeff00112233",
		InSuccessfulCall: true,
		Topics: json.RawMessage(`[{"symbol":"transfer"},` +
			`{"address":"GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAWHF"},` +
			`{"address":"GBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"}]`),
		Value:   json.RawMessage(`{"i128":"1000000000"}`),
		Network: "public",
	}
}

// TestDecodedOverride_SEP41Envelope pins which ?decoded= values suppress
// the additive SEP-41 envelope on the list and single-event paths. Only the
// exact string "false" opts out; every other value keeps the default.
func TestDecodedOverride_SEP41Envelope(t *testing.T) {
	const id = "0000000043-0000000001"

	tests := []struct {
		name       string
		query      string
		wantSEP41  bool
		wantDecode bool // the enriched response shape carries a "decoded" key
	}{
		{name: "no parameter keeps the envelope", query: "", wantSEP41: true},
		{name: "decoded=true keeps the envelope", query: "?decoded=true", wantSEP41: true, wantDecode: true},
		{name: "decoded=false drops the envelope", query: "?decoded=false"},
		{name: "unrecognised value keeps the default", query: "?decoded=yes", wantSEP41: true},
		{name: "empty value keeps the default", query: "?decoded=", wantSEP41: true},
		{name: "mixed case is not the opt-out", query: "?decoded=False", wantSEP41: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := sep41TransferEvent(id)

			t.Run("list", func(t *testing.T) {
				st := &stubStore{events: []store.Event{ev}}
				s := New(st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), "test-key", &stubEnricher{})

				resp, body := doGet(t, s, "/events"+tt.query)
				require.Equal(t, http.StatusOK, resp.StatusCode)

				var out struct {
					Events []map[string]json.RawMessage `json:"events"`
				}
				require.NoError(t, json.Unmarshal(body, &out))
				require.Len(t, out.Events, 1)

				_, ok := out.Events[0]["sep41_event"]
				assert.Equal(t, tt.wantSEP41, ok,
					"sep41_event presence for %q: %s", tt.query, body)
			})

			t.Run("single event", func(t *testing.T) {
				st := &stubStore{event: ev}
				s := New(st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), "test-key", &stubEnricher{})

				resp, body := doGet(t, s, "/events/"+id+tt.query)
				require.Equal(t, http.StatusOK, resp.StatusCode)

				var out map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(body, &out))

				_, ok := out["sep41_event"]
				assert.Equal(t, tt.wantSEP41, ok,
					"sep41_event presence for %q: %s", tt.query, body)

				_, decodedKey := out["decoded"]
				assert.Equal(t, tt.wantDecode, decodedKey,
					"enriched shape for %q: %s", tt.query, body)
			})
		})
	}
}

// TestDecodedOverride_SkipsEnrichment checks ?decoded=false is a hard opt
// out of spec enrichment even with an enricher wired: the response keeps the
// plain event shape, with none of the enriched fields.
func TestDecodedOverride_SkipsEnrichment(t *testing.T) {
	const id = "0000000043-0000000001"

	for _, path := range []string{
		"/events?decoded=false",
		"/events/" + id + "?decoded=false",
		"/events/" + id + "/transaction?decoded=false",
	} {
		t.Run(path, func(t *testing.T) {
			ev := sep41TransferEvent(id)
			st := &stubStore{events: []store.Event{ev}, event: ev, txSiblings: []store.Event{ev}}
			s := New(st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), "test-key", &stubEnricher{})

			resp, body := doGet(t, s, path)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			assert.NotContains(t, string(body), `"decoded"`,
				"an enriched payload leaked into a ?decoded=false response: %s", body)
			assert.NotContains(t, string(body), `"sep41_event"`,
				"a derived envelope leaked into a ?decoded=false response: %s", body)
			// The stored columns themselves are untouched.
			assert.Contains(t, string(body), `"i128":"1000000000"`)
		})
	}
}

// TestDecodedOverride_WithXDR checks the opt-out composes with
// ?include_xdr=true: raw XDR is still projected, the derived envelope is
// still absent.
func TestDecodedOverride_WithXDR(t *testing.T) {
	const id = "0000000043-0000000001"
	ev := sep41TransferEvent(id)
	ev.RawTopicXDR = []string{"AAAADwAAAAh0cmFuc2Zlcg=="}
	ev.RawValueXDR = "AAAACgAAAAAAAAAAAAAAADuaygA="

	st := &stubStore{events: []store.Event{ev}, event: ev}
	s := New(st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), "test-key", &stubEnricher{})

	resp, body := doGet(t, s, "/events?decoded=false&include_xdr=true")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(body), `"topics_xdr"`)
	assert.Contains(t, string(body), ev.RawValueXDR)
	assert.NotContains(t, string(body), `"sep41_event"`)
}

// TestDecodedOverride_Stream checks the NDJSON streaming path honours the
// opt-out too, so a streamed page and a paged read agree.
func TestDecodedOverride_Stream(t *testing.T) {
	ev := sep41TransferEvent("0000000043-0000000001")
	st := &stubStore{events: []store.Event{ev}}
	s := New(st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), "test-key", &stubEnricher{})

	resp, body := doGet(t, s, "/events?stream=true&decoded=false")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NotEmpty(t, strings.TrimSpace(string(body)))
	assert.NotContains(t, string(body), `"decoded"`)
	assert.NotContains(t, string(body), `"sep41_event"`)
}

// mustGetRequest builds a GET request for a path+query, for the parser
// table below.
func mustGetRequest(t *testing.T, target string) *http.Request {
	t.Helper()
	r, err := http.NewRequest(http.MethodGet, target, nil)
	require.NoError(t, err)
	return r
}

// TestDecodeModeFromQuery is the unit-level table for the parser the
// handlers share.
func TestDecodeModeFromQuery(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  decodeMode
	}{
		{"absent", "/events", decodeStored},
		{"true", "/events?decoded=true", decodeEnriched},
		{"false", "/events?decoded=false", decodeRaw},
		{"empty", "/events?decoded=", decodeStored},
		{"unknown", "/events?decoded=maybe", decodeStored},
		{"uppercase true", "/events?decoded=TRUE", decodeStored},
		{"uppercase false", "/events?decoded=FALSE", decodeStored},
		{"numeric zero", "/events?decoded=0", decodeStored},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := mustGetRequest(t, tt.query)
			got := decodeModeFromQuery(r)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.want == decodeEnriched, got.enrich(), "enrich()")
			assert.Equal(t, tt.want != decodeRaw, got.sep41(), "sep41()")
		})
	}
}
