package graphql

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func FuzzParseCursor(f *testing.F) {
	f.Add("eyJvZmZzZXQiOiAxMDJ9")
	f.Add("not-a-valid-cursor")
	f.Fuzz(func(t *testing.T, s string) {
		_, err := DecodeCursor(s)
		if err != nil {
			assert.Error(t, err)
		}
	})
}
