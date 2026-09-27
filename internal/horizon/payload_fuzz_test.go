package horizon

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

func FuzzParsePayload(f *testing.F) {
	f.Add([]byte(`{"id":"123","paging_token":"456","ledger":100}`))
	f.Add([]byte(`invalid horizon payload`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var tx Transaction
		err := json.Unmarshal(data, &tx)
		if err != nil {
			assert.Error(t, err)
		}
	})
}
