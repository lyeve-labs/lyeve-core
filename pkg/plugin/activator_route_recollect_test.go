package plugin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// licenseGatedPlugin is shaped like a plugin whose Routes() binds a method
// value on whatever handler the plugin holds when it is asked, while a license
// arriving later replaces that handler.
type licenseGatedPlugin struct {
	name string

	mu      sync.Mutex
	handler *gatedHandler
}

type gatedHandler struct{ degraded bool }

func (h *gatedHandler) List(w http.ResponseWriter, _ *http.Request) {
	if h.degraded {
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte("degraded"))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("active"))
}

func (p *licenseGatedPlugin) Name() string { return p.name }

func (p *licenseGatedPlugin) Start(context.Context, core.Host) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.handler = &gatedHandler{degraded: true}
	return nil
}

func (p *licenseGatedPlugin) Stop(context.Context) error { return nil }

// activate is what the plugin's own license.changed handler does: a new
// handler, assigned to the field the route was bound to.
func (p *licenseGatedPlugin) activate() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.handler = &gatedHandler{degraded: false}
}

func (p *licenseGatedPlugin) Routes() []RouteDecl {
	p.mu.Lock()
	defer p.mu.Unlock()
	return []RouteDecl{{
		Method:  http.MethodGet,
		Pattern: "/api/admin/gated/items",
		Handler: http.HandlerFunc(p.handler.List),
		Group:   core.GroupAdmin,
	}}
}

func serve(t *testing.T, h http.Handler) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/admin/gated/items", nil))
	return rec.Code, rec.Body.String()
}

// The route the router mounted holds a method value, and a method value
// captures its receiver when it is built. Collected at boot from a plugin that
// has no license, it captures the degraded stub, so activation replaces a field
// the mounted route no longer looks at.
func TestRecollectRoutes_RepointsAMountedRouteAtTheNewHandler(t *testing.T) {
	a := NewActivator(testHost{}, nil)
	a.logger = silentLogger

	p := &licenseGatedPlugin{name: "gated"}
	if err := p.Start(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	a.recordRunning("gated", p)
	if err := a.collectRoutes("gated", p.Routes()); err != nil {
		t.Fatal(err)
	}

	booted := a.CollectedRoutes()
	if len(booted) != 1 || len(booted[0].Routes) != 1 {
		t.Fatalf("expected one collected route, got %#v", booted)
	}
	if code, body := serve(t, booted[0].Routes[0].Handler); code != http.StatusPaymentRequired {
		t.Fatalf("an unlicensed boot should mount the degraded handler, got %d %q", code, body)
	}

	// The license arrives. The plugin rebuilds its handler. The handler already
	// collected does not change, which is why re-collection exists.
	p.activate()
	if code, _ := serve(t, booted[0].Routes[0].Handler); code != http.StatusPaymentRequired {
		t.Fatal("the collected handler was expected to stay bound to the stub; " +
			"if this fails the test tests nothing")
	}

	fresh := a.RecollectRoutes()
	if len(fresh) != 1 || len(fresh[0].Routes) != 1 {
		t.Fatalf("expected one re-collected route, got %#v", fresh)
	}
	if code, body := serve(t, fresh[0].Routes[0].Handler); code != http.StatusOK {
		t.Errorf("the re-collected route answers %d %q; want 200 from the activated handler", code, body)
	}

	// And the activator's own table is what the next router build reads.
	after := a.CollectedRoutes()
	if code, body := serve(t, after[0].Routes[0].Handler); code != http.StatusOK {
		t.Errorf("the collected table still answers %d %q after re-collection", code, body)
	}
}

// The router is rebuilt from the callback, so a re-collection that does not
// fire it leaves the new handlers in the activator and the old ones mounted.
func TestRecollectRoutes_NotifiesSoTheRouterIsRebuilt(t *testing.T) {
	a := NewActivator(testHost{}, nil)
	a.logger = silentLogger

	p := &licenseGatedPlugin{name: "gated"}
	if err := p.Start(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	a.recordRunning("gated", p)
	if err := a.collectRoutes("gated", p.Routes()); err != nil {
		t.Fatal(err)
	}

	var got [][]PluginRoutes
	a.SetRoutesChangeCallback(func(routes []PluginRoutes) {
		got = append(got, routes)
	})

	p.activate()
	a.RecollectRoutes()

	if len(got) != 1 {
		t.Fatalf("expected one route-change notification, got %d", len(got))
	}
	if code, body := serve(t, got[0][0].Routes[0].Handler); code != http.StatusOK {
		t.Errorf("the notification carried a handler answering %d %q; want the activated one", code, body)
	}
}

// A plugin that panics building its route table must not take the license
// event down with it, and must not lose the other plugins' routes.
func TestRecollectRoutes_SurvivesAPanickingRoutesCall(t *testing.T) {
	a := NewActivator(testHost{}, nil)
	a.logger = silentLogger

	good := &licenseGatedPlugin{name: "gated"}
	if err := good.Start(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	a.recordRunning("gated", good)
	if err := a.collectRoutes("gated", good.Routes()); err != nil {
		t.Fatal(err)
	}

	bad := &panickingRoutesPlugin{name: "brittle"}
	a.recordRunning("brittle", bad)
	if err := a.collectRoutes("brittle", []RouteDecl{{
		Method: http.MethodGet, Pattern: "/api/admin/brittle", Handler: http.NotFoundHandler(), Group: core.GroupAdmin,
	}}); err != nil {
		t.Fatal(err)
	}

	good.activate()
	fresh := a.RecollectRoutes()

	var seen []string
	for _, pr := range fresh {
		seen = append(seen, pr.Name)
	}
	if len(seen) != 1 || seen[0] != "gated" {
		t.Fatalf("expected only the sound plugin to re-collect, got %v", seen)
	}
	if code, _ := serve(t, fresh[0].Routes[0].Handler); code != http.StatusOK {
		t.Errorf("the sound plugin did not re-collect around the panicking one")
	}
}

type panickingRoutesPlugin struct{ name string }

func (p *panickingRoutesPlugin) Name() string                           { return p.name }
func (p *panickingRoutesPlugin) Start(context.Context, core.Host) error { return nil }
func (p *panickingRoutesPlugin) Stop(context.Context) error             { return nil }
func (p *panickingRoutesPlugin) Routes() []RouteDecl                    { panic("route table is not built yet") }

// A stopped plugin whose routes have not yet been stripped must not be asked
// to build a route table: it has been torn down, and the two maps are updated
// separately.
func TestRecollectRoutes_SkipsAPluginThatIsNotRunning(t *testing.T) {
	a := NewActivator(testHost{}, nil)
	a.logger = silentLogger

	p := &licenseGatedPlugin{name: "gated"}
	if err := p.Start(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	a.recordRunning("gated", p)
	if err := a.collectRoutes("gated", p.Routes()); err != nil {
		t.Fatal(err)
	}

	a.mu.Lock()
	a.active["gated"].phase = PhaseStopped
	a.mu.Unlock()

	if fresh := a.RecollectRoutes(); len(fresh) != 0 {
		t.Errorf("re-collected %d plugin(s) from a stopped one; want none", len(fresh))
	}
}
