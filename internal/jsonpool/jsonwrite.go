package jsonpool

import (
	"io"

	json "github.com/goccy/go-json"
)

// WriteJSON encodes v to w using a streaming encoder, which writes directly to
// w without an intermediate []byte allocation. At 10K req/s with 5KB responses,
// this avoids ~50 MB/s of garbage.
//
// Map keys are sorted. A content entry's fields are a map, and an encoder that
// writes a map in Go's random iteration order makes one unchanged entry come
// out as different bytes on each read. The read cache tags a response with a
// hash of those bytes, so the tag would change every time and If-None-Match
// would never match. goccy/go-json sorts map keys by default, so the same value
// encodes to the same bytes.
//
// HTML escaping is off, so <, > and & stay literal in a response. The engine
// has always written its JSON that way, and httpx.ErrorCode matches it so a
// message reads the same from either path.
//
// Callers writing to http.ResponseWriter commonly suppress the returned error
// (_ = jsonpool.WriteJSON(w, body)) because an HTTP write failure means the client
// disconnected: there is no recovery path. This is safe.
func WriteJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}
