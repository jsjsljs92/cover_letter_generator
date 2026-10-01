package resume

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"coverletter/internal/apperror"
	"coverletter/internal/logging"
	"coverletter/internal/obs"
	"coverletter/internal/storage"
)

const defaultMaxSize = 5 << 20 // 5 MiB

// newTestService wires the real LocalStorage, PathResolver and PDFParser
// against a fresh temp directory. No fakes: the traversal, size-limit and
// content-sniffing guarantees live in the collaboration between these pieces,
// so mocking any of them would test nothing.
func newTestService(t *testing.T, maxSize int64) (*Service, *storage.LocalStorage) {
	t.Helper()

	store, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("storage.NewLocal() error = %v", err)
	}
	// *LocalStorage supplies both the Storage and the PathResolver role.
	svc := NewService(store, store, NewPDFParser(0), maxSize, obs.New(), logging.Discard())
	return svc, store
}

// countObjects reports how many regular files ended up under root, so a
// rejected upload can be shown to have left nothing behind.
func countObjects(t *testing.T, root string) int {
	t.Helper()

	count := 0
	err := filepath.WalkDir(root, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking storage root: %v", err)
	}
	return count
}

func TestServiceStore(t *testing.T) {
	t.Parallel()

	pdfBytes := buildPDF(t, resumeParagraphs)
	oversized := bytes.Repeat([]byte("x"), 4096)
	// A DOCX is a ZIP: the real thing, not a text file, so this proves the
	// rejection comes from sniffing rather than from being obviously plain.
	docx := append([]byte("PK\x03\x04\x14\x00\x06\x00"), bytes.Repeat([]byte{0x00}, 600)...)

	tests := []struct {
		name     string
		filename string
		body     []byte
		nilBody  bool
		maxSize  int64

		wantErr  bool
		wantCode string
		// wantFilename is the sanitised filename expected in the result.
		wantFilename string
	}{
		{
			name:         "valid PDF upload",
			filename:     "Ada_Lovelace_Resume.pdf",
			body:         pdfBytes,
			wantFilename: "Ada_Lovelace_Resume.pdf",
		},
		{
			name:         "traversal filename cannot steer the storage key",
			filename:     "../../evil.pdf",
			body:         pdfBytes,
			wantFilename: "evil.pdf",
		},
		{
			name:         "windows traversal filename",
			filename:     `..\..\windows\system32\evil.pdf`,
			body:         pdfBytes,
			wantFilename: "evil.pdf",
		},
		{
			name:         "absolute path filename",
			filename:     "/etc/passwd",
			body:         pdfBytes,
			wantFilename: "passwd",
		},
		{
			name:         "mismatched extension is ignored in favour of content",
			filename:     "notes.docx",
			body:         pdfBytes,
			wantFilename: "notes.docx",
		},
		{
			name:         "empty filename falls back",
			filename:     "",
			body:         pdfBytes,
			wantFilename: "resume.pdf",
		},
		{
			name:         "control characters in the filename are stripped",
			filename:     "re\x00sume\n.pdf",
			body:         pdfBytes,
			wantFilename: "resume.pdf",
		},
		{
			name:     "text payload disguised with a .pdf filename is rejected",
			filename: "totally-a-resume.pdf",
			body:     []byte("Dear hiring manager, I hope this finds you well. " + strings.Repeat("padding ", 40)),
			wantErr:  true,
			wantCode: apperror.CodeUnsupportedType,
		},
		{
			name:     "a real DOCX renamed to .pdf is rejected",
			filename: "resume.pdf",
			body:     docx,
			wantErr:  true,
			wantCode: apperror.CodeUnsupportedType,
		},
		{
			name:     "PNG header bytes are rejected",
			filename: "resume.pdf",
			body:     append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0x00}, 600)...),
			wantErr:  true,
			wantCode: apperror.CodeUnsupportedType,
		},
		{
			name:     "empty payload is rejected",
			filename: "resume.pdf",
			body:     nil,
			wantErr:  true,
			wantCode: apperror.CodeUnsupportedType,
		},
		{
			name:     "payload shorter than the PDF magic is rejected",
			filename: "resume.pdf",
			body:     []byte("%PD"),
			wantErr:  true,
			wantCode: apperror.CodeUnsupportedType,
		},
		{
			name:     "nil reader is rejected",
			filename: "resume.pdf",
			nilBody:  true,
			wantErr:  true,
			wantCode: apperror.CodeInvalidResume,
		},
		{
			name:     "payload exceeding the size limit is rejected",
			filename: "resume.pdf",
			body:     append(pdfBytes, oversized...),
			maxSize:  2048,
			wantErr:  true,
			wantCode: apperror.CodePayloadTooLarge,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			maxSize := tc.maxSize
			if maxSize == 0 {
				maxSize = defaultMaxSize
			}
			svc, store := newTestService(t, maxSize)

			var reader io.Reader
			if !tc.nilBody {
				reader = bytes.NewReader(tc.body)
			}
			stored, err := svc.Store(context.Background(), tc.filename, reader)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("Store(%q) = %+v, want a rejection", tc.filename, stored)
				}
				if !apperror.Is(err, tc.wantCode) {
					t.Errorf("Store() error = %v, want code %s", err, tc.wantCode)
				}
				if got := apperror.Status(err); got != 400 {
					t.Errorf("Store() error status = %d, want 400", got)
				}
				// A rejected upload must not leave an artefact behind, which
				// is what makes the size limit an actual bound on disk usage.
				if n := countObjects(t, store.Root()); n != 0 {
					t.Errorf("storage holds %d object(s) after a rejected upload, want 0", n)
				}
				return
			}

			if err != nil {
				t.Fatalf("Store(%q) error = %v", tc.filename, err)
			}
			if stored.Filename != tc.wantFilename {
				t.Errorf("Stored.Filename = %q, want %q", stored.Filename, tc.wantFilename)
			}

			// The key must be server-generated: a client-supplied name never
			// influences where the bytes land.
			const prefix = "resumes/"
			const suffix = ".pdf"
			if !strings.HasPrefix(stored.StorageKey, prefix) || !strings.HasSuffix(stored.StorageKey, suffix) {
				t.Fatalf("StorageKey = %q, want %q<random-id>%q", stored.StorageKey, prefix, suffix)
			}
			idPart := strings.TrimSuffix(strings.TrimPrefix(stored.StorageKey, prefix), suffix)
			if len(idPart) != 16 {
				t.Errorf("StorageKey id %q is %d chars, want a 16-char generated id", idPart, len(idPart))
			}
			for _, r := range idPart {
				if !strings.ContainsRune("0123456789abcdef", r) {
					t.Errorf("StorageKey id %q contains %q, want lowercase hex only", idPart, r)
				}
			}

			// The stored path must stay inside the storage root regardless of
			// what the client called the file.
			if !strings.HasPrefix(stored.Path, store.Root()+string(os.PathSeparator)) {
				t.Errorf("Stored.Path = %q, want it under %q", stored.Path, store.Root())
			}
			if stored.Path != filepath.Join(store.Root(), filepath.FromSlash(stored.StorageKey)) {
				t.Errorf("Stored.Path = %q, want it to match the storage key", stored.Path)
			}

			data, readErr := os.ReadFile(stored.Path)
			if readErr != nil {
				t.Fatalf("reading stored object: %v", readErr)
			}
			if !bytes.HasPrefix(data, []byte("%PDF-")) {
				t.Errorf("stored object starts with %q, want the %%PDF- magic", data[:min(8, len(data))])
			}
			// The sniffed header must be replayed at the head of the stream,
			// not swallowed: a truncated file would pass the magic check and
			// then fail to parse later.
			if !bytes.Equal(data, tc.body) {
				t.Errorf("stored object is %d bytes, want the %d bytes uploaded", len(data), len(tc.body))
			}
			if stored.Bytes != int64(len(tc.body)) {
				t.Errorf("Stored.Bytes = %d, want %d", stored.Bytes, len(tc.body))
			}
		})
	}
}

