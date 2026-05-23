package api

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/httpx"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
	"github.com/lyeve-labs/lyeve-core/pkg/reqparse"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

// The refusals an admin token can meet. Each is static: none carries a
// token's value, and the grant one names only a catalog entry.
const (
	msgAdminTokenOffAdminAPI = "Admin tokens are accepted on the admin API only."
	msgAdminTokenInvalid     = "This admin token is not valid."
	msgAdminTokenRevoked     = "This admin token has been revoked."
	msgAdminTokenExpired     = "This admin token has expired."
	msgAdminTokenOwner       = "The owner of this admin token can no longer use it."
	msgAdminTokenTenant      = "An admin token acts only in the tenant it was issued for."
	msgAdminTokenAddress     = "This admin token is not allowed from this address."
	msgAdminTokenSessionOnly = "This route needs a signed-in session."
	msgAdminTokenCheckFailed = "admin token check unavailable"
)

// isAdminTokenBearer reports whether the request presents an admin token.
func isAdminTokenBearer(r *http.Request) bool {
	return strings.HasPrefix(bearerToken(r), core.AdminTokenPrefix)
}

// refuseAdminTokens answers 401 to any request presenting an admin token.
// Mounted on every router but the admin one: without it the token would read
// as no credential at all, so a public route would answer it as an anonymous
// caller and a client would never learn it sent the wrong credential.
func refuseAdminTokens(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isAdminTokenBearer(r) {
			httpx.ErrorReq(w, r, http.StatusUnauthorized, msgAdminTokenOffAdminAPI)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// adminTokenCaller is what the token middleware leaves on the context for
// the grant check and the request log.
type adminTokenCaller struct {
	token   *core.AdminToken
	pattern string
}

type adminTokenCtxKey struct{}

func adminTokenFromCtx(ctx context.Context) *adminTokenCaller {
	c, _ := ctx.Value(adminTokenCtxKey{}).(*adminTokenCaller)
	return c
}

// adminTokenUsers is the part of the user store token authentication reads.
type adminTokenUsers interface {
	GetByID(ctx context.Context, id uuid.UUID) (*domain.User, error)
}

// adminTokenAuth authenticates admin tokens on the admin router.
type adminTokenAuth struct {
	tokens  core.AdminTokenStore // nil without a database: every token is refused
	users   adminTokenUsers
	members core.MembershipReader // nil when memberships are not wired
	trusted []*net.IPNet
	log     *adminTokenLog
	routes  routeFinder // the router the token is used on, for route patterns
	now     func() time.Time
}

// routeFinder answers which pattern a router serves a method and path under.
// *chi.Mux is one.
type routeFinder interface {
	Find(rctx *chi.Context, method, path string) string
}

// routeFinderOf returns h as a routeFinder. Without one no token request
// could be matched to its grant, so the admin router refuses to build.
func routeFinderOf(h any) (routeFinder, error) {
	f, ok := h.(routeFinder)
	if !ok {
		return nil, fmt.Errorf("admin router: %T cannot resolve route patterns for admin tokens", h)
	}
	return f, nil
}

// routePattern is the pattern the router will serve the request under, or ""
// when no route matches. A HEAD request is served by the GET route.
func (a *adminTokenAuth) routePattern(r *http.Request) string {
	return routePatternOf(a.routes, r)
}

// routePatternOf is the pattern mux serves the request under, or "" when no
// route matches or there is no mux. It asks the routing tree rather than the
// request, so it answers the same before routing as after. A HEAD request is
// served by the GET route.
func routePatternOf(mux routeFinder, r *http.Request) string {
	if mux == nil {
		return ""
	}
	path := r.URL.RawPath
	if path == "" {
		path = r.URL.Path
	}
	if p := mux.Find(chi.NewRouteContext(), r.Method, path); p != "" {
		return p
	}
	if r.Method == http.MethodHead {
		return mux.Find(chi.NewRouteContext(), http.MethodGet, path)
	}
	return ""
}

// authenticate reads an admin token from the Authorization header. A request
// without one passes untouched. A token the engine cannot honor is refused
// here, never passed on as an anonymous request. One that is honored reaches
// the rest of the chain as its owner, with the owner's current roles in the
// token's tenant. The grant check further down decides which routes it may
// call. Every request made with a recognized token is logged, refusals
// included.
func (a *adminTokenAuth) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := bearerToken(r)
		if !strings.HasPrefix(raw, core.AdminTokenPrefix) {
			next.ServeHTTP(w, r)
			return
		}
		if a.tokens == nil {
			httpx.ErrorReq(w, r, http.StatusUnauthorized, msgAdminTokenInvalid)
			return
		}
		tok, err := a.tokens.GetByHash(r.Context(), security.HashKeyPeppered(raw))
		if err != nil {
			// The store is supplied by a plugin, so its contract is
			// core.ErrNotFound rather than the engine's own sentinel.
			if errors.Is(err, core.ErrNotFound) {
				httpx.ErrorReq(w, r, http.StatusUnauthorized, msgAdminTokenInvalid)
				return
			}
			slog.WarnContext(r.Context(), "admin token lookup failed", "err", err)
			httpx.ErrorReq(w, r, httpx.StoreStatusFor(err), msgAdminTokenCheckFailed)
			return
		}

		caller := &adminTokenCaller{token: tok, pattern: a.routePattern(r)}
		ip := reqparse.ClientIPTrusted(r, a.trusted)
		rec := &statusCapture{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			// A handler that panics has written nothing yet. The recoverer
			// further out answers 500, so that is what the log records. The
			// panic carries on to it.
			p := recover()
			if p != nil && !rec.wroteHeader {
				rec.status = http.StatusInternalServerError
			}
			a.log.request(core.AdminTokenRequest{
				TokenID:      tok.ID,
				TenantID:     tok.TenantID,
				Method:       r.Method,
				RoutePattern: caller.pattern,
				Status:       rec.status,
				ClientIP:     ip,
				CreatedAt:    a.now(),
			})
			if p != nil {
				panic(p)
			}
		}()

		claims, status, msg := a.admit(r, tok, ip)
		if status != 0 {
			httpx.ErrorReq(rec, r, status, msg)
			return
		}
		a.log.touch(tok, a.now())
		ctx := context.WithValue(r.Context(), core.ClaimsKey, claims)
		ctx = context.WithValue(ctx, adminTokenCtxKey{}, caller)
		next.ServeHTTP(rec, r.WithContext(ctx))
	})
}

