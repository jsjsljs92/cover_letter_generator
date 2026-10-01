package resume

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"

	pdf "github.com/ledongthuc/pdf"

	"coverletter/internal/apperror"
)

// maxExtractBytes caps how much of a PDF the extractor will read.
const maxExtractBytes = 20 << 20 // 20 MiB

// PDFParser extracts text from PDF resumes using a pure-Go reader, so the
// service needs no cgo toolchain.
type PDFParser struct {
	// MaxRunes truncates the extracted text. 0 disables truncation.
	MaxRunes int
}

// NewPDFParser returns a parser that truncates extracted text to maxRunes.
func NewPDFParser(maxRunes int) *PDFParser {
	return &PDFParser{MaxRunes: maxRunes}
}

// Parse implements Parser.
func (p *PDFParser) Parse(ctx context.Context, path string) (*Resume, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	info, err := os.Stat(path)
	if err != nil {
		return nil, apperror.Wrap(err, apperror.CodeResumeParsing, "The uploaded resume could not be read.", 500)
	}
	if !info.Mode().IsRegular() {
		return nil, apperror.Validation(apperror.CodeInvalidResume, "The uploaded resume is not a regular file.")
	}
	if info.Size() > maxExtractBytes {
		return nil, apperror.Validation(apperror.CodePayloadTooLarge, "The resume is too large to process.")
	}

	file, reader, err := pdf.Open(path)
	if err != nil {
		return nil, apperror.Wrap(err, apperror.CodeResumeParsing,
			"The resume could not be parsed as a PDF.", 422)
	}
	defer func() { _ = file.Close() }()

	plain, err := reader.GetPlainText()
	if err != nil {
		return nil, apperror.Wrap(err, apperror.CodeResumeParsing,
			"The resume text could not be extracted.", 422)
	}

	// Bound the extraction itself: a malformed PDF could otherwise stream an
	// unbounded amount of text.
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, io.LimitReader(plain, maxExtractBytes)); err != nil {
		return nil, apperror.Wrap(err, apperror.CodeResumeParsing, "The resume text could not be read.", 500)
	}

	text := NormalizeText(buf.String())
	if len([]rune(text)) < minExtractedRunes {
		return nil, apperror.New(apperror.CodeResumeParsing,
			"No readable text was found in the resume. Scanned or image-only PDFs are not supported.", 422)
	}

	if p.MaxRunes > 0 && len([]rune(text)) > p.MaxRunes {
		text = truncateRunes(text, p.MaxRunes)
	}

	return &Resume{
		Path:  path,
		Text:  text,
		Bytes: info.Size(),
	}, nil
}

// NormalizeText collapses the whitespace artefacts that PDF text layers produce
// (hyphenated line breaks, ligatures, ragged spacing) into something a language
// model can read reliably.
func NormalizeText(raw string) string {
	if raw == "" {
		return ""
	}

	replacer := strings.NewReplacer(
		"\r\n", "\n",
		"\r", "\n",
		"ﬁ", "fi",
		"ﬂ", "fl",
		"•", "\n• ",
		"●", "\n• ",
		"\t", " ",
		"\u00a0", " ",
		"‘", "'",
		"’", "'",
		"“", `"`,
		"”", `"`,
		"–", "-",
		"—", "-",
	)
	text := replacer.Replace(raw)

	// Rejoin words broken across a line by a hyphen, e.g. "distri-\nbuted"
	// becomes "distributed". Both the hyphen and the newline are artefacts of
	// line wrapping, so both are dropped; the text feeds a model prompt, where
	// "distributed" reads correctly and "distri-buted" does not.
	var sb strings.Builder
	sb.Grow(len(text))
	for {
		idx := strings.Index(text, "-\n")
		if idx < 0 {
			sb.WriteString(text)
			break
		}
		sb.WriteString(text[:idx])

		// Resume past the hyphen, then past the line break and any indent on
		// the continuation line.
		rest := strings.TrimPrefix(text[idx+1:], "\r")
		rest = strings.TrimPrefix(rest, "\n")
		text = strings.TrimLeft(rest, " \t")
	}
	text = sb.String()

	// Collapse horizontal whitespace runs, then blank-line runs.
	var out strings.Builder
	out.Grow(len(text))
	spaceRun := 0
	newlineRun := 0
	for _, r := range text {
		switch {
		case r == '\n':
			spaceRun = 0
			newlineRun++
			if newlineRun <= 2 {
				out.WriteByte('\n')
			}
		case r == ' ':
			newlineRun = 0
			spaceRun++
			if spaceRun <= 1 {
				out.WriteByte(' ')
			}
		default:
			newlineRun = 0
			spaceRun = 0
			out.WriteRune(r)
		}
	}
	return strings.TrimSpace(out.String())
}

// SanitizeFilename strips any path components and unsafe characters from a
// client-supplied filename. The result is for display only: storage keys are
// generated server-side.
func SanitizeFilename(name string) string {
	// Handle both Unix and Windows separators: a Windows client may send
	// "C:\\Users\\me\\resume.pdf".
	name = strings.ReplaceAll(name, `\`, "/")
	name = filepath.Base(name)
	name = strings.TrimSpace(name)

	var sb strings.Builder
	for _, r := range name {
		switch {
		case r < 0x20 || r == 0x7F:
			// Drop control characters.
		case strings.ContainsRune(`/:*?"<>|`, r):
			sb.WriteRune('_')
		default:
			sb.WriteRune(r)
		}
	}
	cleaned := strings.TrimSpace(sb.String())
	if cleaned == "" || cleaned == "." || cleaned == ".." {
		return "resume.pdf"
	}
	if len(cleaned) > 128 {
		cleaned = cleaned[len(cleaned)-128:]
	}
	return cleaned
}

func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return strings.TrimRight(string(runes[:max]), " \n\t,;:-") + "\n[truncated]"
}
