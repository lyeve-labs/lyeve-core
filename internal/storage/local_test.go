package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

func TestLocal_Put_SymlinkEscapeRejected(t *testing.T) {
	root := t.TempDir()

	// Create a symlink inside root that points outside.
	outsideDir := t.TempDir()
	linkName := filepath.Join(root, "escape-link")
	if err := os.Symlink(outsideDir, linkName); err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	l, err := NewLocal(root, "")
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}

	// Key that traverses through the symlink: "escape-link/evil.txt"
	ctx := context.Background()
	_, err = l.Put(ctx, "escape-link/evil.txt", "text/plain", strings.NewReader("malicious"))
	if err == nil {
		t.Fatal("expected error for symlink escape key, got nil")
	}
	if !strings.Contains(err.Error(), "invalid path") {
		t.Errorf("expected ErrInvalidPath for symlink escape, got: %v", err)
	}

	// Verify the file was NOT created outside root.
	outsidePath := filepath.Join(outsideDir, "evil.txt")
	if _, statErr := os.Stat(outsidePath); statErr == nil {
		t.Errorf("file was written outside root: %s", outsidePath)
	}
}

func TestLocal_Put_IntermediateSymlinkEscapeRejected(t *testing.T) {
	root := t.TempDir()

	// Create a normal subdir, but put a symlink inside it.
	normalDir := filepath.Join(root, "media")
	if err := os.MkdirAll(normalDir, 0o755); err != nil {
		t.Fatalf("mkdir media: %v", err)
	}

	outsideDir := t.TempDir()
	linkDest := filepath.Join(normalDir, "escape")
	if err := os.Symlink(outsideDir, linkDest); err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	l, err := NewLocal(root, "")
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}

	ctx := context.Background()
	_, err = l.Put(ctx, "media/escape/bad.txt", "text/plain", strings.NewReader("bad"))
	if err == nil {
		t.Fatal("expected error for intermediate symlink escape key, got nil")
	}
	if !strings.Contains(err.Error(), "invalid path") {
		t.Errorf("expected ErrInvalidPath, got: %v", err)
	}
}

func TestLocal_Put_NormalWorks(t *testing.T) {
	root := t.TempDir()
	l, err := NewLocal(root, "")
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}

	ctx := context.Background()
	obj, err := l.Put(ctx, "images/photo.jpg", "image/jpeg", strings.NewReader("hello world"))
	if err != nil {
		t.Fatalf("Put failed: %v", err)
	}
	if obj.Key != "images/photo.jpg" {
		t.Errorf("Key = %q, want %q", obj.Key, "images/photo.jpg")
	}

	// Verify file exists on disk.
	dest := filepath.Join(root, "images", "photo.jpg")
	if _, statErr := os.Stat(dest); statErr != nil {
		t.Errorf("file not written: %v", statErr)
	}
}

func TestLocal_checkWriteJail_TargetExists(t *testing.T) {
	root := t.TempDir()
	// Create a file under root.
	testFile := filepath.Join(root, "exists.txt")
	if err := os.WriteFile(testFile, []byte("test"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	l := &Local{RootPath: root}

	// Should pass: file inside root.
	if err := l.checkWriteJail(testFile); err != nil {
		t.Errorf("unexpected error for file inside root: %v", err)
	}

	// Should fail: file outside root.
	outsideFile := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outsideFile, []byte("test"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	err := l.checkWriteJail(outsideFile)
	if err == nil {
		t.Error("expected error for file outside root, got nil")
	}
	if !errors.Is(err, core.ErrInvalidPath) {
		t.Errorf("expected ErrInvalidPath, got %v", err)
	}
}

func TestLocal_checkWriteJail_TargetNotExists(t *testing.T) {
	root := t.TempDir()
	l := &Local{RootPath: root}

	// File doesn't exist yet (typical before Put).
	dest := filepath.Join(root, "new-file.txt")

	if err := l.checkWriteJail(dest); err != nil {
		t.Errorf("unexpected error for non-existent file inside root: %v", err)
	}

	// File outside root, doesn't exist.
	outsideDest := filepath.Join(t.TempDir(), "new-outside.txt")
	err := l.checkWriteJail(outsideDest)
	if err == nil {
		t.Error("expected error for non-existent file outside root, got nil")
	}
}
