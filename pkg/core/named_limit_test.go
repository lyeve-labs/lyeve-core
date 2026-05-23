package core

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fixedResolver map[string]NamedLimit

func (f fixedResolver) NamedLimit(_ context.Context, name string) (NamedLimit, bool) {
	l, ok := f[name]
	return l, ok
}

func resetNamedLimits(t *testing.T) {
	t.Helper()
	namedLimits.Lock()
	saved, savedResolver := namedLimits.decls, namedLimits.resolver
	namedLimits.decls, namedLimits.resolver = map[string]NamedLimitDecl{}, nil
	namedLimits.Unlock()
	t.Cleanup(func() {
		namedLimits.Lock()
		namedLimits.decls, namedLimits.resolver = saved, savedResolver
		namedLimits.Unlock()
	})
}

func TestResolveNamedLimit_Precedence(t *testing.T) {
	declared := NamedLimit{Requests: 1, Window: 5 * time.Minute}
	fallback := NamedLimit{Requests: 9, Window: time.Minute}
	configured := NamedLimit{Requests: 3, Window: 10 * time.Minute}

	cases := []struct {
		name     string
		declare  bool
		resolver NamedLimitResolver
		want     NamedLimit
	}{
		{"undeclared and no resolver uses the fallback", false, nil, fallback},
		{"declared and no resolver uses the caller's value, not the declaration", true, nil, fallback},
		{"resolver answer wins", true, fixedResolver{"reset.email": configured}, configured},
		{"resolver with no answer uses the fallback", true, fixedResolver{}, fallback},
		{"invalid resolver answer uses the fallback", true, fixedResolver{"reset.email": {Requests: 0, Window: time.Minute}}, fallback},
		{"sub-second window is invalid", true, fixedResolver{"reset.email": {Requests: 5, Window: time.Millisecond}}, fallback},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetNamedLimits(t)
			if tc.declare {
				DeclareNamedLimit(NamedLimitDecl{Name: "reset.email", Plugin: "reset", Default: declared})
			}
			if tc.resolver != nil {
				RegisterNamedLimitResolver(tc.resolver)
			}
			assert.Equal(t, tc.want, ResolveNamedLimit(context.Background(), "reset.email", fallback))
		})
	}
}

func TestDeclareNamedLimit_ReplacesAndRejectsInvalid(t *testing.T) {
	resetNamedLimits(t)
	DeclareNamedLimit(NamedLimitDecl{Name: "b.key", Default: NamedLimit{Requests: 1, Window: time.Minute}})
	DeclareNamedLimit(NamedLimitDecl{Name: "a.key", Default: NamedLimit{Requests: 2, Window: time.Minute}})
	DeclareNamedLimit(NamedLimitDecl{Name: "a.key", Default: NamedLimit{Requests: 4, Window: time.Minute}})
	DeclareNamedLimit(NamedLimitDecl{Name: "c.key", Default: NamedLimit{Requests: 0, Window: time.Minute}})

	got := DeclaredNamedLimits()
	require.Len(t, got, 2)
	assert.Equal(t, "a.key", got[0].Name)
	assert.Equal(t, 4, got[0].Default.Requests)
	assert.Equal(t, "b.key", got[1].Name)
	assert.False(t, NamedLimitDeclared("c.key"))
}

func TestRegisterNamedLimitResolver_UnregisterOnlyRemovesItself(t *testing.T) {
	resetNamedLimits(t)
	fallback := NamedLimit{Requests: 1, Window: time.Minute}
	first := fixedResolver{"x.key": {Requests: 2, Window: time.Minute}}
	second := fixedResolver{"x.key": {Requests: 3, Window: time.Minute}}

	unregisterFirst := RegisterNamedLimitResolver(first)
	RegisterNamedLimitResolver(second)
	unregisterFirst()
	assert.Equal(t, 3, ResolveNamedLimit(context.Background(), "x.key", fallback).Requests)
}
