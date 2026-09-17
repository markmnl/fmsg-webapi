package middleware

import (
	"encoding/json"
	"io"
)

func decodeJSON(r io.Reader, v any) error {
	return json.NewDecoder(r).Decode(v)
}