// admit decides whether tok may act on this request, and as whom. A zero
// status admits it.
func (a *adminTokenAuth) admit(r *http.Request, tok *core.AdminToken, ip string) (*core.AuthClaims, int, string) {
	ctx := r.Context()
	now := a.now()
	if tok.RevokedAt != nil {
		return nil, http.StatusUnauthorized, msgAdminTokenRevoked
	}
	if !now.Before(tok.ExpiresAt) {
		return nil, http.StatusUnauthorized, msgAdminTokenExpired
	}

	// The owner is read on every request. A token acts for its owner, so an
	// owner who is gone, disabled, past their account expiry or erased ends
	// it. Ending the owner's sessions does not: a logout or a password change
	// is about a person's browser, and a token is revoked on its own.
	owner, err := a.users.GetByID(ctx, tok.OwnerUserID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, http.StatusUnauthorized, msgAdminTokenOwner
		}
		slog.WarnContext(ctx, "admin token owner lookup failed", "err", err, "token_id", tok.ID)
		return nil, httpx.StoreStatusFor(err), msgAdminTokenCheckFailed
	}
	// An erased account keeps its row, marked anonymized, which the token
	// lookup read alongside the token.
	if owner.Disabled || (owner.ExpiresAt != nil && !now.Before(*owner.ExpiresAt)) || tok.OwnerErased {
		return nil, http.StatusUnauthorized, msgAdminTokenOwner
	}

	// A token names its tenant. A header naming another is refused whatever
	// the owner's role, so a super admin's token cannot be pointed elsewhere.
	if h := r.Header.Get("X-Tenant-ID"); h != "" && h != tok.TenantID { //nolint:raw-tenant-header // refuses a token's attempt to name another tenant before TenantHeader resolves one
		return nil, http.StatusForbidden, msgAdminTokenTenant
	}

	if len(tok.AllowedIPs) > 0 && !addressAllowed(ip, tok.AllowedIPs) {
		return nil, http.StatusForbidden, msgAdminTokenAddress
	}

	roles, err := ownerRolesIn(ctx, a.members, owner, tok.TenantID)
	if err != nil {
		if errors.Is(err, errNotAMember) {
			return nil, http.StatusUnauthorized, msgAdminTokenOwner
		}
		slog.WarnContext(ctx, "admin token owner membership lookup failed", "err", err, "token_id", tok.ID)
		return nil, httpx.StoreStatusFor(err), msgAdminTokenCheckFailed
	}
	// Only an admin of the tenant may own a token there, on every request:
	// an owner demoted below admin ends every token they own, not just the
	// admin routes.
	if !hasRole(roles, "admin") && !hasRole(roles, "super_admin") {
		return nil, http.StatusUnauthorized, msgAdminTokenOwner
	}
	return &core.AuthClaims{
		UserID:       owner.ID.String(),
		Email:        owner.Email,
		Roles:        tokenRoles(roles),
		TenantID:     tok.TenantID,
		TokenType:    "admin_token",
		AdminTokenID: tok.ID.String(),
	}, 0, ""
}

