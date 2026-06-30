package spool

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/zeebo/blake3"
)

// CanonicalHash returns a deterministic blake3-hex (32 bytes -> 64 hex chars)
// fingerprint of the JSON payload. Two payloads that differ only in key order
// or whitespace produce the same hash.
//
// Algorithm: parse -> re-marshal with sorted object keys + no whitespace ->
// blake3-256 -> hex. Non-object/array roots (string, number, bool, null) hash
// as-is after parse+remarshal (json.Marshal is deterministic for scalars).
func CanonicalHash(payload []byte) (string, error) {
	canon, err := canonicalizeJSON(payload)
	if err != nil {
		return "", fmt.Errorf("canonicalize: %w", err)
	}
	sum := blake3.Sum256(canon)
	return hex.EncodeToString(sum[:]), nil
}

// canonicalizeJSON parses arbitrary JSON and re-emits it with sorted object
// keys, no insignificant whitespace, and no trailing newline.
func canonicalizeJSON(payload []byte) ([]byte, error) {
	var v any
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber() // preserve numeric precision
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := writeCanonical(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeCanonical(buf *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if x {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case string:
		b, err := json.Marshal(x)
		if err != nil {
			return err
		}
		buf.Write(b)
	case json.Number:
		buf.WriteString(x.String())
	case []any:
		buf.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, item); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			kb, err := json.Marshal(k)
			if err != nil {
				return err
			}
			buf.Write(kb)
			buf.WriteByte(':')
			if err := writeCanonical(buf, x[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return err
		}
		buf.Write(b)
	}
	return nil
}
