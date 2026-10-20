package testdialect

import "testing"

func TestEnabled_FollowsCIDialect(t *testing.T) {
	cases := []struct {
		env, name string
		want      bool
	}{
		{"", "postgres", true},
		{"", "mysql", true},
		{"", "mssql", true},
		{"postgres", "postgres", true},
		{"postgres", "mysql", false},
		{"postgres", "mssql", false},
		{"mssql", "mssql", true},
		{"mssql", "postgres", false},
	}
	for _, c := range cases {
		t.Run(c.env+"_covers_"+c.name, func(t *testing.T) {
			t.Setenv("CI_DIALECT", c.env)
			if got := Enabled(c.name); got != c.want {
				t.Errorf("CI_DIALECT=%q: Enabled(%q) = %v, want %v", c.env, c.name, got, c.want)
			}
		})
	}
}

func TestRequire_SkipsADialectTheRunDoesNotCover(t *testing.T) {
	t.Setenv("CI_DIALECT", "postgres")

	reached := false
	if !t.Run("mysql", func(t *testing.T) {
		Require(t, "mysql")
		reached = true
	}) {
		t.Fatal("Require failed the test instead of skipping it")
	}
	if reached {
		t.Fatal("a mysql test ran on past Require in a run limited to postgres")
	}

	reached = false
	t.Run("postgres", func(t *testing.T) {
		Require(t, "postgres")
		reached = true
	})
	if !reached {
		t.Fatal("Require skipped the dialect the run covers")
	}
}
