// Package api provides the LyEve CMS HTTP API layer.
//
// SwappableHandler wraps an http.Handler and supports atomic replacement of
// the underlying handler at runtime. Used by the dev-mode hot-reload system
// to update plugin routes without restarting the server.
package api

import (
	"net/http"
	"sync"
)

// SwappableHandler is an http.Handler that can atomically swap its delegate
// handler. Reads are lock-free (RWMutex RLock). Writes take an exclusive lock.
//
// Typical usage in runtime:
//
//	swapper := api.NewSwappableHandler(router)
//	apiSrv.Handler = swapper
//
//	activator.SetRoutesChangeCallback(func(routes []plugin.PluginRoutes) {
//	    swapper.Swap(mountFunc(routes)) // rebuild chi router with new routes
//	})
type SwappableHandler struct {
	mu sync.RWMutex
	h  http.Handler
}

// NewSwappableHandler returns a SwappableHandler that delegates to h.
func NewSwappableHandler(h http.Handler) *SwappableHandler {
	return &SwappableHandler{h: h}
}

// ServeHTTP implements http.Handler.
func (s *SwappableHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	h := s.h
	s.mu.RUnlock()
	h.ServeHTTP(w, r)
}

// Swap atomically replaces the underlying handler. Safe to call from any
// goroutine while the server is serving requests.
func (s *SwappableHandler) Swap(h http.Handler) {
	s.mu.Lock()
	s.h = h
	s.mu.Unlock()
}

// Current returns the handler requests are delegated to now.
func (s *SwappableHandler) Current() http.Handler {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.h
}
