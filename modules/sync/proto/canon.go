package proto

import (
	"bytes"
	"encoding/json"
	"io"
)

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

func encodeNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
