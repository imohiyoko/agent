package mcpgw

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// errAmbiguous is returned for JSON that different parsers could read
// differently: an object with the same key twice, also when the keys differ
// only in case (encoding/json matches struct fields case-insensitively, so
// {"method":"tools/call","Method":"ping"} would be checked as ping and run
// as tools/call by a server that keeps the first key).
var errAmbiguous = errors.New("duplicate or case-colliding object keys")

// decodeStrict decodes one JSON-RPC message, refusing ambiguous JSON.
func decodeStrict(b []byte, m *message) error {
	if err := checkKeys(json.NewDecoder(bytes.NewReader(b))); err != nil {
		return err
	}
	return json.Unmarshal(b, m)
}

func checkKeys(dec *json.Decoder) error {
	dec.UseNumber()
	if err := checkValue(dec, 0); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data")
	}
	return nil
}

func checkValue(dec *json.Decoder, depth int) error {
	if depth > 64 {
		return errors.New("too deeply nested")
	}
	t, err := dec.Token()
	if err != nil {
		return err
	}
	d, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	switch d {
	case '{':
		seen := map[string]bool{}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return err
			}
			k := strings.ToLower(kt.(string))
			if seen[k] {
				return errAmbiguous
			}
			seen[k] = true
			if err := checkValue(dec, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for dec.More() {
			if err := checkValue(dec, depth+1); err != nil {
				return err
			}
		}
	}
	_, err = dec.Token() // closing delimiter
	return err
}

// sameID reports whether two JSON-RPC ids are the same value, whatever
// their spelling ("a" and "a", 1 and 1.0).
func sameID(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return x == y
}
