package core

import (
	"context"
	"errors"
	"testing"
)

// fakeInvoker records what a transport asked for and answers a fixed run.
type fakeInvoker struct {
	slugs    []string
	surfaces []string
	inputs   []map[string]any
	fail     error
}

func (f *fakeInvoker) Invoke(ctx context.Context, slug, surface string, input map[string]any) (*FlowInvokeResult, error) {
	if TenantIDFromCtx(ctx) == "" {
		return nil, ErrTenantRequired
	}
	f.slugs = append(f.slugs, slug)
	f.surfaces = append(f.surfaces, surface)
	f.inputs = append(f.inputs, input)
	if f.fail != nil {
		return nil, f.fail
	}
	return &FlowInvokeResult{RunID: "run-1", Status: 200, Headers: map[string]string{"X-Flow-Cache": "miss"}, Body: map[string]any{"ok": true}}, nil
}

func (f *fakeInvoker) Published(ctx context.Context) ([]FlowSummary, error) {
	if TenantIDFromCtx(ctx) == "" {
		return nil, ErrTenantRequired
	}
	return []FlowSummary{{Slug: "order-totals", Name: "Order totals", Auth: "auth"}}, nil
}

var _ FlowInvoker = (*fakeInvoker)(nil)

// invokerHost is a stubHost that can hold an invoker, the way the engine
// host does once the plugin that runs flows has registered one.
type invokerHost struct {
	*stubHost
	invoker FlowInvoker
}

func (h *invokerHost) RegisterFlowInvoker(inv FlowInvoker) { h.invoker = inv }
func (h *invokerHost) FlowInvoker() FlowInvoker            { return h.invoker }

// The plugin that runs flows registers its invoker through its scoped host
// and a transport plugin fetches the same one through its own. The arguments
// and the result pass through untouched, and clearing the registration is
// what the transport sees as nil.
func TestScopedHost_FlowInvoker_RegisteredByFlowReachesTransport(t *testing.T) {
	inner := &invokerHost{stubHost: &stubHost{}}
	flow := NewScopedHost(inner, "flow", CapAll)
	graphql := NewScopedHost(inner, "graphql", CapDBWrite|CapRoutes)

	fake := &fakeInvoker{}
	var reg Host = flow
	r, ok := reg.(FlowInvokerRegistrar)
	if !ok {
		t.Fatal("ScopedHost must satisfy FlowInvokerRegistrar through the Host interface")
	}
	r.RegisterFlowInvoker(fake)

	var host Host = graphql
	p, ok := host.(FlowInvokerProvider)
	if !ok {
		t.Fatal("ScopedHost must satisfy FlowInvokerProvider through the Host interface")
	}
	got := p.FlowInvoker()
	if got != FlowInvoker(fake) {
		t.Fatalf("FlowInvoker() = %T, want the registered invoker", got)
	}

	ctx := WithTenantID(context.Background(), "acme")
	res, err := got.Invoke(ctx, "order-totals", "graphql", map[string]any{"id": 7})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.RunID != "run-1" || res.Status != 200 || res.Headers["X-Flow-Cache"] != "miss" {
		t.Errorf("result did not pass through: %+v", res)
	}
	if fake.slugs[0] != "order-totals" || fake.surfaces[0] != "graphql" || fake.inputs[0]["id"] != 7 {
		t.Errorf("arguments did not pass through: %v %v %v", fake.slugs, fake.surfaces, fake.inputs)
	}
	list, err := got.Published(ctx)
	if err != nil || len(list) != 1 || list[0].Slug != "order-totals" {
		t.Errorf("Published = %v, %v", list, err)
	}

	flow.RegisterFlowInvoker(nil)
	if graphql.FlowInvoker() != nil {
		t.Error("after RegisterFlowInvoker(nil) the invoker must be nil")
	}
}

// The sentinel errors are what a transport maps to its own status codes, so
// they must survive wrapping.
func TestFlowInvoker_SentinelsSurviveWrapping(t *testing.T) {
	inner := &invokerHost{stubHost: &stubHost{}}
	flow := NewScopedHost(inner, "flow", CapAll)
	grpc := NewScopedHost(inner, "grpc", CapRoutes)
	fake := &fakeInvoker{fail: ErrFlowRateLimited}
	flow.RegisterFlowInvoker(fake)

	_, err := grpc.FlowInvoker().Invoke(WithTenantID(context.Background(), "acme"), "x", "grpc", nil)
	if !errors.Is(err, ErrFlowRateLimited) {
		t.Fatalf("err = %v, want ErrFlowRateLimited", err)
	}
	_, err = fake.Invoke(context.Background(), "x", "grpc", nil)
	if !errors.Is(err, ErrTenantRequired) {
		t.Fatalf("a call without a tenant must be refused, got %v", err)
	}
}

// A bare host answers nil and logs once, with the op named, so a transport
// on an install where no plugin runs flows exposes nothing and says why once.
func TestScopedHost_FlowInvoker_NilWhenInnerCannotProvide(t *testing.T) {
	h := newCapturingHost()
	sh := NewScopedHost(h, "graphql", CapAll)
	for range 3 {
		if got := sh.FlowInvoker(); got != nil {
			t.Fatalf("FlowInvoker() = %v, want nil", got)
		}
	}
	recs := unavailableRecords(t, h)
	if len(recs) != 1 {
		t.Fatalf("want exactly 1 unavailable log record, got %d: %q", len(recs), h.buf.String())
	}
	assertField(t, recs[0], "op", "FlowInvoker")
}
