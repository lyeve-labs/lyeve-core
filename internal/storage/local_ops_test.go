package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// errReader always fails on Read, used to exercise the io.Copy error path.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

func TestLocal_Get_RoundTrip(t *testing.T) {
	root := t.TempDir()
	l, err := NewLocal(root, "")
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	ctx := context.Background()

	want := []byte("round-trip payload")
	if _, err := l.Put(ctx, "docs/readme.txt", "text/plain", bytes.NewReader(want)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	rc, obj, err := l.Get(ctx, "docs/readme.txt")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("Get bytes = %q, want %q", got, want)
	}
	if obj.Key != "docs/readme.txt" {
		t.Errorf("obj.Key = %q, want %q", obj.Key, "docs/readme.txt")
	}
	if obj.Size != int64(len(want)) {
		t.Errorf("obj.Size = %d, want %d", obj.Size, len(want))
	}
}

func TestLocal_Get_Overwrite(t *testing.T) {
	root := t.TempDir()
	l, _ := NewLocal(root, "")
	ctx := context.Background()

	if _, err := l.Put(ctx, "file.txt", "text/plain", strings.NewReader("first")); err != nil {
		t.Fatalf("Put first: %v", err)
	}
	if _, err := l.Put(ctx, "file.txt", "text/plain", strings.NewReader("second-longer")); err != nil {
		t.Fatalf("Put second: %v", err)
	}

	rc, obj, err := l.Get(ctx, "file.txt")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()

	got, _ := io.ReadAll(rc)
	if string(got) != "second-longer" {
		t.Errorf("overwrite content = %q, want %q", got, "second-longer")
	}
	if obj.Size != int64(len("second-longer")) {
		t.Errorf("overwrite size = %d, want %d", obj.Size, len("second-longer"))
	}
}

func TestLocal_Get_NotFound(t *testing.T) {
	l, _ := NewLocal(t.TempDir(), "")
	_, _, err := l.Get(context.Background(), "does/not/exist.txt")
	if err == nil {
		t.Fatal("expected error for missing key, got nil")
	}
	if !strings.Contains(err.Error(), "key not found") {
		t.Errorf("expected 'key not found', got: %v", err)
	}
}

func TestLocal_Get_InvalidKey(t *testing.T) {
	l, _ := NewLocal(t.TempDir(), "")
	_, _, err := l.Get(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for empty key, got nil")
	}
	if !errors.Is(err, core.ErrInvalidPath) {
		t.Errorf("expected ErrInvalidPath, got: %v", err)
	}
}

