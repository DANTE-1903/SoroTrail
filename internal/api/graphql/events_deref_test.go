package graphql

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDerefTime covers derefTime with a table test. A nil pointer must
// mean "no constraint" (the zero time), not a filter bound — otherwise
// every unfiltered query would silently gain a lower bound.
func TestDerefTime(t *testing.T) {
	utc := time.Date(2026, 7, 28, 15, 30, 0, 0, time.UTC)
	eastern := time.FixedZone("EST", -5*60*60)
	inEastern := time.Date(2026, 7, 28, 15, 30, 0, 0, eastern)
	epoch := time.Unix(0, 0).UTC()
	zero := time.Time{}

	tests := []struct {
		name  string
		input *time.Time
		want  time.Time
		// wantZero reports whether the result must be the zero time,
		// i.e. indistinguishable from "no constraint".
		wantZero bool
	}{
		{
			name:     "nil returns zero time meaning no constraint",
			input:    nil,
			want:     time.Time{},
			wantZero: true,
		},
		{
			name:     "non-nil returns its value",
			input:    &utc,
			want:     utc,
			wantZero: false,
		},
		{
			name:     "returned time keeps its location",
			input:    &inEastern,
			want:     inEastern,
			wantZero: false,
		},
		{
			name:     "explicit epoch is distinguishable from zero",
			input:    &epoch,
			want:     epoch,
			wantZero: false,
		},
		{
			name:     "explicit zero stays zero",
			input:    &zero,
			want:     time.Time{},
			wantZero: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := derefTime(tt.input)
			if tt.wantZero {
				assert.True(t, got.IsZero(), "expected zero time, got %v", got)
			} else {
				assert.False(t, got.IsZero(), "expected non-zero time, got zero")
			}
			assert.True(t, got.Equal(tt.want), "expected %v, got %v", tt.want, got)
			// The function returns the value verbatim, so a non-UTC
			// location must survive the round trip unchanged.
			if tt.input != nil {
				assert.Equal(t, tt.want.Location().String(), got.Location().String(),
					"location must be preserved")
			}
			require.Equal(t, tt.want, got)
		})
	}
}
