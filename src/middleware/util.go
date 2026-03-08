package middleware

import (
	"encoding/json"
	"io"
)

func decodeJSON(r io.Reader, v interface{}) error {
	return json.NewDecoder(r).Decode(v)
}
