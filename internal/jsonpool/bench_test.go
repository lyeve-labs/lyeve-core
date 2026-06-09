package jsonpool

import (
	"bytes"
	"encoding/json"
	"testing"
)

// Buffer Pool Benchmarks

func BenchmarkGetBuffer(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		buf := GetBuffer()
		buf.WriteString("hello")
		Buffer(buf)
	}
}

func BenchmarkNewBuffer(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		buf := bytes.NewBuffer(make([]byte, 0, 4096))
		buf.WriteString("hello")
		_ = buf
	}
}

// MarshalJSON Benchmarks

type smallPayload struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Count  int    `json:"count"`
}

var marshalPayload = smallPayload{
	ID:     "550e8400-e29b-41d4-a716-446655440000",
	Status: "published",
	Count:  42,
}

func BenchmarkMarshalJSON(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := MarshalJSON(marshalPayload)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStdMarshal(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := json.Marshal(marshalPayload)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// WriteJSON Benchmark

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

func BenchmarkWriteJSON(b *testing.B) {
	var w nopWriter
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = WriteJSON(w, marshalPayload)
	}
}

func BenchmarkStdNewEncoder(b *testing.B) {
	var w nopWriter
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = json.NewEncoder(w).Encode(marshalPayload)
	}
}

func TestMarshalJSONRoundtrip(t *testing.T) {
	payload := smallPayload{ID: "test", Status: "draft", Count: 1}
	raw, err := MarshalJSON(payload)
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	var back smallPayload
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back != payload {
		t.Errorf("roundtrip mismatch: %+v != %+v", back, payload)
	}
}

func TestBufferPoolSizes(t *testing.T) {
	buf := GetBuffer()
	if buf.Cap() < 4096 {
		t.Errorf("pooled buffer cap too small: %d", buf.Cap())
	}
	Buffer(buf)

	// Very large buffers should NOT be pooled
	big := GetBuffer()
	big.Grow(131072) // grow to 128KB+
	Buffer(big)

	// Next GetBuffer should be fresh (big buffer wasn't pooled)
	buf2 := GetBuffer()
	if buf2.Cap() >= 65536 {
		t.Errorf("big buffer was pooled; expected fresh: cap=%d", buf2.Cap())
	}
	Buffer(buf2)
}
