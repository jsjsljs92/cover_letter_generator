package resume

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"time"

	"coverletter/internal/apperror"
	"coverletter/internal/id"
	"coverletter/internal/obs"
	"coverletter/internal/storage"
)

// pdfMagic is the "%PDF-" file signature. Checking it is the only file-type
// check the client cannot spoof, unlike Content-Type or the filename extension.
var pdfMagic = []byte("%PDF-")

// sniffLen is how many bytes are read ahead to validate the magic number. Some
// producers emit a small amount of junk before the signature, so the search
// window is generous; beyond this point a real PDF header is implausible.
const sniffLen = 1024

// Stored is an upload that passed validation and was persisted.
type Stored struct {
	// Path is the on-disk location of the stored file.
	Path string
	// StorageKey is the key the file is stored under.
	StorageKey string
	// Filename is the sanitised original filename, for display only.
	Filename string
	// Bytes is the size of the stored file.
	Bytes int64
}

// PathResolver maps a storage key back to a local filesystem path.
//
// The text extractor opens a file by name, so it needs a path rather than a
// stream. This narrow interface keeps that need explicit without adding a
// filesystem concept to the general Storage abstraction, which an object-store
// backend would not be able to satisfy.
type PathResolver interface {
	Path(key string) (string, error)
}

// Service validates, stores and parses resume uploads.
type Service struct {
	storage  storage.Storage
	resolver PathResolver
	parser   Parser
	maxSize  int64
	metrics  *obs.Registry
	logger   *slog.Logger
}

// NewService builds the resume service.
func NewService(store storage.Storage, resolver PathResolver, parser Parser, maxSize int64, metrics *obs.Registry, logger *slog.Logger) *Service {
	return &Service{
		storage:  store,
		resolver: resolver,
		parser:   parser,
		maxSize:  maxSize,
		metrics:  metrics,
		logger:   logger,
	}
}

// Store validates and persists an uploaded resume.
//
// The storage key is generated server-side from a cryptographically random
// identifier, so a hostile filename cannot influence where the file lands or
// overwrite another upload.
func (s *Service) Store(ctx context.Context, filename string, r io.Reader) (*Stored, error) {
	if r == nil {
		return nil, apperror.Validation(apperror.CodeInvalidResume, "A resume PDF is required.")
	}

	// Peek at the header without consuming it: the declared type is never
	// trusted, only the bytes are.
	header := make([]byte, sniffLen)
	n, err := io.ReadFull(r, header)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, apperror.Wrap(err, apperror.CodeInvalidResume, "The uploaded resume could not be read.", 400)
	}
	header = header[:n]

	if !isPDF(header) {
		s.metrics.Inc("resume.store.rejected")
		return nil, apperror.Validation(apperror.CodeUnsupportedType, "The resume must be a PDF file.")
	}

	key := "resumes/" + id.New() + ".pdf"
	// The sniffed bytes are already consumed, so replay them at the head of
	// the stream before persisting.
	body := io.MultiReader(bytes.NewReader(header), r)

	path, err := s.storage.PutReader(ctx, key, body, s.maxSize)
	if err != nil {
		if errors.Is(err, storage.ErrTooLarge) {
			s.metrics.Inc("resume.store.rejected")
			return nil, apperror.Validation(apperror.CodePayloadTooLarge,
				"The resume exceeds the maximum allowed size.")
		}
		return nil, apperror.Wrap(err, apperror.CodeStorage, "The resume could not be stored.", 500)
	}

	var size int64
	if info, statErr := os.Stat(path); statErr == nil {
		size = info.Size()
	}

	s.metrics.Inc("resume.store.succeeded")
	// The filename has been stripped of path components and control characters,
	// so it is safe to log. Resume contents are never logged.
	s.logger.Debug("stored resume",
		slog.String("storage_key", key),
		slog.String("filename", SanitizeFilename(filename)),
		slog.Int64("bytes", size),
	)

	return &Stored{
		Path:       path,
		StorageKey: key,
		Filename:   SanitizeFilename(filename),
		Bytes:      size,
	}, nil
}

// StoredFromKey rebuilds a Stored descriptor for a file persisted by an
// earlier request, so a queued job can be resumed without the original upload.
func (s *Service) StoredFromKey(key, filename string, size int64) (*Stored, error) {
	if s.resolver == nil {
		return nil, apperror.Internal(nil, "Resume storage cannot resolve paths.")
	}
	path, err := s.resolver.Path(key)
	if err != nil {
		return nil, apperror.Wrap(err, apperror.CodeStorage, "The stored resume could not be located.", 500)
	}
	return &Stored{
		Path:       path,
		StorageKey: key,
		Filename:   SanitizeFilename(filename),
		Bytes:      size,
	}, nil
}

// Parse extracts text from a stored resume.
func (s *Service) Parse(ctx context.Context, stored *Stored) (*Resume, error) {
	if stored == nil {
		return nil, apperror.Internal(nil, "No resume was stored for this request.")
	}

	start := time.Now()
	resumed, err := s.parser.Parse(ctx, stored.Path)
	if err != nil {
		s.metrics.Inc("resume.parse.failed")
		return nil, err
	}
	s.metrics.Observe("resume.parse.duration", time.Since(start))

	resumed.Filename = stored.Filename
	resumed.Bytes = stored.Bytes

	s.metrics.Inc("resume.parse.succeeded")
	// Only the size of the extraction is logged, never its content.
	s.logger.Debug("parsed resume",
		slog.String("storage_key", stored.StorageKey),
		slog.Int("text_runes", len([]rune(resumed.Text))),
	)
	return resumed, nil
}

// isPDF reports whether the buffer contains the PDF signature within the sniff
// window. It is searched rather than required at offset zero because some
// producers prepend a few bytes of junk.
func isPDF(header []byte) bool {
	if len(header) < len(pdfMagic) {
		return false
	}
	return bytes.Contains(header, pdfMagic)
}
