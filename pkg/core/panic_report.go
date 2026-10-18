package core

// RecoveredPanic is one panic the engine recovered while keeping the process
// up: in a request handler, a plugin's included, or in a goroutine the engine
// spawned.
//
// It carries no request body, header, query string, path value or
// credential. Route is the pattern the router matched, so a token that sits
// in a URL never reaches a reporter.
type RecoveredPanic struct {
	// TenantID is the tenant the request resolved to, or "" for a goroutine
	// and for a request that panicked before its tenant was resolved.
	TenantID string
	// Method is the request method, or "" for a goroutine.
	Method string
	// Route is the route pattern the request matched, such as
	// /api/v1/content/{schema}/{id}, or "" for a goroutine and for a request
	// that matched no pattern.
	Route string
	// Goroutine names the goroutine that panicked, or "" for a request.
	Goroutine string
	// Value is the value passed to panic.
	Value any
	// Stack is the panicking goroutine's stack, read inside the recover.
	Stack []byte
}

// PanicReporter is implemented by a plugin that is told of every panic the
// engine recovers, such as error tracking. The engine calls each running
// plugin that implements it from inside the recover, before a request's 500
// is written or a goroutine exits. ReportPanic must return promptly, so a
// reporter that stores the panic queues the write, and it must be safe for
// concurrent use. A reporter that keeps the panic past the call formats
// Value first, because it may reference the request's memory. A panic inside
// ReportPanic is recovered and logged.
type PanicReporter interface {
	ReportPanic(p RecoveredPanic)
}
