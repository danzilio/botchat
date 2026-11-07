package install

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// object is a JSON object that keeps its keys in their original order, so
// rewriting a user's config file doesn't shuffle it (INS-4). Values are kept
// as raw JSON.
type object []member

type member struct {
	Key   string
	Value json.RawMessage
}

// parseObject parses a JSON object. Empty (or whitespace-only) input is an
// empty object. Anything else that isn't an object, including null, is an
// error, and so are duplicate keys, since decoders disagree on which wins.
func parseObject(b []byte) (object, error) {
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return object{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if tok != json.Delim('{') {
		return nil, errors.New("not a JSON object")
	}
	o := object{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("invalid JSON: %w", err)
		}
		key, ok := tok.(string)
		if !ok {
			return nil, errors.New("invalid JSON: expected a key")
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, fmt.Errorf("invalid JSON: %w", err)
		}
		if _, dup := o.get(key); dup {
			return nil, fmt.Errorf("duplicate key %q", key)
		}
		o = append(o, member{key, v})
	}
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("invalid JSON: data after the object")
	}
	return o, nil
}

func (o object) get(key string) (json.RawMessage, bool) {
	for _, m := range o {
		if m.Key == key {
			return m.Value, true
		}
	}
	return nil, false
}

// set replaces key's value in place, or appends it.
func (o object) set(key string, v json.RawMessage) object {
	for i, m := range o {
		if m.Key == key {
			o[i].Value = v
			return o
		}
	}
	return append(o, member{key, v})
}

func (o object) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, m := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, err := marshal(m.Key)
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		b.Write(m.Value)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}