// tokenRoles are the roles a token acts with: the owner's, with super_admin
// read as admin. A token is bound to one tenant, and super_admin is the
// role that reaches across tenants and the install, so no token carries it.
// Its owner acts through it as an admin of the token's tenant.
func tokenRoles(roles []string) []string {
	out := make([]string, 0, len(roles))
	seen := map[string]bool{}
	for _, r := range roles {
		if r == "super_admin" {
			r = "admin"
		}
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	return out
}

// ownerRolesIn returns the roles u holds in tenant, by the rules a session
// acting in that tenant gets its roles: the home tenant from the account or
// its membership row, another tenant from its membership row, and every
// tenant for a super admin. errNotAMember when u may not act in tenant.
//
// A single-tenant account records no home tenant and acts in the default
// one, so the default tenant is its home.
func ownerRolesIn(ctx context.Context, members core.MembershipReader, u *domain.User, tenant string) ([]string, error) {
	h := &AuthHandler{memberships: members}
	if tenant == u.TenantID || (u.TenantID == "" && tenant == core.DefaultTenantSlug) {
		_, roles, err := h.resolveActingTenant(ctx, u, "")
		return roles, err
	}
	_, roles, err := h.resolveActingTenant(ctx, u, tenant)
	return roles, err
}

// addressAllowed reports whether ip falls in any of the ranges.
func addressAllowed(ip string, ranges []string) bool {
	addr := net.ParseIP(ip)
	if addr == nil {
		return false
	}
	for _, c := range ranges {
		if _, n, err := net.ParseCIDR(c); err == nil && n.Contains(addr) {
			return true
		}
	}
	return false
}

// enforceGrants lets an admin token reach a route only when the route
// declares a grant the token holds. It sits after TenantHeader on the admin
// router and passes session and API key requests through untouched.
//
// It is fail-closed: a route with no declaration is session only, a public
// route is refused as it is on every other router, and a path no route
// serves is 404.
func enforceGrants(reg *adminGrantRegistry) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			caller := adminTokenFromCtx(r.Context())
			if caller == nil {
				next.ServeHTTP(w, r)
				return
			}
			// A path no route serves is answered here rather than handed on:
			// nothing below this point is allowed to decide for a token.
			if caller.pattern == "" {
				httpx.ErrorReq(w, r, http.StatusNotFound, "not found")
				return
			}
			method := r.Method
			if method == http.MethodHead {
				method = http.MethodGet
			}
			if reg.isPublic(method, caller.pattern) {
				httpx.ErrorReq(w, r, http.StatusUnauthorized, msgAdminTokenOffAdminAPI)
				return
			}
			grant := reg.grantFor(method, caller.pattern)
			if grant == "" {
				httpx.ErrorReq(w, r, http.StatusForbidden, msgAdminTokenSessionOnly)
				return
			}
			if !caller.token.HasGrant(grant) {
				httpx.ErrorReq(w, r, http.StatusForbidden, "This token is not granted "+grant+".")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// engineAdminGrants are the engine's own admin routes an admin token may
// reach, by method and chi pattern. Everything the engine serves that is not
// here is session only.
//
// Group states the role gate the router puts on the route, so the grant
// check can refuse a grant on a super admin route here as it does for a
// plugin's. A router test holds each entry to the gate it declares.
var engineAdminGrants = []core.RouteDecl{
	{Method: http.MethodGet, Pattern: "/api/admin/schemas/export", Group: core.GroupAdmin, AdminGrant: core.AdminGrantSchemasRead},
}

// enginePublicAdminRoutes are the engine's routes on the admin server that
// take no credential. An admin token presented to one is refused.
var enginePublicAdminRoutes = []core.RouteDecl{
	{Method: http.MethodGet, Pattern: "/api/admin/setup"},
	{Method: http.MethodPost, Pattern: "/api/admin/setup"},
	{Method: http.MethodPost, Pattern: "/api/admin/auth/login"},
	{Method: http.MethodPost, Pattern: "/api/admin/auth/refresh"},
	{Method: http.MethodPost, Pattern: "/api/admin/auth/mfa-verify"},
	{Method: http.MethodPost, Pattern: "/api/admin/auth/device"},
	{Method: http.MethodPost, Pattern: "/api/admin/auth/device/token"},
	{Method: http.MethodGet, Pattern: "/healthz"},
	{Method: http.MethodGet, Pattern: "/readyz"},
	{Method: http.MethodGet, Pattern: "/startup"},
	{Method: http.MethodGet, Pattern: "/robots.txt"},
}

// adminGrantRoute is one route that declares a grant.
type adminGrantRoute struct {
	Method  string `json:"method"`
	Pattern string `json:"pattern"`
}

// adminGrantRegistry maps each admin route an admin token may reach to the
// grant it needs, and knows the admin server's public routes.
type adminGrantRegistry struct {
	grants  map[string]string
	public  map[string]bool
	byGrant map[string][]adminGrantRoute
}

func grantKey(method, pattern string) string { return strings.ToUpper(method) + " " + pattern }

// buildAdminGrantRegistry reads the grants the engine table, the plugins and
// the other route owners declare. An unknown grant name, or a grant on a
// session-only route, is an error: the admin router refuses to build rather
// than open a route by mistake or leave one silently closed.
func buildAdminGrantRegistry(pluginRoutes, ownerRoutes []plugin.PluginRoutes) (*adminGrantRegistry, error) {
	reg := &adminGrantRegistry{grants: map[string]string{}, public: map[string]bool{}, byGrant: map[string][]adminGrantRoute{}}
	if err := core.ValidateAdminGrants(engineAdminGrants); err != nil {
		return nil, fmt.Errorf("engine admin grants: %w", err)
	}
	// An error names the set it found as its owner: a plugin by the word and
	// its name, any other owner by its name alone.
	type routeSet struct {
		owner, subject string
		routes         []core.RouteDecl
	}
	sets := make([]routeSet, 0, len(pluginRoutes)+len(ownerRoutes))
	for _, pr := range pluginRoutes {
		sets = append(sets, routeSet{owner: "plugin " + pr.Name, subject: "the plugin", routes: pr.Routes})
	}
	for _, ow := range ownerRoutes {
		sets = append(sets, routeSet{owner: ow.Name, subject: "its owner", routes: ow.Routes})
	}
	// A route one declaration marks session only refuses a grant from every
	// declaration, the engine's own table included.
	var declared []core.RouteDecl
	for _, set := range sets {
		declared = append(declared, set.routes...)
	}
	engineGranted := map[string]bool{}
	for _, rd := range engineAdminGrants {
		engineGranted[grantKey(rd.Method, normalizeRoutePattern(rd.Pattern))] = true
	}
	engineOwned := engineOwnedRoutes(adminPathPrefix)
	for _, set := range sets {
		if err := core.ValidateAdminGrants(set.routes, declared...); err != nil {
			return nil, fmt.Errorf("%s: %w", set.owner, err)
		}
		for _, rd := range engineAdminGrants {
			if core.IsSessionOnlyAdminRoute(rd.Method, rd.Pattern, set.routes...) {
				return nil, fmt.Errorf("%s: route %s %s is declared session only, and the engine grants it to admin tokens as %q",
					set.owner, rd.Method, rd.Pattern, rd.AdminGrant)
			}
		}
		// The engine's handler wins a pattern both declare, so a declared
		// grant there would open the engine's route to tokens.
		for _, rd := range set.routes {
			if rd.AdminGrant == "" {
				continue
			}
			key := grantKey(rd.Method, normalizeRoutePattern(rd.Pattern))
			if _, owned := engineOwned[key]; owned && !engineGranted[key] {
				return nil, fmt.Errorf("%s: route %s %s is served by the engine, which grants it to no admin token; %s cannot declare admin grant %q on it",
					set.owner, rd.Method, rd.Pattern, set.subject, rd.AdminGrant)
			}
		}
	}
	add := func(rd core.RouteDecl) {
		key := grantKey(rd.Method, rd.Pattern)
		if _, seen := reg.grants[key]; seen {
			return
		}
		reg.grants[key] = rd.AdminGrant
		reg.byGrant[rd.AdminGrant] = append(reg.byGrant[rd.AdminGrant], adminGrantRoute{Method: strings.ToUpper(rd.Method), Pattern: rd.Pattern})
	}
	// The engine's handler wins a pattern both declare, so its entry does.
	for _, rd := range engineAdminGrants {
		add(rd)
	}
	for _, rd := range enginePublicAdminRoutes {
		reg.public[grantKey(rd.Method, rd.Pattern)] = true
	}
	for _, set := range sets {
		for _, rd := range set.routes {
			if !strings.HasPrefix(rd.Pattern, adminPathPrefix+"/") {
				continue
			}
			if rd.Group == core.GroupPublic {
				reg.public[grantKey(rd.Method, rd.Pattern)] = true
				continue
			}
			if rd.AdminGrant != "" {
				add(rd)
			}
		}
	}
	return reg, nil
}

func (g *adminGrantRegistry) grantFor(method, pattern string) string {
	return g.grants[grantKey(method, pattern)]
}

func (g *adminGrantRegistry) isPublic(method, pattern string) bool {
	return g.public[grantKey(method, pattern)]
}

// routesFor lists the routes declaring grant, in declaration order.
func (g *adminGrantRegistry) routesFor(grant string) []adminGrantRoute {
	out := append([]adminGrantRoute(nil), g.byGrant[grant]...)
	if out == nil {
		out = []adminGrantRoute{}
	}
	return out
}

// statusCapture records the status a handler wrote.
type statusCapture struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusCapture) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusCapture) Write(b []byte) (int, error) {
	s.wroteHeader = true
	return s.ResponseWriter.Write(b)
}

