package spool

import "testing"

func TestCanonicalHashStability(t *testing.T) {
	// Same payload must produce same hash.
	payload := []byte(`{"b":2,"a":1}`)
	h1, err := CanonicalHash(payload)
	if err != nil {
		t.Fatalf("CanonicalHash: %v", err)
	}
	h2, err := CanonicalHash(payload)
	if err != nil {
		t.Fatalf("CanonicalHash second: %v", err)
	}
	if h1 != h2 {
		t.Errorf("hash not stable: %s != %s", h1, h2)
	}
}

func TestCanonicalHashKeyOrder(t *testing.T) {
	// Different key order must produce same hash.
	h1, err := CanonicalHash([]byte(`{"a":1,"b":2}`))
	if err != nil {
		t.Fatal(err)
	}
	h2, err := CanonicalHash([]byte(`{"b":2,"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Errorf("key order mismatch: %s != %s", h1, h2)
	}
}

func TestCanonicalHashWhitespace(t *testing.T) {
	// Whitespace differences must produce same hash.
	h1, err := CanonicalHash([]byte(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	h2, err := CanonicalHash([]byte(`{  "a" : 1  }`))
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Errorf("whitespace mismatch: %s != %s", h1, h2)
	}
}

func TestCanonicalHashDifferentPayloads(t *testing.T) {
	h1, err := CanonicalHash([]byte(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	h2, err := CanonicalHash([]byte(`{"a":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h2 {
		t.Error("different payloads produced same hash")
	}
}

func TestCanonicalHashLength(t *testing.T) {
	h, err := CanonicalHash([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	// blake3-256 -> 32 bytes -> 64 hex chars
	if len(h) != 64 {
		t.Errorf("hash length: got %d, want 64", len(h))
	}
}

func TestCanonicalHashNestedJSON(t *testing.T) {
	// Nested objects with different key orders.
	h1, err := CanonicalHash([]byte(`{"outer":{"b":2,"a":1},"x":true}`))
	if err != nil {
		t.Fatal(err)
	}
	h2, err := CanonicalHash([]byte(`{"x":true,"outer":{"a":1,"b":2}}`))
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Errorf("nested key order mismatch: %s != %s", h1, h2)
	}
}

func TestCanonicalHashInvalidJSON(t *testing.T) {
	_, err := CanonicalHash([]byte(`not json`))
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}
