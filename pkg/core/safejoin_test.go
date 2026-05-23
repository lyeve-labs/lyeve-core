package core

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSafeJoin(t *testing.T) {
	root := t.TempDir()

	t.Run("normal_path", func(t *testing.T) {
		got, err := SafeJoin(root, "images/cat.jpg")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := filepath.Join(root, "images", "cat.jpg")
		if got != want {
			t.Fatalf("SafeJoin = %q, want %q", got, want)
		}
	})

	t.Run("empty_prefix", func(t *testing.T) {
		_, err := SafeJoin(root, "")
		if err == nil {
			t.Fatal("expected ErrInvalidPath for empty rel")
		}
		if !errors.Is(err, ErrInvalidPath) {
			t.Fatalf("expected ErrInvalidPath, got %v", err)
		}
	})

	t.Run("dot_prefix", func(t *testing.T) {
		got, err := SafeJoin(root, ".")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := filepath.Join(root, ".")
		if got != want {
			t.Fatalf("SafeJoin = %q, want %q", got, want)
		}
	})

	t.Run("nested_prefix", func(t *testing.T) {
		got, err := SafeJoin(root, "a/b/c")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := filepath.Join(root, "a", "b", "c")
		if got != want {
			t.Fatalf("SafeJoin = %q, want %q", got, want)
		}
	})

	t.Run("traversal_normalized_under_root", func(t *testing.T) {
		// SafeJoin normalizes traversal attempts to be under root,
		// matching the existing abs() pattern in all three drivers.
		got, err := SafeJoin(root, "../../etc")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Must be under root
		if !strings.HasPrefix(got, root) {
			t.Fatalf("SafeJoin(%q, %q) = %q, not under root", root, "../../etc", got)
		}
	})

	t.Run("traversal_inside_root", func(t *testing.T) {
		// Traversal sequences within the root boundary collapse safely.
		got, err := SafeJoin(root, "a/./b/../c")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := filepath.Join(root, "a", "c")
		if got != want {
			t.Fatalf("SafeJoin = %q, want %q", got, want)
		}
	})

	t.Run("all_results_stay_under_root", func(t *testing.T) {
		// "" is intentionally excluded: empty rel is rejected by the early guard
		// (tested explicitly in the "empty_prefix" subtest above).
		prefixes := []string{
			".",
			"images",
			"images/../docs",
			"../../../tmp",
			"a/b/c/../../../d",
		}
		for _, p := range prefixes {
			got, err := SafeJoin(root, p)
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", p, err)
			}
			if !strings.HasPrefix(got, root) {
				t.Fatalf("SafeJoin(root, %q) = %q: escaped root", p, got)
			}
		}
	})

	t.Run("verify_no_possible_escape", func(t *testing.T) {
		// Create a sibling directory to verify SafeJoin can't reach it.
		sibling := filepath.Join(filepath.Dir(root), "secret")
		os.MkdirAll(sibling, 0o755)
		os.WriteFile(filepath.Join(sibling, "passwords.txt"), []byte("secret"), 0o644)

		// Even with aggressive traversal, result stays under root.
		got, err := SafeJoin(root, "../../../secret/passwords.txt")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// The resolved path must be under root, not reaching the sibling.
		if !strings.HasPrefix(got, root) {
			t.Fatalf("SafeJoin leaked to sibling: %q", got)
		}
		// Verify the file at that path either doesn't exist or isn't the sibling's.
		if _, statErr := os.Stat(got); statErr == nil {
			t.Fatalf("resolution reached sibling file: %q", got)
		}
	})

	t.Run("slash_clean_normalizes", func(t *testing.T) {
		// filepath.Clean("/" + path) normalizes all relative components.
		got, err := SafeJoin(root, "images/../../../../etc")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.HasPrefix(got, root) {
			t.Fatalf("SafeJoin escaped: %q", got)
		}
	})

	// sibling-directory prefix bypass (defense-in-depth).
	// If root is "/var/lib/lyeve/storage" and someone creates
	// "/var/lib/lyeve/storage-evil", a bare HasPrefix(joined, root)
	// would match both. The separator-appended check
	// HasPrefix(joined, root+Separator) closes that gap.
	// Not exploitable via filepath.Join (it always produces a path
	// under root), but the correct pattern prevents future misuse.
	t.Run("sibling_directory_guard", func(t *testing.T) {
		// Create a sibling directory whose name shares root's prefix.
		sibling := root + "-evil"
		if err := os.MkdirAll(sibling, 0o755); err != nil {
			t.Fatalf("mkdir sibling: %v", err)
		}

		// SafeJoin with a normal key stays under root: not the sibling.
		got, err := SafeJoin(root, "doc.txt")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := filepath.Join(root, "doc.txt")
		if got != want {
			t.Fatalf("SafeJoin = %q, want %q", got, want)
		}

		// Verify the result is genuinely under root, not the sibling.
		if !strings.HasPrefix(got, root+string(filepath.Separator)) && got != root {
			t.Fatalf("SafeJoin result escaped root: %q", got)
		}
	})
}

// Roots are usually written the way an operator writes them, and
// STORAGE_LOCAL_PATH defaults to "./uploads". filepath.Join cleans its result,
// so an uncleaned root would never prefix-match, and every upload would come
// back as a traversal attempt.
func TestSafeJoin_UncleanRoot(t *testing.T) {
	cases := []struct {
		name string
		root string
		want string
	}{
		{"dot slash prefix", "./uploads", filepath.Join("uploads", "media", "a.png")},
		{"trailing slash", "uploads/", filepath.Join("uploads", "media", "a.png")},
		{"redundant segments", "uploads/./sub/../", filepath.Join("uploads", "media", "a.png")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SafeJoin(tc.root, "media/a.png")
			if err != nil {
				t.Fatalf("SafeJoin(%q): %v", tc.root, err)
			}
			if got != tc.want {
				t.Errorf("SafeJoin(%q) = %q, want %q", tc.root, got, tc.want)
			}
		})
	}
}

// Cleaning the root must not open a traversal route out of it.
func TestSafeJoin_UncleanRootStillRejectsEscape(t *testing.T) {
	for _, rel := range []string{"../escape.png", "media/../../escape.png"} {
		if _, err := SafeJoin("./uploads", rel); err != nil && !errors.Is(err, ErrInvalidPath) {
			t.Errorf("SafeJoin(./uploads, %q) = %v, want nil or ErrInvalidPath", rel, err)
		} else if err == nil {
			got, _ := SafeJoin("./uploads", rel)
			if !strings.HasPrefix(got, "uploads"+string(filepath.Separator)) {
				t.Errorf("SafeJoin(./uploads, %q) = %q, escaped the root", rel, got)
			}
		}
	}
}
