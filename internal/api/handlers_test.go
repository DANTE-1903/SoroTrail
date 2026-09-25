package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validAddr returns a 56-character string that starts with the given
// prefix and is otherwise filled with a legal base32 character ("A").
func validAddr(prefix byte) string {
	b := make([]byte, 56)
	b[0] = prefix
	for i := 1; i < 56; i++ {
		b[i] = 'A'
	}
	return string(b)
}

func TestIsValidAddress(t *testing.T) {
	validG := validAddr('G')
	validC := validAddr('C')

	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{
			name:  "valid G address returns true",
			input: validG,
			want:  true,
		},
		{
			name:  "valid C contract address returns true",
			input: validC,
			want:  true,
		},
		{
			name:  "55-character string returns false",
			input: validG[:55],
			want:  false,
		},
		{
			name:  "57-character string returns false",
			input: validG + "A",
			want:  false,
		},
		{
			name:  "correct-length string starting with X returns false",
			input: validAddr('X'),
			want:  false,
		},
		{
			name:  "lowercase input returns false",
			input: "g" + validG[1:],
			want:  false,
		},
		{
			name:  "base32-invalid characters 0, 1, 8, 9 return false",
			input: "G0000000000000000000000000000000000000000000000000000000",
			want:  false,
		},
		{
			name:  "empty string returns false",
			input: "",
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isValidAddress(tt.input)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestPaginationLink(t *testing.T) {
	tests := []struct {
		name            string
		url             string
		cursor          string
		wantContains    []string
		wantNotContains []string
	}{
		{
			name:         "non-empty cursor is set in the returned URL",
			url:          "https://example.com/events?contract_id=CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC",
			cursor:       "abc123",
			wantContains: []string{"cursor=abc123"},
		},
		{
			name:            "empty cursor removes the cursor parameter entirely",
			url:             "https://example.com/events?cursor=old&contract_id=CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC",
			cursor:          "",
			wantContains:    []string{"contract_id="},
			wantNotContains: []string{"cursor"},
		},
		{
			name:         "unrelated filters survive unchanged",
			url:          "https://example.com/events?contract_id=CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC&limit=50",
			cursor:       "nextpage",
			wantContains: []string{"cursor=nextpage", "contract_id=CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC", "limit=50"},
		},
		{
			name:            "existing cursor parameter is replaced rather than duplicated",
			url:             "https://example.com/events?cursor=old_value",
			cursor:          "new_value",
			wantContains:    []string{"cursor=new_value"},
			wantNotContains: []string{"cursor=old_value"},
		},
		{
			name:         "path is preserved for a nested route",
			url:          "https://example.com/contracts/CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC/events?from_ledger=100",
			cursor:       "page2",
			wantContains: []string{"/contracts/CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC/events", "cursor=page2", "from_ledger=100"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := http.NewRequest(http.MethodGet, tt.url, nil)
			assert.NoError(t, err)

			got := paginationLink(r, tt.cursor)

			for _, s := range tt.wantContains {
				assert.Contains(t, got, s)
			}
			for _, s := range tt.wantNotContains {
				assert.NotContains(t, got, s)
			}
		})
	}
}

func TestIngestLagLedgers(t *testing.T) {
	tests := []struct {
		name         string
		chainHead    int64
		lastIngested int64
		want         int64
	}{
		{
			name:         "chain head ahead of last ingested returns the difference",
			chainHead:    1000,
			lastIngested: 950,
			want:         50,
		},
		{
			name:         "equal values return zero",
			chainHead:    1000,
			lastIngested: 1000,
			want:         0,
		},
		{
			name:         "zero chain head returns zero rather than negative lag",
			chainHead:    0,
			lastIngested: 100,
			want:         0,
		},
		{
			name:         "negative chain head returns zero rather than negative lag",
			chainHead:    -1,
			lastIngested: 100,
			want:         0,
		},
		{
			name:         "zero last ingested returns zero",
			chainHead:    100,
			lastIngested: 0,
			want:         0,
		},
		{
			name:         "negative last ingested returns zero",
			chainHead:    100,
			lastIngested: -1,
			want:         0,
		},
		{
			name:         "last ingested ahead of chain head does not return negative",
			chainHead:    95,
			lastIngested: 100,
			want:         0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ingestLagLedgers(tt.chainHead, tt.lastIngested)
			assert.Equal(t, tt.want, got)
		})
	}
}

// countingReader records how many bytes were pulled from the body so a test
// can prove an oversized request was not buffered in full.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// TestDecodeJSONBody pins the boundary every JSON request body crosses
// first: the 4 KiB cap, the unknown-field policy, the empty-body message,
// and that a rejected body neither half-populates dst nor echoes its
// values back in the error.
func TestDecodeJSONBody(t *testing.T) {
	type target struct {
		ContractID string   `json:"contract_id"`
		Limit      int      `json:"limit"`
		Tags       []string `json:"tags"`
	}
	// secret stands in for a credential a client might post. It must never
	// appear in an error, which handlers pass straight to writeError.
	const secret = "sk_live_DO_NOT_ECHO"
	sentinel := target{ContractID: "untouched"}

	valid := `{"contract_id":"` + secret + `","limit":5,"tags":["a","b"]}`
	padTo := func(s string, n int) string { return s + strings.Repeat(" ", n-len(s)) }

	tests := []struct {
		name    string
		body    string
		nilBody bool
		want    target
		wantErr string
	}{
		{
			name: "valid body decodes into the struct",
			body: valid,
			want: target{ContractID: secret, Limit: 5, Tags: []string{"a", "b"}},
		},
		{
			// dst is replaced, not merged, so a field absent from the body
			// cannot carry a stale value into the handler.
			name: "fields absent from the body are zero, not carried over",
			body: `{"limit":1}`,
			want: target{Limit: 1},
		},
		{
			name: "surrounding whitespace is allowed",
			body: "\n\t " + valid + " \n",
			want: target{ContractID: secret, Limit: 5, Tags: []string{"a", "b"}},
		},
		{
			name: "body exactly at the limit is accepted",
			body: padTo(`{"limit":7}`, maxJSONBodyBytes),
			want: target{Limit: 7},
		},

		{name: "empty body", body: "", wantErr: "request body is empty"},
		{name: "whitespace-only body", body: " \r\n\t ", wantErr: "request body is empty"},
		{name: "nil body", nilBody: true, wantErr: "request body is empty"},

		{name: "one byte over the limit", body: padTo(`{"limit":7}`, maxJSONBodyBytes+1), wantErr: "request body exceeds 4096 bytes"},
		{name: "far over the limit", body: `{"contract_id":"` + strings.Repeat(secret, 1<<14) + `"}`, wantErr: "request body exceeds 4096 bytes"},

		{name: "malformed JSON", body: `{"contract_id":"` + secret + `", limit}`, wantErr: "invalid JSON body"},
		{name: "truncated JSON", body: `{"contract_id":"` + secret, wantErr: "invalid JSON body: unexpected EOF"},
		{name: "not an object", body: `"` + secret + `"`, wantErr: "invalid JSON body: json: cannot unmarshal string"},
		{
			// encoding/json fills contract_id before reaching the bad limit;
			// that partial value must not leak into dst.
			name:    "wrongly typed field leaves dst untouched",
			body:    `{"contract_id":"` + secret + `","limit":"` + secret + `"}`,
			wantErr: "cannot unmarshal string into Go struct field",
		},
		{
			// A typo in a field name is rejected rather than silently
			// dropped, per the doc comment's policy.
			name:    "unknown field is rejected",
			body:    `{"contract_id":"` + secret + `","contractID":"` + secret + `"}`,
			wantErr: `invalid JSON body: json: unknown field "contractID"`,
		},
		{name: "trailing garbage", body: valid + "garbage", wantErr: "unexpected data after the JSON value"},
		{name: "second JSON value", body: valid + `{"limit":9}`, wantErr: "unexpected data after the JSON value"},
		{name: "stray closing brace", body: valid + "}", wantErr: "unexpected data after the JSON value"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tt.body))
			if tt.nilBody {
				r.Body = nil
			}
			got := sentinel

			err := decodeJSONBody(r, &got)

			if tt.wantErr == "" {
				require.NoError(t, err)
				assert.Equal(t, tt.want, got)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			assert.NotContains(t, err.Error(), secret, "error must not echo body values")
			assert.Equal(t, sentinel, got, "dst must be untouched when decoding fails")
		})
	}
}

// TestDecodeJSONBody_OversizedBodyIsNotBuffered proves the cap bounds how
// much of a hostile body is read, not just whether it is accepted.
func TestDecodeJSONBody_OversizedBodyIsNotBuffered(t *testing.T) {
	src := &countingReader{r: strings.NewReader(`{"contract_id":"` + strings.Repeat("A", 1<<20) + `"}`)}
	r := httptest.NewRequest(http.MethodPost, "/", src)

	var dst struct {
		ContractID string `json:"contract_id"`
	}
	err := decodeJSONBody(r, &dst)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "request body exceeds")
	assert.LessOrEqual(t, src.n, maxJSONBodyBytes+1, "must stop reading one byte past the cap")
}

// TestDecodeJSONBody_RejectsNonPointerDst guards the scratch-value copy:
// decoding needs somewhere addressable to publish the result.
func TestDecodeJSONBody_RejectsNonPointerDst(t *testing.T) {
	var nilPtr *struct{}
	for _, dst := range []any{nil, struct{}{}, nilPtr} {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
		err := decodeJSONBody(r, dst)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "dst must be a non-nil pointer")
	}
}
