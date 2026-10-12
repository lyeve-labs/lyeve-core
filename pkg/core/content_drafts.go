package core

import "context"

// ContentStatusKey is the key a content row, and a content event's payload,
// carries its publication status under.
const ContentStatusKey = "_status"

// ContentPublished reports whether a content row or an event payload is
// public. A schema without draft and publish has no status, and every row of
// it is public. A NULL status reads as published, the way the Content API's
// list filter treats it.
func ContentPublished(data map[string]any) bool {
	status, ok := data[ContentStatusKey].(string)
	return !ok || status == ContentStatusPublished
}

// ContentEventPublished reports whether a content event's payload is
// public, failing closed where ContentPublished fails open. On a schema with
// draft and publish only a payload whose status reads published is public:
// a writer that sends only the columns it changed sends no status, and that
// change may be to a draft. On any other schema every payload is public. A
// caller that cannot tell whether the schema has draft and publish passes
// true.
func ContentEventPublished(draftPublish bool, data map[string]any) bool {
	if !draftPublish {
		return true
	}
	status, ok := data[ContentStatusKey].(string)
	return ok && status == ContentStatusPublished
}

// DraftPermissionChecker returns the checker that decides who may read
// drafts on a host: the registered rule engine when there is one, and the
// kernel's role-only checker otherwise, so a host without rules is closed
// rather than open.
func DraftPermissionChecker(host any) PermissionChecker {
	if p, ok := host.(PermissionCheckerProvider); ok {
		if c := p.PermissionChecker(); c != nil {
			return c
		}
	}
	return RoleOnlyPermissionChecker{}
}

// CanReadDrafts reports whether the caller on ctx may see unpublished rows
// of schema. Every surface that serves content asks it, so a draft is
// hidden from the same callers whichever transport reads it.
//
// Drafts belong to the people who write them, so the test is the update
// permission on the schema. An API key needs more than its roles: a key
// scoped to reading content is a read credential, and a read credential
// sees what the public sees, so it also needs the update scope on this
// schema, which content:write and content.<schema>:update both grant. A
// key holding admin or super_admin is judged by role, as it is at every
// other scope gate. A rule store that cannot be read answers no, and so do
// absent claims. The caller passes the claims it authenticated, because a
// session and a key reach a handler under different context keys.
func CanReadDrafts(ctx context.Context, checker PermissionChecker, claims *AuthClaims, schema string) bool {
	if claims == nil || !claims.AllowsSchema(schema) {
		return false
	}
	if claims.IsAPIKey && !claims.HasRole("admin") && !claims.HasRole("super_admin") &&
		!ScopeGrants(claims.Scopes, "content."+schema, ActionUpdate) {
		return false
	}
	if checker == nil {
		checker = RoleOnlyPermissionChecker{}
	}
	ok, err := checker.Allowed(ctx, claims.Roles, schema, "update")
	return err == nil && ok
}
