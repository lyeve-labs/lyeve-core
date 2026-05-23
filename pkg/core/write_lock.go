package core

import "context"

// WriteSerializer is implemented by a host that can run a write under an
// exclusive lock every replica of the install honors. It is what keeps a
// ceiling a ceiling: a count read before an insert lets two writes at the
// limit both pass, and a count read under the lock cannot.
//
// fn runs inside one transaction on one connection, and the lock lasts until
// that transaction commits or rolls back. Every query fn makes through
// Querier with the context it is handed runs in that transaction, so a store
// needs no change to take part. fn must not open a transaction of its own,
// and a Begin inside it is refused. fn's error rolls everything back and is
// returned as it is. A lock that cannot be had in time wraps
// ErrServiceUnavailable, which httpx.StatusFor and httpx.StoreStatusFor both
// answer as 503.
//
// Only Querier with the context fn is handed reaches the transaction. These
// run outside the lock, on connections of their own, and see nothing fn has
// not committed:
//
//   - QuerierRO, which may also read a replica
//   - a connection taken from the pool directly
//   - the admin querier
//   - a goroutine fn starts, and any query made with a context other than
//     the one fn is handed
//
// So the count a ceiling checks inside fn must read through
// host.Querier(ctx), with that ctx. A count read any other way is the
// unserialized count the lock exists to replace.
//
// key names what is serialized and the host adds nothing to it. Name it by
// the owner and the scope, such as "flow.flows:" plus the tenant for a
// per-tenant ceiling, so two plugins never share a lock by accident.
type WriteSerializer interface {
	SerializeWrite(ctx context.Context, key string, fn func(ctx context.Context) error) error
}

// SerializeWrite runs fn under host's write lock named key. A host that
// cannot take one runs fn directly, which only a host built outside the
// runtime does: the engine host and the plugintest hosts all serialize.
func SerializeWrite(ctx context.Context, host any, key string, fn func(ctx context.Context) error) error {
	if s, ok := host.(WriteSerializer); ok {
		return s.SerializeWrite(ctx, key, fn)
	}
	return fn(ctx)
}

// SerializeWrite forwards to inner if it can serialize, and runs fn directly
// otherwise. The lock grants nothing a plugin could not already do with its
// own writes, so no capability gates it.
func (h *ScopedHost) SerializeWrite(ctx context.Context, key string, fn func(ctx context.Context) error) error {
	return SerializeWrite(ctx, h.inner, key, fn)
}
