package decode

// Golden-file coverage for the ScVal decoder. The input file contains
// representative, literal base64 XDR values; the matching golden file pins
// the JSON shape emitted for each value. Keeping both sides in testdata
// makes a wire-format or decoder-output change visible in a focused diff.
//
// Regenerate the expected JSON after an intentional decoder change with:
//
//	go test ./internal/decode -run TestXDRDecoder_GoldenFixtures -update-golden
//
// Review the resulting changes before committing them.

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var updateDecoderGolden = flag.Bool("update-golden", false, "rewrite ScVal decoder golden outputs")

type scvalFixture struct {
	Name string `json:"name"`
	XDR  string `json:"xdr"`
}

type scvalGolden map[string]json.RawMessage

func TestXDRDecoder_GoldenFixtures(t *testing.T) {
	fixtures := loadScvalFixtures(t)
	require.NotEmpty(t, fixtures, "decoder fixture file must not be empty")

	actual := make(scvalGolden, len(fixtures))
	seen := make(map[string]struct{}, len(fixtures))
	for _, fixture := range fixtures {
		require.NotEmpty(t, fixture.Name, "fixture name must not be empty")
		require.NotEmpty(t, fixture.XDR, "fixture %q must contain an XDR blob", fixture.Name)
		_, duplicate := seen[fixture.Name]
		require.False(t, duplicate, "duplicate decoder fixture %q", fixture.Name)
		seen[fixture.Name] = struct{}{}

		got, err := (XDRDecoder{}).DecodeScVal(fixture.XDR)
		require.NoErrorf(t, err, "decoding fixture %q", fixture.Name)
		actual[fixture.Name] = append(json.RawMessage(nil), got...)
	}

	if *updateDecoderGolden {
		writeScvalGolden(t, actual)
		return
	}

	golden := loadScvalGolden(t)
	require.Len(t, golden, len(fixtures), "every fixture must have exactly one golden output")
	for _, fixture := range fixtures {
		fixture := fixture
		t.Run(fixture.Name, func(t *testing.T) {
			want, ok := golden[fixture.Name]
			require.Truef(t, ok, "missing golden output for %q", fixture.Name)
			got := actual[fixture.Name]
			assert.JSONEqf(t, string(want), string(got), "decoded shape drifted for fixture %q", fixture.Name)
		})
	}
	for name := range golden {
		_, ok := seen[name]
		assert.Truef(t, ok, "golden output %q has no matching fixture", name)
	}
}

func loadScvalFixtures(t *testing.T) []scvalFixture {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "scval_fixtures.json"))
	require.NoError(t, err, "reading ScVal fixtures")
	var fixtures []scvalFixture
	require.NoError(t, json.Unmarshal(data, &fixtures), "parsing ScVal fixtures")
	return fixtures
}

func loadScvalGolden(t *testing.T) scvalGolden {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "golden", "scval_decoded.json"))
	require.NoError(t, err, "reading ScVal golden outputs")
	var golden scvalGolden
	require.NoError(t, json.Unmarshal(data, &golden), "parsing ScVal golden outputs")
	return golden
}

func writeScvalGolden(t *testing.T, golden scvalGolden) {
	t.Helper()
	data, err := json.MarshalIndent(golden, "", "  ")
	require.NoError(t, err, "marshaling ScVal golden outputs")
	data = append(data, '\n')
	path := filepath.Join("testdata", "golden", "scval_decoded.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, data, 0o644))
	t.Logf("updated ScVal decoder golden file %s", path)
}
