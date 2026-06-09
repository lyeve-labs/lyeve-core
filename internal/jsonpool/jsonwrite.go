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
	return sonic.ConfigFastest.NewEncoder(w).Encode(v)
}