// Unwrap exposes the writer underneath for http.ResponseController.
func (s *statusCapture) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// Flush passes a streaming response through.
func (s *statusCapture) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack hands the connection to a handler that wants to take it over. An
// admin token route is served over a real connection like any other, so a
// wrapper that swallowed this would make an upgrade fail behind admin token
// auth and work everywhere else.
func (s *statusCapture) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := s.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return h.Hijack()
}

// Compile-time check that statusCapture keeps http.Flusher and http.Hijacker,
// so a wrapper that drops one fails to build instead of failing at run time.
var (
	_ http.Flusher  = (*statusCapture)(nil)
	_ http.Hijacker = (*statusCapture)(nil)
)

// Request log.

const (
	// adminTokenLogQueue bounds the rows waiting to be written. A request
	// never waits on the log: past this, a row is not queued but counted
	// against its token, and the count is written as a row of its own.
	adminTokenLogQueue = 1024
	// adminTokenLogBatch caps the rows one insert writes.
	adminTokenLogBatch = 100
	// adminTokenRetention is how long request rows are kept.
	adminTokenRetention = 90 * 24 * time.Hour
	// adminTokenPruneEvery spaces the prunes the writer runs.
	adminTokenPruneEvery = time.Hour
	// adminTokenPruneBatch caps the rows one prune statement deletes, and
	// adminTokenPruneRounds the statements one prune runs, so a backlog is
	// worked off in slices rather than in one long lock.
	adminTokenPruneBatch  = 1000
	adminTokenPruneRounds = 50
)

