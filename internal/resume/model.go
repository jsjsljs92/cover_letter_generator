// Package resume handles PDF resume uploads: validation, storage and text
// extraction.
//
// The client is never trusted. The declared Content-Type and filename are
// advisory only; the file type is determined by sniffing the file's magic bytes
// and the content is validated after extraction.
package resume

import "context"

// Resume is a parsed, text-extracted resume.
type Resume struct {
	// Path is where the original PDF is stored.
	Path string
	// Filename is the sanitised original filename, for display only.
	Filename string
	// Text is the extracted plain text, normalised for the model.
	Text string
	// Bytes is the size of the stored file.
	Bytes int64
}

// Parser extracts text from a stored resume file.
//
// Keeping this behind an interface means DOCX or OCR-based extraction can be
// added without changing the service.
type Parser interface {
	Parse(ctx context.Context, path string) (*Resume, error)
}

// minExtractedRunes is the shortest acceptable extraction. A PDF that yields
// less than this is almost certainly a scan with no text layer, or a file that
// is not really a PDF, and would produce an empty or invented cover letter.
const minExtractedRunes = 120
