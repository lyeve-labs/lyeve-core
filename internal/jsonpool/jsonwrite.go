package jsonpool

import (
	"io"

	"github.com/bytedance/sonic"
)

// WriteJSON encodes v to w using sonic's streaming encoder, which writes
// directly to w without an intermediate []byte allocation. At 10K req/s with 5KB responses, this avoids
// ~50 MB/s of garbage.
//
// Callers writing to http.ResponseWriter commonly suppress the returned error
// (_ = jsonpool.WriteJSON(w, body)) because an HTTP write failure means the client
// disconnected: there is no recovery path. This is safe.
func WriteJSON(w io.Writer, v any) error {
	return encoding.NewEncoder(w).Encode(v)
}

// encoding is sonic's fastest configuration with map keys sorted. A content
// entry's fields are a map, and the fastest configuration writes a map in Go's
// random iteration order, so one unchanged entry came out as different bytes on
// each read. The read cache tags a response with a hash of those bytes, so the
// tag changed every time and If-None-Match never matched. Sorted keys make the
// same value encode to the same bytes.
var encoding = sonic.Config{
	NoValidateJSONMarshaler: true,
	NoValidateJSONSkip:      true,
	SortMapKeys:             true,
}.Froze()
