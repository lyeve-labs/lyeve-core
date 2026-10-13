package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// The refusals AuthorizeContent reports. Anything else it returns is the rule
// store failing, which a transport answers as unavailable rather than as a
// refusal.
var (
	ErrNoCaller         = errors.New("no caller")
	ErrSchemaNotAllowed = errors.New("schema not allowed for this credential")
	ErrScopeDenied      = errors.New("insufficient scope")
	ErrPermissionDenied = errors.New("permission denied")
	// ErrHiddenFieldQueried is a filter or a sort that names a field the
	// caller's mask hides.
	ErrHiddenFieldQueried = errors.New("a filter or sort names a field this caller cannot read")
)

// PermissionCheckerOf returns the checker v provides, read at the moment of
// the call. Anything that does not provide one gets the kernel's role-only
// checker, so a transport that cannot reach the rules is closed rather than
// open.
func PermissionCheckerOf(v any) PermissionChecker {
	if p, ok := v.(PermissionCheckerProvider); ok {
		if c := p.PermissionChecker(); c != nil {
			return c
		}
	}
	return RoleOnlyPermissionChecker{}
}

// AuthorizeContent is the one decision every content transport makes before
// it reads or writes an entry of schema. A caller limited to a list of
// schemas is refused one outside it, an API key whose scopes do not grant
// action on schema is refused next, and then the caller's roles go through
// checker for action.
//
// The scope is asked here and not only at the route. A route's scope gate
// reads the schema its path names. A relation, a populated field and a
// transport that serves every schema from one endpoint each reach a schema
// no gate saw, so a key held to content.posts would read authors through
// posts. The terms are the gate's: content:<action> covers every schema,
// content.<schema>:<action> covers that one, and a key holding admin or
// super_admin answers to its role instead of its scopes. A caller that is
// not an API key is judged by its roles alone.
//
// Nil claims are refused. Every content transport authenticates first, so a
// missing caller means a route mounted without authentication, and a gate
// that waves through what it cannot identify is open to everyone.
func AuthorizeContent(ctx context.Context, checker PermissionChecker, claims *AuthClaims, schema, action string) error {
	if claims == nil {
		return ErrNoCaller
	}
	if !claims.AllowsSchema(schema) {
		return ErrSchemaNotAllowed
	}
	if claims.IsAPIKey && !claims.HasRole(RoleAdmin) && !claims.HasRole(RoleSuperAdmin) &&
		!ScopeGrants(claims.Scopes, "content."+schema, action) {
		return ErrScopeDenied
	}
	if checker == nil {
		checker = RoleOnlyPermissionChecker{}
	}
	allowed, err := checker.Allowed(ctx, claims.Roles, schema, action)
	if err != nil {
		return fmt.Errorf("permission check %s: %w", schema, err)
	}
	if !allowed {
		return ErrPermissionDenied
	}
	return nil
}

// HiddenFields is the set of fields of one schema a caller may not read.
type HiddenFields map[string]struct{}

// Names reports whether key names a hidden field: the field itself, or the
// column a relation field is stored and filtered in, which carries an _id
// suffix. A filter or a sort on a hidden field answers which entries hold a
// guessed value, so it reads the field as surely as serving it would.
func (h HiddenFields) Names(key string) bool {
	if len(h) == 0 {
		return false
	}
	if _, ok := h[key]; ok {
		return true
	}
	if base, ok := strings.CutSuffix(key, "_id"); ok {
		_, hidden := h[base]
		return hidden
	}
	return false
}

// Apply returns data without the hidden fields. It copies an entry it
// changes and never edits the map it is given.
func (h HiddenFields) Apply(data map[string]any) map[string]any {
	if len(h) == 0 || len(data) == 0 {
		return data
	}
	out := make(map[string]any, len(data))
	for k, v := range data {
		if _, off := h[k]; !off {
			out[k] = v
		}
	}
	return out
}

// ContentHiddenFields resolves the fields of schema the caller may not read.
// A checker that does not implement FieldMasker hides nothing, since a mask
// only ever comes from a rule. Resolve it once per request, because each
// resolution asks the rule store once per role.
func ContentHiddenFields(ctx context.Context, checker PermissionChecker, claims *AuthClaims, schema string) (HiddenFields, error) {
	masker, ok := checker.(FieldMasker)
	if !ok {
		return nil, nil
	}
	if claims == nil {
		return nil, ErrNoCaller
	}
	fields, err := masker.FieldMask(ctx, claims.Roles, schema)
	if err != nil {
		return nil, fmt.Errorf("field mask %s: %w", schema, err)
	}
	if len(fields) == 0 {
		return nil, nil
	}
	hidden := make(HiddenFields, len(fields))
	for _, f := range fields {
		hidden[f] = struct{}{}
	}
	return hidden, nil
}

// ContentFieldMask is ContentHiddenFields as the function that drops the
// hidden fields from an entry.
func ContentFieldMask(ctx context.Context, checker PermissionChecker, claims *AuthClaims, schema string) (func(map[string]any) map[string]any, error) {
	hidden, err := ContentHiddenFields(ctx, checker, claims, schema)
	if err != nil {
		return nil, err
	}
	return hidden.Apply, nil
}