func TestServiceStoreGeneratesDistinctKeys(t *testing.T) {
	t.Parallel()

	svc, _ := newTestService(t, defaultMaxSize)
	body := buildPDF(t, resumeParagraphs)

	seen := make(map[string]bool)
	for i := 0; i < 8; i++ {
		stored, err := svc.Store(context.Background(), "same-name.pdf", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("Store() error = %v", err)
		}
		if seen[stored.StorageKey] {
			t.Fatalf("Store() reused storage key %q; a second upload would overwrite the first", stored.StorageKey)
		}
		seen[stored.StorageKey] = true
	}
}

func TestServiceStoreCountsRejections(t *testing.T) {
	t.Parallel()

	svc, _ := newTestService(t, defaultMaxSize)
	metrics := svc.metrics

	stored, err := svc.Store(context.Background(), "ok.pdf", bytes.NewReader(buildPDF(t, resumeParagraphs)))
	if err != nil {
		t.Fatalf("Store() error = %v", err)
	}
	if _, err := svc.Store(context.Background(), "bad.pdf", bytes.NewReader([]byte("not a pdf at all"))); err == nil {
		t.Fatal("Store() = nil error, want a non-PDF to be rejected")
	}

	if got := metrics.Counter("resume.store.succeeded"); got != 1 {
		t.Errorf("resume.store.succeeded = %d, want 1", got)
	}
	if got := metrics.Counter("resume.store.rejected"); got != 1 {
		t.Errorf("resume.store.rejected = %d, want 1", got)
	}

	if _, err := svc.Parse(context.Background(), stored); err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if got := metrics.Counter("resume.parse.succeeded"); got != 1 {
		t.Errorf("resume.parse.succeeded = %d, want 1", got)
	}
	if got := metrics.Value("resume.parse.duration").Count; got != 1 {
		t.Errorf("resume.parse.duration count = %d, want 1", got)
	}
}

