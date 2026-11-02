package logging_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/lyeve-labs/lyeve-core/internal/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// chi pools its route context and hands it to the next request once a
// request returns. A line logged after its request, such as one from an
// async hook, must not read that context: under -race the read is a data
// race, and without it the line names another request's route.
func TestEnrichHandler_LineAfterTheRequestReadsNoPooledRoute(t *testing.T) {
	var mu sync.Mutex
	var buf bytes.Buffer
	logger := slog.New(logging.NewEnrichHandler(slog.NewJSONHandler(lockedWriter{&mu, &buf}, nil), nil))

	const routes, requests, workers = 20, 200, 8
	var logged sync.WaitGroup
	router := chi.NewRouter()
	for i := 0; i < routes; i++ {
		pattern := fmt.Sprintf("/r%d/{id}", i)
		router.Get(pattern, func(_ http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			returned := ctx.Value(returnedKey{}).(chan struct{})
			logged.Add(1)
			go func() {
				defer logged.Done()
				<-returned
				for j := 0; j < 5; j++ {
					logger.InfoContext(ctx, "after the request", "own_route", pattern)
				}
			}()
		})
	}

	jobs := make(chan int)
	var served sync.WaitGroup
	for w := 0; w < workers; w++ {
		served.Add(1)
		go func() {
			defer served.Done()
			for n := range jobs {
				target := fmt.Sprintf("/r%d/%d", n%routes, n)
				returned := make(chan struct{})
				req := httptest.NewRequest(http.MethodGet, target, nil)
				req = req.WithContext(context.WithValue(req.Context(), returnedKey{}, returned))
				router.ServeHTTP(httptest.NewRecorder(), req)
				// chi has put the route context back in its pool by now,
				// and this worker's next request takes it again.
				close(returned)
			}
		}()
	}
	for n := 0; n < requests; n++ {
		jobs <- n
	}
	close(jobs)
	served.Wait()
	logged.Wait()

	dec := json.NewDecoder(&buf)
	lines, wrong := 0, 0
	for dec.More() {
		var line map[string]any
		require.NoError(t, dec.Decode(&line))
		lines++
		if route, ok := line["route"]; ok && route != line["own_route"] {
			wrong++
		}
	}
	require.Equal(t, requests*5, lines)
	assert.Zero(t, wrong, "%d of %d lines named another request's route", wrong, lines)
}

type returnedKey struct{}

type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
