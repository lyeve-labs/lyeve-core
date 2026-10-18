// Package panicreport hands every panic the engine recovers to one reporter.
//
// The request recoverers and the goroutine spawners in pkg/engine each catch
// a panic and log it. Logging alone leaves the panic on stderr, so the
// runtime installs a reporter here that hands it to the plugins holding the
// panic reporter role, and every recovery point calls Report. The package is
// internal so that only the runtime installs the reporter: a plugin cannot
// redirect or silence the engine's panics. It sits below pkg/core, which
// imports pkg/engine, so Panic mirrors core.RecoveredPanic field for field.
package panicreport

import (
	"fmt"
	"log/slog"
	"sync/atomic"
)

// Panic is one recovered panic. Its fields are those of core.RecoveredPanic.
type Panic struct {
	TenantID  string
	Method    string
	Route     string
	Goroutine string
	Value     any
	Stack     []byte
}

// Reporter receives one recovered panic.
type Reporter func(p Panic)

var current atomic.Pointer[Reporter]

// Set installs the reporter every recovered panic is handed to. Nil removes it.
func Set(r Reporter) {
	if r == nil {
		current.Store(nil)
		return
	}
	current.Store(&r)
}

// Report hands a recovered panic to the installed reporter, if there is one.
//
// It runs inside a deferred recover, where a second panic would end the
// process, so a reporter that panics is recovered and logged here.
func Report(p Panic) {
	r := current.Load()
	if r == nil {
		return
	}
	defer func() {
		if rp := recover(); rp != nil {
			slog.Error("panic reporter panicked",
				"route", p.Route, "goroutine", p.Goroutine, "panic", fmt.Sprintf("%v", rp))
		}
	}()
	(*r)(p)
}