func TestServiceParse(t *testing.T) {
	t.Parallel()

	svc, _ := newTestService(t, defaultMaxSize)
	ctx := context.Background()

	stored, err := svc.Store(ctx, "Grace_Hopper.pdf", bytes.NewReader(buildPDF(t, resumeParagraphs)))
	if err != nil {
		t.Fatalf("Store() error = %v", err)
	}

	resumed, err := svc.Parse(ctx, stored)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if resumed.Text == "" {
		t.Fatal("Parse() returned empty text for a valid PDF")
	}
	for _, word := range []string{"Grace", "Hopper", "Kubernetes", "compiler"} {
		if !strings.Contains(resumed.Text, word) {
			t.Errorf("extracted text does not contain %q; got:\n%s", word, resumed.Text)
		}
	}
	// Parse must carry the stored descriptor's metadata through, since the
	// filename and size the caller showed the user must match what was
	// actually persisted.
	if resumed.Filename != stored.Filename {
		t.Errorf("Resume.Filename = %q, want %q", resumed.Filename, stored.Filename)
	}
	if resumed.Bytes != stored.Bytes {
		t.Errorf("Resume.Bytes = %d, want %d", resumed.Bytes, stored.Bytes)
	}
	if resumed.Path != stored.Path {
		t.Errorf("Resume.Path = %q, want %q", resumed.Path, stored.Path)
	}
}

func TestServiceParseRejectsBadInput(t *testing.T) {
	t.Parallel()

	t.Run("nil stored descriptor", func(t *testing.T) {
		t.Parallel()
		svc, _ := newTestService(t, defaultMaxSize)
		if _, err := svc.Parse(context.Background(), nil); err == nil {
			t.Fatal("Parse(nil) = nil error, want a rejection")
		}
	})

	t.Run("stored object that is not a PDF", func(t *testing.T) {
		t.Parallel()
		svc, store := newTestService(t, defaultMaxSize)
		ctx := context.Background()

		// A PDF that was valid on upload can be replaced underneath us, or a
		// queued job can point at a key that no longer holds what we wrote.
		// Parse must re-validate rather than trust the Stored descriptor.
		path, err := store.Put(ctx, "resumes/deadbeefdeadbeef.pdf", []byte("this is not a pdf"))
		if err != nil {
			t.Fatalf("Put() error = %v", err)
		}
		stored := &Stored{Path: path, StorageKey: "resumes/deadbeefdeadbeef.pdf", Filename: "resume.pdf", Bytes: 17}

		if _, err := svc.Parse(ctx, stored); err == nil {
			t.Fatal("Parse() = nil error, want a non-PDF object to be rejected")
		}
	})

	t.Run("missing stored object", func(t *testing.T) {
		t.Parallel()
		svc, _ := newTestService(t, defaultMaxSize)
		stored := &Stored{Path: filepath.Join(t.TempDir(), "gone.pdf"), StorageKey: "resumes/gone.pdf"}
		if _, err := svc.Parse(context.Background(), stored); err == nil {
			t.Fatal("Parse() = nil error, want a missing object to be reported")
		}
	})
}

func TestStoredFromKey(t *testing.T) {
	t.Parallel()

	svc, store := newTestService(t, defaultMaxSize)
	ctx := context.Background()

	path, err := store.Put(ctx, "resumes/cafe0000deadbeef.pdf", buildPDF(t, resumeParagraphs))
	if err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	got, err := svc.StoredFromKey("resumes/cafe0000deadbeef.pdf", "../../evil.pdf", 42)
	if err != nil {
		t.Fatalf("StoredFromKey() error = %v", err)
	}
	if got.Path != path {
		t.Errorf("Stored.Path = %q, want %q", got.Path, path)
	}
	// The filename is rebuilt from untrusted input on the resume path too,
	// so it must be sanitised here as well.
	if got.Filename != "evil.pdf" {
		t.Errorf("Stored.Filename = %q, want %q", got.Filename, "evil.pdf")
	}
	if got.Bytes != 42 {
		t.Errorf("Stored.Bytes = %d, want 42", got.Bytes)
	}
}

func TestStoredFromKeyRejectsEscapingKey(t *testing.T) {
	t.Parallel()

	svc, _ := newTestService(t, defaultMaxSize)

	for _, key := range []string{"../outside.pdf", "/etc/passwd", "resumes/../../outside.pdf", ""} {
		if _, err := svc.StoredFromKey(key, "resume.pdf", 1); err == nil {
			t.Errorf("StoredFromKey(%q) = nil error, want the key to be rejected", key)
		}
	}
}

func TestStoredFromKeyWithoutResolver(t *testing.T) {
	t.Parallel()

	svc := NewService(nil, nil, NewPDFParser(0), defaultMaxSize, obs.New(), logging.Discard())
	if _, err := svc.StoredFromKey("resumes/abc.pdf", "resume.pdf", 1); err == nil {
		t.Fatal("StoredFromKey() = nil error, want it to fail without a path resolver")
	}
}
