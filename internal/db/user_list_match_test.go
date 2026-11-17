//go:build !mutest

package db_test

import (
	"context"
	"sort"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
)

func TestUserStore_ListMatchingFiltersByAddressAndRole(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		ctx := context.Background()
		store := db.NewUserStore(pool)

		seed := []struct {
			email string
			roles []string
		}{
			{"ann@alpha.test", []string{"editor"}},
			{"bob@alpha.test", []string{"editor", "admin"}},
			{"carol@beta.test", []string{"viewer"}},
			{"dave@beta.test", []string{"super_admin"}},
			{"a_b@lit.test", []string{"editor"}},
			{"axb@lit.test", []string{"editor"}},
			{"pct%x@lit.test", []string{"editor"}},
			{"pctyx@lit.test", []string{"editor"}},
			{"jos\u00e9@accent.test", []string{"viewer"}},
			{"erin@beta.test", []string{"marketing", "editor"}},
		}
		for _, s := range seed {
			if _, err := store.Create(ctx, s.email, "x", s.roles, "acme"); err != nil {
				t.Fatalf("Create %s: %v", s.email, err)
			}
		}

		cases := []struct {
			name, q, role string
			want          []string
		}{
			{"no filter lists everyone", "", "", []string{
				"a_b@lit.test", "ann@alpha.test", "axb@lit.test", "bob@alpha.test", "carol@beta.test",
				"dave@beta.test", "erin@beta.test", "jos\u00e9@accent.test", "pct%x@lit.test", "pctyx@lit.test",
			}},
			{"q ignores case", "ALPHA", "", []string{"ann@alpha.test", "bob@alpha.test"}},
			{"q matches a literal underscore", "a_b", "", []string{"a_b@lit.test"}},
			{"q matches a literal percent", "pct%", "", []string{"pct%x@lit.test"}},
			{"q in decomposed form finds the composed address", "JOSE\u0301", "", []string{"jos\u00e9@accent.test"}},
			{"q that matches nothing", "nobody", "", nil},
			{"role is exact membership", "", "admin", []string{"bob@alpha.test"}},
			{"role finds every holder", "", "viewer", []string{"carol@beta.test", "jos\u00e9@accent.test"}},
			{"a role a provider maps is found", "", "marketing", []string{"erin@beta.test"}},
			{"a role nobody holds finds nothing", "", "sales", nil},
			{"role matches case exactly, as the role checks do", "", "ADMIN", nil},
			{"q and role together", "alpha", "editor", []string{"ann@alpha.test", "bob@alpha.test"}},
			{"q and role that exclude each other", "alpha", "viewer", nil},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got, err := store.ListMatching(ctx, tc.q, tc.role, 100, 0)
				if err != nil {
					t.Fatalf("ListMatching: %v", err)
				}
				assertEmails(t, got, tc.want)
			})
		}

		t.Run("pages past the first page of matches", func(t *testing.T) {
			first, err := store.ListMatching(ctx, "LIT.test", "editor", 3, 0)
			if err != nil {
				t.Fatalf("first page: %v", err)
			}
			second, err := store.ListMatching(ctx, "LIT.test", "editor", 3, 3)
			if err != nil {
				t.Fatalf("second page: %v", err)
			}
			if len(first) != 3 || len(second) != 1 {
				t.Fatalf("pages of 3 over 4 matches: got %d and %d rows", len(first), len(second))
			}
			assertEmails(t, append(first, second...), []string{
				"a_b@lit.test", "axb@lit.test", "pct%x@lit.test", "pctyx@lit.test",
			})
		})
	})
}

// assertEmails compares the listed addresses with want, in any order, and
// fails on a repeat, since a page that repeats a row also skips one.
func assertEmails(t *testing.T, got []*domain.User, want []string) {
	t.Helper()
	emails := make([]string, 0, len(got))
	for _, u := range got {
		emails = append(emails, u.Email)
	}
	sort.Strings(emails)
	want = append([]string(nil), want...)
	sort.Strings(want)
	if len(emails) != len(want) {
		t.Fatalf("listed %q, want %q", emails, want)
	}
	for i := range want {
		if emails[i] != want[i] {
			t.Fatalf("listed %q, want %q", emails, want)
		}
	}
}
