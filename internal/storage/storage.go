// Package storage abstracts persistence of binary artefacts (uploaded resumes
// and generated PDFs) behind a narrow interface so the local filesystem can be
// swapped for object storage later without touching business logic.
package storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Storage stores and retrieves binary objects by opaque key.
type Storage interface {
	// Put writes data under key, overwriting any existing object, and returns
	// the stored location.
	Put(ctx context.Context, key string, data []byte) (string, error)
	// PutReader streams r under key, failing if the stream exceeds limit.
	PutReader(ctx context.Context, key string, r io.Reader, limit int64) (string, error)
	// Open returns a reader for the object. The caller closes it.
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	// Exists reports whether the object exists.
	Exists(ctx context.Context, key string) (bool, error)
	// Delete removes the object; removing a missing object is not an error.
	Delete(ctx context.Context, key string) error
}

// LocalStorage is a Storage backed by a single directory on disk.
type LocalStorage struct {
	root string
}

// NewLocal creates the root directory if needed and returns a LocalStorage.
func NewLocal(root string) (*LocalStorage, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("storage: resolve root %q: %w", root, err)
	}
	if err := os.MkdirAll(abs, 0o750); err != nil {
		return nil, fmt.Errorf("storage: create root %q: %w", abs, err)
	}
	return &LocalStorage{root: abs}, nil
}

// Root returns the absolute directory backing this storage.
func (s *LocalStorage) Root() string { return s.root }

// Path resolves a key to an absolute filesystem path.
//
// It is deliberately not part of the Storage interface: an object-store backend
// has no local path, and pretending otherwise would leak the filesystem into
// the abstraction. Components that genuinely need a path — the resume text
// extractor, which opens a file by name — depend on this concrete method.
func (s *LocalStorage) Path(key string) (string, error) {
	return s.resolve(key)
}

// Put implements Storage.
func (s *LocalStorage) Put(ctx context.Context, key string, data []byte) (string, error) {
	path, err := s.resolve(key)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return "", fmt.Errorf("storage: create directory for %q: %w", key, err)
	}

	// Write to a temporary file first so a crash mid-write cannot leave a
	// truncated artefact that later looks complete.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return "", fmt.Errorf("storage: create temp file for %q: %w", key, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op after a successful rename

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("storage: write %q: %w", key, err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("storage: close %q: %w", key, err)
	}
	if err := os.Chmod(tmpName, 0o640); err != nil {
		return "", fmt.Errorf("storage: chmod %q: %w", key, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return "", fmt.Errorf("storage: commit %q: %w", key, err)
	}
	return path, nil
}

// PutReader implements Storage.
func (s *LocalStorage) PutReader(ctx context.Context, key string, r io.Reader, limit int64) (string, error) {
	path, err := s.resolve(key)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return "", fmt.Errorf("storage: create directory for %q: %w", key, err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return "", fmt.Errorf("storage: create temp file for %q: %w", key, err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()

	// Read one byte past the limit so an oversized body is detected rather than
	// silently truncated.
	var src io.Reader = r
	if limit > 0 {
		src = io.LimitReader(r, limit+1)
	}
	written, err := io.Copy(tmp, src)
	if err != nil {
		return "", fmt.Errorf("storage: write %q: %w", key, err)
	}
	if limit > 0 && written > limit {
		return "", ErrTooLarge
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("storage: close %q: %w", key, err)
	}
	if err := os.Chmod(tmpName, 0o640); err != nil {
		return "", fmt.Errorf("storage: chmod %q: %w", key, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return "", fmt.Errorf("storage: commit %q: %w", key, err)
	}
	committed = true
	return path, nil
}

// Open implements Storage.
func (s *LocalStorage) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	path, err := s.resolve(key)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("storage: open %q: %w", key, err)
	}
	return f, nil
}

// Exists implements Storage.
func (s *LocalStorage) Exists(ctx context.Context, key string) (bool, error) {
	path, err := s.resolve(key)
	if err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("storage: stat %q: %w", key, err)
	}
	return true, nil
}

// Delete implements Storage.
func (s *LocalStorage) Delete(ctx context.Context, key string) error {
	path, err := s.resolve(key)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("storage: delete %q: %w", key, err)
	}
	return nil
}

// resolve maps a key to an absolute path, rejecting anything that would escape
// the storage root.
func (s *LocalStorage) resolve(key string) (string, error) {
	clean, err := ValidateKey(key)
	if err != nil {
		return "", err
	}
	path := filepath.Join(s.root, filepath.FromSlash(clean))
	// Defence in depth: even after cleaning, confirm we stayed inside the root.
	if path != s.root && !strings.HasPrefix(path, s.root+string(os.PathSeparator)) {
		return "", fmt.Errorf("storage: key %q escapes storage root", key)
	}
	return path, nil
}

// ValidateKey rejects keys that are empty, absolute, or contain traversal or
// path-separator tricks.
func ValidateKey(key string) (string, error) {
	if key == "" {
		return "", fmt.Errorf("storage: empty key")
	}
	if strings.ContainsRune(key, 0) {
		return "", fmt.Errorf("storage: key contains a null byte")
	}
	if strings.HasPrefix(key, "/") || strings.Contains(key, `\`) {
		return "", fmt.Errorf("storage: key %q must be a relative slash-separated path", key)
	}
	clean := filepath.ToSlash(filepath.Clean(key))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("storage: key %q escapes storage root", key)
	}
	return clean, nil
}

// Errors returned by LocalStorage that callers may want to distinguish.
var (
	ErrNotFound = fmt.Errorf("storage: object not found")
	ErrTooLarge = fmt.Errorf("storage: object exceeds size limit")
)
