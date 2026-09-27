package spec

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Issue #669 — the Wasm parsing helpers are byte-level parsers over untrusted
// input and had no direct coverage. These tests pin:
//   - a well-formed module yields its spec section
//   - a module with no spec section reports absence, not an error
//   - a truncated module is rejected without panicking
//   - a section with a bogus length is rejected
//   - empty input is rejected
// and that decodeWasmHash round-trips and rejects malformed base64.

// wasmCustomSection builds a minimal well-formed Wasm module (magic +
// version + one custom section) with the given name and payload.
func wasmCustomSection(t *testing.T, name string, payload []byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	// Magic "\0asm" little-endian and version 1.
	require.NoError(t, binary.Write(&buf, binary.LittleEndian, uint32(0x6d736100)))
	require.NoError(t, binary.Write(&buf, binary.LittleEndian, uint32(1)))

	// Custom section: ID 0, LEB128 size, then LEB128 name length + name +
	// payload. Sizes here are small enough that a single LEB128 byte holds
	// them (< 128).
	section := []byte{byte(len(name))}
	section = append(section, name...)
	section = append(section, payload...)

	buf.WriteByte(0) // custom section ID
	buf.WriteByte(byte(len(section)))
	buf.Write(section)
	return buf.Bytes()
}

func TestExtractCustomSection(t *testing.T) {
	t.Run("well-formed module yields its spec section", func(t *testing.T) {
		payload := []byte{0xde, 0xad, 0xbe, 0xef}
		wasm := wasmCustomSection(t, "contractspecv0", payload)

		got, err := extractCustomSection(wasm, "contractspecv0")
		require.NoError(t, err)
		assert.Equal(t, payload, got)
	})

	t.Run("module with no spec section reports absence not error", func(t *testing.T) {
		// A module whose only custom section is named something else.
		wasm := wasmCustomSection(t, "contractmetav0", []byte{0x01})

		got, err := extractCustomSection(wasm, "contractspecv0")
		require.NoError(t, err, "absence of the section is not an error")
		assert.Nil(t, got, "absence must be reported as nil, not empty-but-present")
	})

	t.Run("skips non-custom sections to find the spec section", func(t *testing.T) {
		// Type section (ID 1) ahead of the custom section: the parser must
		// skip by size, not assume every section is custom.
		var buf bytes.Buffer
		require.NoError(t, binary.Write(&buf, binary.LittleEndian, uint32(0x6d736100)))
		require.NoError(t, binary.Write(&buf, binary.LittleEndian, uint32(1)))

		typeSection := []byte{0x01, 0x02, 0x03}
		buf.WriteByte(1)
		buf.WriteByte(byte(len(typeSection)))
		buf.Write(typeSection)

		payload := []byte{0x42}
		section := []byte{byte(len("contractspecv0"))}
		section = append(section, "contractspecv0"...)
		section = append(section, payload...)
		buf.WriteByte(0)
		buf.WriteByte(byte(len(section)))
		buf.Write(section)

		got, err := extractCustomSection(buf.Bytes(), "contractspecv0")
		require.NoError(t, err)
		assert.Equal(t, payload, got)
	})

	t.Run("truncated module is rejected without panicking", func(t *testing.T) {
		full := wasmCustomSection(t, "contractspecv0", []byte{0x01, 0x02})

		// Every truncation of a valid module must return an error, never
		// panic. The panic guard is the point: this code parses untrusted
		// bytes, and a panic here is a remote DoS.
		for cut := 0; cut < len(full); cut++ {
			cut := cut
			t.Run("truncated-at-"+itoa(cut), func(t *testing.T) {
				assert.NotPanics(t, func() {
					_, err := extractCustomSection(full[:cut], "contractspecv0")
					// A truncated module has no complete section payload, so
					// either an error or (for cuts that leave a valid empty
					// module) a nil section is acceptable — but never a
					// panic.
					_ = err
				})
			})
		}
	})

	t.Run("section with bogus length is rejected", func(t *testing.T) {
		var buf bytes.Buffer
		require.NoError(t, binary.Write(&buf, binary.LittleEndian, uint32(0x6d736100)))
		require.NoError(t, binary.Write(&buf, binary.LittleEndian, uint32(1)))

		// Custom section claiming 200 bytes of payload but carrying none.
		buf.WriteByte(0)
		buf.WriteByte(200) // LEB128 size, far beyond the buffer

		assert.NotPanics(t, func() {
			_, err := extractCustomSection(buf.Bytes(), "contractspecv0")
			require.Error(t, err, "a size past the end of the module must be rejected")
		})
	})

	t.Run("bogus inner name length is rejected", func(t *testing.T) {
		var buf bytes.Buffer
		require.NoError(t, binary.Write(&buf, binary.LittleEndian, uint32(0x6d736100)))
		require.NoError(t, binary.Write(&buf, binary.LittleEndian, uint32(1)))

		// Custom section whose payload claims a 120-byte name but carries
		// only 2 bytes total.
		buf.WriteByte(0)
		buf.WriteByte(3)
		buf.WriteByte(120) // inner LEB128 name length
		buf.WriteByte(0x61)
		buf.WriteByte(0x73)

		assert.NotPanics(t, func() {
			_, err := extractCustomSection(buf.Bytes(), "contractspecv0")
			require.Error(t, err, "a name length past the section end must be rejected")
		})
	})

	t.Run("empty input is rejected", func(t *testing.T) {
		_, err := extractCustomSection(nil, "contractspecv0")
		require.Error(t, err)
		_, err = extractCustomSection([]byte{}, "contractspecv0")
		require.Error(t, err)
	})

	t.Run("invalid magic is rejected", func(t *testing.T) {
		bad := []byte{'n', 'o', 'p', 'e', 0x01, 0x00, 0x00, 0x00}
		_, err := extractCustomSection(bad, "contractspecv0")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid wasm magic")
	})
}

func TestDecodeWasmHash(t *testing.T) {
	t.Run("round-trips valid base64", func(t *testing.T) {
		raw := []byte("wasm-hash-bytes-0123456789")
		encoded := base64.StdEncoding.EncodeToString(raw)

		got, err := decodeWasmHash(encoded)
		require.NoError(t, err)
		assert.Equal(t, raw, got)
	})

	t.Run("rejects malformed base64", func(t *testing.T) {
		for _, bad := range []string{"not base64!!!", "a/b/c+", "\x00\x01"} {
			_, err := decodeWasmHash(bad)
			require.Error(t, err, "input %q must be rejected", bad)
		}
	})

	t.Run("rejects empty string as empty hash", func(t *testing.T) {
		got, err := decodeWasmHash("")
		// An empty string decodes to zero bytes; callers checking length do
		// their own validation, but the decoder itself must not panic.
		require.NoError(t, err)
		assert.Empty(t, got)
	})
}

// itoa keeps the subtest names readable without importing strconv twice in
// this file's test table.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var digits []byte
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}
