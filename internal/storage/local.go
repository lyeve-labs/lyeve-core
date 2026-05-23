package storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// Local is a Storage driver that saves objects to the local filesystem.
// All keys are joined under RootPath. SignedURL is not supported.
type Local struct {
	RootPath string
	// BaseURL is the public URL prefix served by the HTTP media handler,
	// e.g. "https://example.com/media". Used as the base for object URLs.
	BaseURL string
}

// NewLocal creates a new Local storage driver rooted at rootPath.
func NewLocal(rootPath, baseURL string) (*Local, error) {
	if err := os.MkdirAll(rootPath, 0o755); err != nil {
		return nil, fmt.Errorf("storage/local: mkdir %s: %w", rootPath, err)
	}
	return &Local{RootPath: rootPath, BaseURL: baseURL}, nil
}

// Put writes the contents of r under key, creating any required directories.
// Returns a StorageObject describing the stored file.
func (l *Local) Put(ctx context.Context, key, contentType string, r io.Reader) (*core.StorageObject, error) {
	dest, err := l.abs(key)
	if err != nil {
		return nil, fmt.Errorf("storage/local put: %w", err)
	}
	if err = os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return nil, fmt.Errorf("storage/local put mkdir: %w", err)
	}

	// Defense-in-depth: resolve symlinks in the parent directory and verify
	// the resolved path stays under RootPath. This catches symlink escapes
	// even when the key itself contains no ".." components.
	if err := l.checkWriteJail(dest); err != nil {
		return nil, fmt.Errorf("storage/local put: %w", err)
	}

	f, err := os.Create(dest) //nolint:gosec // path validated by checkWriteJail
	if err != nil {
		return nil, fmt.Errorf("storage/local put create: %w", err)
	}
	n, err := io.Copy(f, r)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("storage/local put write: %w", err)
	}
	info, _ := f.Stat()
	var mod time.Time
	if info != nil {
		mod = info.ModTime()
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("storage/local put close: %w", err)
	}
	return &core.StorageObject{Key: key, ContentType: contentType, Size: n, LastModified: mod}, nil
}

// Get opens the object stored under key and returns a read-closer together with
// its metadata. The caller must close the returned reader.
func (l *Local) Get(ctx context.Context, key string) (io.ReadCloser, *core.StorageObject, error) {
	path, err := l.abs(key)
	if err != nil {
		return nil, nil, fmt.Errorf("storage/local get: %w", err)
	}
	f, err := os.Open(path) //nolint:gosec
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, fmt.Errorf("storage/local get: key not found: %s", key)
		}
		return nil, nil, fmt.Errorf("storage/local get: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	obj := &core.StorageObject{Key: key, Size: info.Size(), LastModified: info.ModTime()}
	return f, obj, nil
}

// Delete removes the object at key. Returns nil when the key does not exist.
func (l *Local) Delete(ctx context.Context, key string) error {
	absKey, err := l.abs(key)
	if err != nil {
		return fmt.Errorf("storage/local delete: %w", err)
	}
	err = os.Remove(absKey)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// SignedURL returns a public URL for key using BaseURL as the prefix.
// Returns an empty string when BaseURL is not configured.
func (l *Local) SignedURL(_ context.Context, key string, _ time.Duration) (string, error) {
	if l.BaseURL == "" {
		return "", nil
	}
	return strings.TrimRight(l.BaseURL, "/") + "/" + key, nil
}

// List returns objects whose keys start with prefix, up to limit results.
// limit ≤ 0 returns all matching objects. An empty prefix lists the entire
// root tree.
func (l *Local) List(ctx context.Context, prefix string, limit int) ([]*core.StorageObject, error) {
	var root string
	var err error
	if prefix == "" {
		root = l.RootPath
	} else {
		root, err = core.SafeJoin(l.RootPath, filepath.FromSlash(prefix))
		if err != nil {
			return nil, fmt.Errorf("storage/local list: %w", err)
		}
	}
	var objects []*core.StorageObject
	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(l.RootPath, path)
		key := filepath.ToSlash(rel)
		objects = append(objects, &core.StorageObject{
			Key:          key,
			Size:         info.Size(),
			LastModified: info.ModTime(),
		})
		if limit > 0 && len(objects) >= limit {
			return filepath.SkipAll
		}
		return nil
	})
	return objects, err
}

func (l *Local) abs(key string) (string, error) {
	return core.SafeJoin(l.RootPath, key)
}

// checkWriteJail resolves all symlinks in the path leading to the file,
// then verifies the resolved absolute path has RootPath as a prefix.
// This catches symlink-based escapes that SafeJoin alone cannot detect.
func (l *Local) checkWriteJail(target string) error {
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		// If the target file doesn't exist yet (typical for Put), resolve
		// the parent directory instead and join the filename.
		if os.IsNotExist(err) {
			dir := filepath.Dir(target)
			base := filepath.Base(target)
			resolvedDir, dirErr := filepath.EvalSymlinks(dir)
			if dirErr != nil {
				return fmt.Errorf("path resolution failed: %w", dirErr)
			}
			resolved = filepath.Join(resolvedDir, base)
		} else {
			return fmt.Errorf("path resolution failed: %w", err)
		}
	}
	// Normalize separators for prefix check.
	cleanRoot := filepath.Clean(l.RootPath)
	cleanResolved := filepath.Clean(resolved)
	if !strings.HasPrefix(cleanResolved, cleanRoot+string(filepath.Separator)) &&
		cleanResolved != cleanRoot {
		return core.ErrInvalidPath
	}
	return nil
}