func TestLocal_Delete(t *testing.T) {
	root := t.TempDir()
	l, _ := NewLocal(root, "")
	ctx := context.Background()

	if _, err := l.Put(ctx, "a/b.txt", "text/plain", strings.NewReader("data")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Delete existing -> nil, file removed from disk.
	if err := l.Delete(ctx, "a/b.txt"); err != nil {
		t.Fatalf("Delete existing: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "a", "b.txt")); !os.IsNotExist(statErr) {
		t.Errorf("file still present after delete: stat err = %v", statErr)
	}

	// Delete-then-Get -> not found.
	if _, _, err := l.Get(ctx, "a/b.txt"); err == nil || !strings.Contains(err.Error(), "key not found") {
		t.Errorf("expected 'key not found' after delete, got: %v", err)
	}

	// Delete missing key -> nil (Storage interface contract: no error on missing).
	if err := l.Delete(ctx, "a/b.txt"); err != nil {
		t.Errorf("Delete missing key should return nil, got: %v", err)
	}
}

func TestLocal_Delete_InvalidKey(t *testing.T) {
	l, _ := NewLocal(t.TempDir(), "")
	err := l.Delete(context.Background(), "")
	if !errors.Is(err, core.ErrInvalidPath) {
		t.Errorf("expected ErrInvalidPath for empty key, got: %v", err)
	}
}

func TestLocal_SignedURL(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		key     string
		want    string
	}{
		{"no base url returns empty", "", "images/x.png", ""},
		{"no trailing slash", "https://cdn.example.com/media", "images/x.png", "https://cdn.example.com/media/images/x.png"},
		{"trailing slash trimmed", "https://cdn.example.com/media/", "images/x.png", "https://cdn.example.com/media/images/x.png"},
		{"multiple trailing slashes trimmed", "https://cdn.example.com/media///", "a.txt", "https://cdn.example.com/media/a.txt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := &Local{RootPath: t.TempDir(), BaseURL: tt.baseURL}
			got, err := l.SignedURL(context.Background(), tt.key, time.Hour)
			if err != nil {
				t.Fatalf("SignedURL: %v", err)
			}
			if got != tt.want {
				t.Errorf("SignedURL = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLocal_List(t *testing.T) {
	root := t.TempDir()
	l, _ := NewLocal(root, "")
	ctx := context.Background()

	// Content equals the key string, so on-disk size == len(key).
	seed := []string{"a/1.txt", "a/2.txt", "b/3.txt"}
	for _, k := range seed {
		if _, err := l.Put(ctx, k, "text/plain", strings.NewReader(k)); err != nil {
			t.Fatalf("Put %s: %v", k, err)
		}
	}

	// Empty prefix lists the entire tree (files only, not directories).
	all, err := l.List(ctx, "", 0)
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("List all len = %d, want 3", len(all))
	}
	sizes := map[string]int64{}
	for _, o := range all {
		sizes[o.Key] = o.Size
	}
	for _, k := range seed {
		if size, ok := sizes[k]; !ok {
			t.Errorf("missing key %q in listing %v", k, sizes)
		} else if size != int64(len(k)) {
			t.Errorf("key %q size = %d, want %d", k, size, len(k))
		}
	}

	// Prefix filter returns only matching keys.
	aOnly, err := l.List(ctx, "a", 0)
	if err != nil {
		t.Fatalf("List prefix a: %v", err)
	}
	if len(aOnly) != 2 {
		t.Errorf("List 'a' len = %d, want 2", len(aOnly))
	}
	for _, o := range aOnly {
		if !strings.HasPrefix(o.Key, "a/") {
			t.Errorf("prefix listing leaked key %q", o.Key)
		}
	}

	// A positive limit caps the number of results.
	limited, err := l.List(ctx, "", 1)
	if err != nil {
		t.Fatalf("List limit: %v", err)
	}
	if len(limited) != 1 {
		t.Errorf("List limit=1 len = %d, want 1", len(limited))
	}
}

func TestLocal_List_NonexistentPrefix(t *testing.T) {
	l, _ := NewLocal(t.TempDir(), "")
	// A prefix that resolves under root but has no files walks an absent
	// directory. Walk swallows the lstat error, so List returns empty, nil.
	objs, err := l.List(context.Background(), "nope/missing", 0)
	if err != nil {
		t.Fatalf("List nonexistent prefix: %v", err)
	}
	if len(objs) != 0 {
		t.Errorf("expected empty listing, got %d objects", len(objs))
	}
}

func TestLocal_Put_InvalidKey(t *testing.T) {
	l, _ := NewLocal(t.TempDir(), "")
	_, err := l.Put(context.Background(), "", "text/plain", strings.NewReader("x"))
	if !errors.Is(err, core.ErrInvalidPath) {
		t.Errorf("expected ErrInvalidPath for empty key, got: %v", err)
	}
}

func TestLocal_Put_ReaderError(t *testing.T) {
	l, _ := NewLocal(t.TempDir(), "")
	_, err := l.Put(context.Background(), "x.bin", "application/octet-stream", errReader{})
	if err == nil {
		t.Fatal("expected error from failing reader, got nil")
	}
	if !strings.Contains(err.Error(), "put write") {
		t.Errorf("expected 'put write' error, got: %v", err)
	}
}

func TestLocal_Put_DestIsDirectory(t *testing.T) {
	root := t.TempDir()
	l, _ := NewLocal(root, "")
	// Pre-create the key path as a directory so os.Create fails.
	if err := os.MkdirAll(filepath.Join(root, "collide"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	_, err := l.Put(context.Background(), "collide", "text/plain", strings.NewReader("x"))
	if err == nil {
		t.Fatal("expected error when key path is an existing directory, got nil")
	}
	if !strings.Contains(err.Error(), "put create") {
		t.Errorf("expected 'put create' error, got: %v", err)
	}
}

func TestLocal_Put_ParentIsFile(t *testing.T) {
	root := t.TempDir()
	l, _ := NewLocal(root, "")
	// A regular file cannot act as a parent directory in the key path.
	if err := os.WriteFile(filepath.Join(root, "afile"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	_, err := l.Put(context.Background(), "afile/child.txt", "text/plain", strings.NewReader("x"))
	if err == nil {
		t.Fatal("expected error when parent path is a file, got nil")
	}
	if !strings.Contains(err.Error(), "put mkdir") {
		t.Errorf("expected 'put mkdir' error, got: %v", err)
	}
}

func TestNewLocal_MkdirError(t *testing.T) {
	tmp := t.TempDir()
	fileAsParent := filepath.Join(tmp, "notadir")
	if err := os.WriteFile(fileAsParent, []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	// Rooting under an existing file makes MkdirAll fail (ENOTDIR).
	_, err := NewLocal(filepath.Join(fileAsParent, "sub"), "")
	if err == nil {
		t.Fatal("expected error creating root under a file, got nil")
	}
	if !strings.Contains(err.Error(), "mkdir") {
		t.Errorf("expected 'mkdir' error, got: %v", err)
	}
}
