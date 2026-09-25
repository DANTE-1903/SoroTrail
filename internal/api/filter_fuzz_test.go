package api

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

func FuzzFilterValues(f *testing.F) {
	f.Add("contract_id=C123&limit=10")
	f.Add("contract_id=%ZZ")
	f.Fuzz(func(t *testing.T, raw string) {
		req, err := http.NewRequest("GET", "/?"+raw, nil)
		if err == nil {
			_, _, err = parseFilterAndFields(req)
			if err != nil {
				assert.Error(t, err)
			}
		}
	})
}
