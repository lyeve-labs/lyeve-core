package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/hooks"
	"github.com/lyeve-labs/lyeve-core/internal/tenant"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// asSignedIn is the context the session middleware leaves: the internal
// claims and the plugin-facing copy of them, side by side.
func asSignedIn(ctx context.Context, userID string) context.Context {
	claims := &auth.Claims{UserID: userID, Roles: []string{"super_admin"}}
	ctx = context.WithValue(ctx, auth.ClaimsKey, claims)
	return context.WithValue(ctx, core.ClaimsKey, claims.AuthClaims())
}

// A subscriber that records who changed an entry runs after the request is
// gone, so the event has to name the caller. A session names its user on a
// create, an update and a delete. An API key names the id the key
// authenticates as.
func TestContentHandler_LifecycleEventsNameTheActor(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"notes"}, func(t *testing.T, fx schemaContentFixture) {
		var events []hooks.Event
		for _, et := range []hooks.EventType{hooks.AfterCreate, hooks.AfterUpdate, hooks.AfterDelete} {
			fx.content.hooks.Register("notes", et, func(_ context.Context, e hooks.Event) error {
				events = append(events, e)
				return nil
			})
		}

		user := uuid.NewString()
		session := asSignedIn(fx.ctx, user)
		path := "/api/v1/content/notes"
		rr := fx.send(t, session, http.MethodPost, path, map[string]string{"schema": "notes"},
			map[string]any{"data": map[string]any{"title": "Draft", "body": "first"}}, fx.content.Create)
		id := createdID(t, rr)
		params := map[string]string{"schema": "notes", "id": id}
		rr = fx.send(t, session, http.MethodPut, path+"/"+id, params,
			map[string]any{"data": map[string]any{"title": "Draft", "body": "second"}}, fx.content.Update)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		rr = fx.send(t, session, http.MethodDelete, path+"/"+id, params, nil, fx.content.Delete)
		require.Less(t, rr.Code, 300, rr.Body.String())

		key := uuid.NewString()
		apiKey := context.WithValue(tenant.WithID(context.Background(), "t1"), core.ClaimsKey, &core.AuthClaims{
			UserID: key, Roles: []string{"super_admin"}, IsAPIKey: true, APIKeyID: key,
		})
		rr = fx.send(t, apiKey, http.MethodPost, path, map[string]string{"schema": "notes"},
			map[string]any{"data": map[string]any{"title": "By key", "body": "third"}}, fx.content.Create)
		createdID(t, rr)

		require.Len(t, events, 4)
		want := []struct {
			typ   hooks.EventType
			actor string
			kind  string
		}{
			{hooks.AfterCreate, user, core.ActorKindUser},
			{hooks.AfterUpdate, user, core.ActorKindUser},
			{hooks.AfterDelete, user, core.ActorKindUser},
			{hooks.AfterCreate, key, core.ActorKindAPIKey},
		}
		for i, w := range want {
			assert.Equal(t, w.typ, events[i].Type, "event %d", i)
			assert.Equal(t, w.actor, events[i].ActorID, "event %d (%s)", i, w.typ)
			assert.Equal(t, w.kind, events[i].ActorKind, "event %d (%s)", i, w.typ)
		}
	})
}
