package storage

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		key     string
		wantErr bool
	}{
		{name: "simple key", key: "resumes/abc.pdf"},
		{name: "bare filename", key: "resume.pdf"},
		{name: "redundant separators are cleaned", key: "a//b/../c.pdf"},

		{name: "empty", key: "", wantErr: true},
		{name: "absolute path", key: "/etc/passwd", wantErr: true},
		{name: "parent traversal", key: "../escape.pdf", wantErr: true},
		{name: "embedded traversal", key: "resumes/../../escape.pdf", wantErr: true},
		{name: "windows separator", key: `a\b.pdf`, wantErr: true},
		{name: "null byte", key: "a\x00b.pdf", wantErr: true},
		{name: "dot", key: ".", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := ValidateKey(tc.key)
			if tc.wantErr && err == nil {
				t.Fatalf("ValidateKey(%q) = nil error, want a rejection", tc.key)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ValidateKey(%q) error = %v", tc.key, err)
			}
		})
	}
}

func TestLocalStorageRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	store, err := NewLocal(root)
	if err != nil {
		t.Fatalf("NewLocal() error = %v", err)
	}

	payload := []byte("the original bytes")
	path, err := store.Put(ctx, "resumes/abc.pdf", payload)
	if err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	if !strings.HasPrefix(path, root) {
		t.Errorf("Put() wrote to %q, want a path under %q", path, root)
	}

	reader, err := store.Open(ctx, "resumes/abc.pdf")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = reader.Close() }()

	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("reading object: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("read %q, want %q", got, payload)
	}

	if err := store.Delete(ctx, "resumes/abc.pdf"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := store.Open(ctx, "resumes/abc.pdf"); err != ErrNotFound {
		t.Errorf("Open() after Delete error = %v, want ErrNotFound", err)
	}
	// Deleting something that is already gone is not an error.
	if err := store.Delete(ctx, "resumes/abc.pdf"); err != nil {
		t.Errorf("Delete() on a missing object error = %v, want nil", err)
	}
}

func TestLocalStorageOverwritesAtomically(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal() error = %v", err)
	}

	if _, err := store.Put(ctx, "a/b.txt", []byte("first")); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	if _, err := store.Put(ctx, "a/b.txt", []byte("second")); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	reader, err := store.Open(ctx, "a/b.txt")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = reader.Close() }()

	got, _ := io.ReadAll(reader)
	if string(got) != "second" {
		t.Errorf("content = %q, want %q", got, "second")
	}

	// No temporary files may be left behind.
	entries, err := os.ReadDir(filepath.Join(store.Root(), "a"))
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".tmp-") {
			t.Errorf("temporary file %q was left behind", entry.Name())
		}
	}
}

func TestPutReaderEnforcesLimit(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal() error = %v", err)
	}

	oversized := bytes.Repeat([]byte("x"), 100)
	if _, err := store.PutReader(ctx, "big.bin", bytes.NewReader(oversized), 50); err != ErrTooLarge {
		t.Fatalf("PutReader() error = %v, want ErrTooLarge", err)
	}

	// A rejected write must not leave a partial object behind.
	if ok, _ := store.Exists(ctx, "big.bin"); ok {
		t.Error("an oversized object was committed to storage")
	}

	if _, err := store.PutReader(ctx, "ok.bin", bytes.NewReader(oversized), 100); err != nil {
		t.Fatalf("PutReader() at the limit error = %v", err)
	}
}

func TestPathRejectsEscapingKeys(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store, err := NewLocal(root)
	if err != nil {
		t.Fatalf("NewLocal() error = %v", err)
	}

	for _, key := range []string{"../outside.txt", "/etc/passwd", "a/../../outside.txt"} {
		if _, err := store.Path(key); err == nil {
			t.Errorf("Path(%q) = nil error, want the key to be rejected", key)
		}
	}

	path, err := store.Path("resumes/abc.pdf")
	if err != nil {
		t.Fatalf("Path() error = %v", err)
	}
	if !strings.HasPrefix(path, root) {
		t.Errorf("Path() = %q, want a path under %q", path, root)
	}
}

func TestExists(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal() error = %v", err)
	}

	if ok, err := store.Exists(ctx, "missing.pdf"); err != nil || ok {
		t.Errorf("Exists() = %v, %v; want false, nil", ok, err)
	}
	if _, err := store.Put(ctx, "present.pdf", []byte("x")); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	if ok, err := store.Exists(ctx, "present.pdf"); err != nil || !ok {
		t.Errorf("Exists() = %v, %v; want true, nil", ok, err)
	}
}