// AdminTokenDroppedPrefix opens the route pattern of a request log row that
// stands for rows the full queue could not take: "dropped:<n>", with status
// 0 and no method, one row per token each time the writer catches up.
const AdminTokenDroppedPrefix = "dropped:"

type adminTokenLogItem struct {
	req     *core.AdminTokenRequest
	touchID uuid.UUID
	tenant  string
	at      time.Time
}

type adminTokenDropped struct {
	tenant string
	n      int
}

// adminTokenLog writes the request log and last_used_at off the request
// path, on one goroutine per router build. The engine runs no periodic
// database cleanup of its own, so the writer also prunes rows past the
// retention window, at most once an hour.
type adminTokenLog struct {
	store     core.AdminTokenStore
	items     chan adminTokenLogItem
	mu        sync.Mutex
	lastTouch map[uuid.UUID]time.Time
	dropped   map[uuid.UUID]*adminTokenDropped
	lastPrune time.Time
}

func newAdminTokenLog(ctx context.Context, store core.AdminTokenStore) *adminTokenLog {
	l := &adminTokenLog{
		store:     store,
		items:     make(chan adminTokenLogItem, adminTokenLogQueue),
		lastTouch: map[uuid.UUID]time.Time{},
		dropped:   map[uuid.UUID]*adminTokenDropped{},
	}
	if store != nil {
		go l.run(ctx)
	}
	return l
}

