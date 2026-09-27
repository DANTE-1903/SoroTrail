package telemetry

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCanonicalLogFields(t *testing.T) {
	assert.Equal(t, "contract_id", FieldContractID)
	assert.Equal(t, "ledger", FieldLedger)
	assert.Equal(t, "duration_ms", FieldDurationMs)
	assert.Equal(t, "error", FieldError)
	assert.Equal(t, "url", FieldURL)
	assert.Equal(t, "tx_hash", FieldTxHash)
}

func TestSanitizeURL(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "plain url unchanged",
			input: "https://rpc.example.com",
			want:  "https://rpc.example.com",
		},
		{
			name:  "basic auth password redacted",
			input: "https://user:secretpassword@rpc.example.com/path",
			want:  "https://user:%2A%2A%2A@rpc.example.com/path",
		},
		// A password does not have to be exotic to defeat url.Parse, and the
		// old fallback returned the string verbatim — so the one input most
		// likely to hold a secret was the one that got logged in full.
		{
			name:  "unparseable url is redacted, not passed through",
			input: "https://user:sekret%pass@rpc.example.com/path",
			want:  "<redacted>",
		},
		{
			name:  "space in userinfo is redacted",
			input: "https://user:has space@rpc.example.com/path",
			want:  "<redacted>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SanitizeURL(tt.input)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestAttrHelpers(t *testing.T) {
	cAttr := WithContractID("CA")
	assert.Equal(t, FieldContractID, cAttr.Key)

	lAttr := WithLedger(42)
	assert.Equal(t, FieldLedger, lAttr.Key)

	dAttr := WithDurationMs(150)
	assert.Equal(t, FieldDurationMs, dAttr.Key)

	eAttr := WithError(errors.New("failed"))
	assert.Equal(t, FieldError, eAttr.Key)
}
