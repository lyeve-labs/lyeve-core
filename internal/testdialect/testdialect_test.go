package testdialect

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

// Every test that starts SQL Server names the pinned image, so no file names
// the image by a tag that can move.
func TestMSSQLImage_IsTheOnlyReferenceToTheImage(t *testing.T) {
	if !strings.Contains(MSSQLImage, "@sha256:") {
		t.Fatalf("MSSQLImage %q is not pinned by digest", MSSQLImage)
	}
	tagged := "azure-sql-edge" + ":"
	err := filepath.WalkDir("../..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// The lint suite under tools quotes image names as fixtures of the
		// code it refuses, and starts no server.
		if d.IsDir() && (d.Name() == "node_modules" || d.Name() == ".git" || path == filepath.Join("..", "..", "tools")) {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), tagged) {
			t.Errorf("%s names the SQL Server image by tag. Use testdialect.MSSQLImage", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