func (l *adminTokenLog) enqueue(it adminTokenLogItem) bool {
	if l == nil || l.store == nil {
		return true
	}
	select {
	case l.items <- it:
		return true
	default:
		return false
	}
}

// request records one request a token made.
func (l *adminTokenLog) request(r core.AdminTokenRequest) {
	if l.enqueue(adminTokenLogItem{req: &r}) {
		return
	}
	l.mu.Lock()
	d := l.dropped[r.TokenID]
	if d == nil {
		d = &adminTokenDropped{tenant: r.TenantID}
		l.dropped[r.TokenID] = d
	}
	d.n++
	l.mu.Unlock()
}

// touch records a use, at most once a minute per token.
func (l *adminTokenLog) touch(tok *core.AdminToken, at time.Time) {
	if l == nil {
		return
	}
	l.mu.Lock()
	last, ok := l.lastTouch[tok.ID]
	if ok && at.Sub(last) < time.Minute {
		l.mu.Unlock()
		return
	}
	l.lastTouch[tok.ID] = at
	l.mu.Unlock()
	l.enqueue(adminTokenLogItem{touchID: tok.ID, tenant: tok.TenantID, at: at})
}

func (l *adminTokenLog) run(ctx context.Context) {
	for {
		select {
		case it := <-l.items:
			l.writeBatch(it)
		case <-ctx.Done():
			// A router build that is replaced still owes the rows it queued.
			for {
				select {
				case it := <-l.items:
					l.writeBatch(it)
				default:
					l.flushDropped()
					return
				}
			}
		}
	}
}

// writeBatch writes first and whatever else is already queued, up to one
// batch of request rows, then the counts of any rows that were dropped.
func (l *adminTokenLog) writeBatch(first adminTokenLogItem) {
	// Not the request's context: the request has returned by now.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var rows []core.AdminTokenRequest
	take := func(it adminTokenLogItem) {
		if it.req != nil {
			rows = append(rows, *it.req)
			return
		}
		if err := l.store.TouchLastUsed(ctx, it.tenant, it.touchID, it.at); err != nil {
			slog.Warn("admin token last-used write failed", "err", err)
		}
	}
	take(first)
drain:
	for len(rows) < adminTokenLogBatch {
		select {
		case it := <-l.items:
			take(it)
		default:
			break drain
		}
	}
	if len(rows) > 0 {
		if err := l.store.LogRequests(ctx, rows); err != nil {
			slog.Warn("admin token request log write failed", "rows", len(rows), "err", err)
		}
		l.prune(ctx, rows[len(rows)-1].CreatedAt)
	}
	l.flushDropped()
}

// flushDropped writes one row per token for the rows the queue refused
// since the last flush.
func (l *adminTokenLog) flushDropped() {
	l.mu.Lock()
	if len(l.dropped) == 0 {
		l.mu.Unlock()
		return
	}
	pending := l.dropped
	l.dropped = map[uuid.UUID]*adminTokenDropped{}
	l.mu.Unlock()

	now := time.Now()
	rows := make([]core.AdminTokenRequest, 0, len(pending))
	for id, d := range pending {
		rows = append(rows, core.AdminTokenRequest{
			TokenID:      id,
			TenantID:     d.tenant,
			RoutePattern: AdminTokenDroppedPrefix + strconv.Itoa(d.n),
			CreatedAt:    now,
		})
		slog.Warn("admin token request log queue was full; rows were dropped", "token_id", id, "dropped", d.n)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for start := 0; start < len(rows); start += adminTokenLogBatch {
		end := min(start+adminTokenLogBatch, len(rows))
		if err := l.store.LogRequests(ctx, rows[start:end]); err != nil {
			slog.Warn("admin token dropped-row count write failed", "err", err)
		}
	}
}

// prune deletes rows past the retention window, batch after batch until one
// comes back short, at most once an hour.
func (l *adminTokenLog) prune(ctx context.Context, now time.Time) {
	if !l.lastPrune.IsZero() && now.Sub(l.lastPrune) < adminTokenPruneEvery {
		return
	}
	l.lastPrune = now
	cutoff := now.Add(-adminTokenRetention)
	for range adminTokenPruneRounds {
		n, err := l.store.PruneRequests(ctx, cutoff, adminTokenPruneBatch)
		if err != nil {
			slog.Warn("admin token request log prune failed", "err", err)
			return
		}
		if n < adminTokenPruneBatch {
			return
		}
	}
}
