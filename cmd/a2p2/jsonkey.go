package main

import (
	"bytes"
	"encoding/json"
	"errors"
)

// withoutKey is the JSON object raw with its top-level member key left
// out. The other members keep their order and their bytes, so the output
// is the daemon's response less that one member.
func withoutKey(raw []byte, key string) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, errors.New("status: the daemon's answer is not a JSON object")
	}
	var out bytes.Buffer
	out.WriteByte('{')
	first := true
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		name, _ := t.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		if name == key {
			continue
		}
		if !first {
			out.WriteByte(',')
		}
		first = false
		k, _ := json.Marshal(name)
		out.Write(k)
		out.WriteByte(':')
		out.Write(v)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	out.WriteString("}\n")
	return out.Bytes(), nil
}
