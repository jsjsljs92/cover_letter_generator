package resume

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-pdf/fpdf"

	"coverletter/internal/apperror"
)

// resumeParagraphs is comfortably longer than minExtractedRunes, so the
// extract-then-validate path in Parse is exercised rather than short-circuited.
var resumeParagraphs = []string{
	"Grace Hopper is a senior platform engineer with nine years of experience building distributed systems.",
	"She designed the compiler for the COBOL machine and led the team that produced the first standard library.",
	"At Acme she ran the Kubernetes migration, moving three hundred services onto a managed cluster.",
	"Her current work focuses on build reliability, deployment automation and cost control across regions.",
	"She mentors engineers on incident response and maintains an open source tool for tracing slow queries.",
}

// buildPDF renders paragraphs into a real, structurally valid PDF with an
// extractable text layer. Using a real producer rather than a hand-written
// fixture is what makes the extraction tests meaningful.
func buildPDF(t *testing.T, paragraphs []string) []byte {
	t.Helper()

	doc := fpdf.New(fpdf.OrientationPortrait, "mm", fpdf.PageSizeA4, "")
	doc.SetMargins(20, 20, 20)
	doc.SetAutoPageBreak(true, 20)
	doc.AddPage()
	doc.SetFont("Helvetica", "", 12)
	for _, p := range paragraphs {
		doc.MultiCell(170, 6, p, "", "L", false)
		doc.Ln(3)
	}

	var buf bytes.Buffer
	if err := doc.Output(&buf); err != nil {
		t.Fatalf("building test PDF: %v", err)
	}
	if !bytes.HasPrefix(buf.Bytes(), []byte("%PDF-")) {
		t.Fatalf("test fixture is not a PDF: %q", buf.Bytes()[:min(8, buf.Len())])
	}
	return buf.Bytes()
}

// writeTempFile drops content into a fresh temp dir under the given name. The
// name matters: several cases below are specifically about a file whose name
// lies about its contents.
func writeTempFile(t *testing.T, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("writing %q: %v", path, err)
	}
	return path
}

func TestPDFParserParse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// setup returns the path to hand to Parse.
		setup         func(t *testing.T) string
		maxRunes      int
		wantErr       bool
		wantCode      string
		wantMsg       string
		wantContains  []string
		wantTruncated bool
	}{
		{
			name: "valid PDF with a real text layer",
			setup: func(t *testing.T) string {
				return writeTempFile(t, "resume.pdf", buildPDF(t, resumeParagraphs))
			},
			wantContains: []string{"Grace", "Hopper", "Kubernetes", "compiler", "Acme"},
		},
		{
			name: "non-PDF bytes are rejected",
			setup: func(t *testing.T) string {
				return writeTempFile(t, "resume.pdf", []byte("hello"))
			},
			wantErr:  true,
			wantCode: apperror.CodeResumeParsing,
		},
		{
			name: "text file renamed to .pdf is rejected",
			// The filename and any declared Content-Type are advisory only; if
			// the extension were trusted, this upload would sail through and
			// be handed to the text extractor as a document.
			setup: func(t *testing.T) string {
				return writeTempFile(t, "resume.pdf", []byte(
					"Dear hiring manager,\nI am writing to apply for the position.\n"+
						"This body is deliberately long enough to look like a resume\n"+
						"but it is plain text and not a PDF at all, so it must fail.\n"))
			},
			wantErr:  true,
			wantCode: apperror.CodeResumeParsing,
		},
		{
			name: "truncated PDF header alone is rejected",
			setup: func(t *testing.T) string {
				return writeTempFile(t, "resume.pdf", []byte("%PDF-1.7\n"))
			},
			wantErr:  true,
			wantCode: apperror.CodeResumeParsing,
		},
		{
			name: "empty file is rejected",
			setup: func(t *testing.T) string {
				return writeTempFile(t, "resume.pdf", nil)
			},
			wantErr:  true,
			wantCode: apperror.CodeResumeParsing,
		},
		{
			name: "PDF whose text is too short to be a resume is rejected",
			// A structurally valid PDF with no usable text layer is what a
			// scan arrives as. Accepting it would hand the model an empty
			// document and invite it to invent the candidate's history.
			setup: func(t *testing.T) string {
				return writeTempFile(t, "scan.pdf", buildPDF(t, []string{"Jane Doe"}))
			},
			wantErr:  true,
			wantCode: apperror.CodeResumeParsing,
			wantMsg:  "image-only",
		},
		{
			name: "missing file is reported as a read failure",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "absent.pdf")
			},
			wantErr:  true,
			wantCode: apperror.CodeResumeParsing,
		},
		{
			name: "directory is not a regular file",
			setup: func(t *testing.T) string {
				return t.TempDir()
			},
			wantErr:  true,
			wantCode: apperror.CodeInvalidResume,
		},
		{
			name: "extraction is truncated to MaxRunes",
			setup: func(t *testing.T) string {
				return writeTempFile(t, "resume.pdf", buildPDF(t, resumeParagraphs))
			},
			maxRunes:      150,
			wantTruncated: true,
			// Truncation must not silently lose the document's beginning.
			wantContains: []string{"Grace", "Hopper"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := tc.setup(t)
			parser := NewPDFParser(tc.maxRunes)

			got, err := parser.Parse(context.Background(), path)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("Parse(%q) = nil error, want a rejection", path)
				}
				if tc.wantCode != "" && !apperror.Is(err, tc.wantCode) {
					t.Errorf("Parse() error = %v, want code %s", err, tc.wantCode)
				}
				if tc.wantMsg != "" && !strings.Contains(err.Error(), tc.wantMsg) {
					t.Errorf("Parse() error = %v, want it to mention %q", err, tc.wantMsg)
				}
				if got != nil {
					t.Errorf("Parse() = %+v alongside an error, want nil", got)
				}
				return
			}

			if err != nil {
				t.Fatalf("Parse(%q) error = %v", path, err)
			}
			if got.Path != path {
				t.Errorf("Resume.Path = %q, want %q", got.Path, path)
			}
			info, statErr := os.Stat(path)
			if statErr != nil {
				t.Fatalf("Stat(%q): %v", path, statErr)
			}
			if got.Bytes != info.Size() {
				t.Errorf("Resume.Bytes = %d, want the file size %d", got.Bytes, info.Size())
			}
			if runes := len([]rune(got.Text)); runes < minExtractedRunes {
				t.Errorf("extracted %d runes, want at least minExtractedRunes (%d)", runes, minExtractedRunes)
			}
			for _, word := range tc.wantContains {
				if !strings.Contains(got.Text, word) {
					t.Errorf("extracted text does not contain %q; got:\n%s", word, got.Text)
				}
			}
			if tc.wantTruncated {
				if !strings.HasSuffix(got.Text, "\n[truncated]") {
					t.Errorf("truncated text = %q, want it to end with the truncation marker", lastRunes(got.Text, 40))
				}
				if runes := len([]rune(got.Text)); runes > tc.maxRunes+len("\n[truncated]") {
					t.Errorf("truncated text is %d runes, want no more than %d plus the marker", runes, tc.maxRunes)
				}
			}
		})
	}
}

func TestPDFParserParseHonoursContextCancellation(t *testing.T) {
	t.Parallel()

	path := writeTempFile(t, "resume.pdf", buildPDF(t, resumeParagraphs))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := NewPDFParser(0).Parse(ctx, path); err == nil {
		t.Fatal("Parse() = nil error, want context cancellation to abort extraction")
	}
}

func TestNormalizeText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "empty input",
			in:   "",
			want: "",
		},
		{
			name: "fi and fl ligatures are decomposed",
			in:   "efﬁcient workﬂow",
			want: "efficient workflow",
		},
		{
			name: "curly single quotes become straight",
			in:   "the ‘best’ run",
			want: "the 'best' run",
		},
		{
			name: "curly double quotes become straight",
			in:   "he said “hello” twice",
			want: `he said "hello" twice`,
		},
		{
			name: "en and em dashes become hyphens",
			in:   "an en–dash and an em—dash",
			want: "an en-dash and an em-dash",
		},
		{
			// The hyphen and the newline are both wrapping artefacts, so
			// "distri-\nbuted" must become "distributed" rather than keeping
			// the hyphen.
			name: "hyphenated line break is rejoined",
			in:   "distri-\nbuted systems",
			want: "distributed systems",
		},
		{
			name: "several hyphenated line breaks are all rejoined",
			in:   "manu-\nfacturing and distri-\nbuted",
			want: "manufacturing and distributed",
		},
		{
			name: "an indented continuation line loses its indent",
			in:   "distri-\n   buted",
			want: "distributed",
		},
		{
			name: "tabs and non-breaking spaces become spaces",
			in:   "Go\tand Rust",
			want: "Go and Rust",
		},
		{
			name: "runs of spaces collapse",
			in:   "  too    many   spaces   ",
			want: "too many spaces",
		},
		{
			name: "runs of blank lines collapse to one blank line",
			in:   "first\n\n\n\n\nsecond",
			want: "first\n\nsecond",
		},
		{
			name: "windows and classic mac line endings normalise",
			in:   "one\r\ntwo\rthree",
			want: "one\ntwo\nthree",
		},
		{
			// Each bullet becomes "\n• ", so the newline already present in
			// the input between bullets survives as well: the run collapses
			// to one blank line rather than a single newline.
			name: "bullet glyphs are put on their own lines",
			in:   "• Python\n• Go",
			want: "• Python\n\n• Go",
		},
		{
			name: "mixed artefacts normalise together",
			in:   "\r\nProﬁcient—Kubernetes  work\r\n\r\ndistri-\nbuted\t infra  ",
			want: "Proficient-Kubernetes work\n\ndistributed infra",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := NormalizeText(tc.in); got != tc.want {
				t.Errorf("NormalizeText(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSanitizeFilename(t *testing.T) {
	t.Parallel()

	const longName = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" +
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" +
		"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccc" +
		"ddddddddddddddddddddddddddddddresume.pdf"

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "ordinary name is unchanged",
			in:   "Ada_Lovelace_Resume.pdf",
			want: "Ada_Lovelace_Resume.pdf",
		},
		{
			name: "unix traversal is reduced to the base name",
			in:   "../../etc/passwd",
			want: "passwd",
		},
		{
			name: "windows traversal is reduced to the base name",
			in:   `..\..\windows\system32\config`,
			want: "config",
		},
		{
			name: "windows drive path is reduced to the base name",
			in:   `C:\Users\me\Documents\resume.pdf`,
			want: "resume.pdf",
		},
		{
			name: "absolute path is reduced to the base name",
			in:   "/var/tmp/resume.pdf",
			want: "resume.pdf",
		},
		{
			name: "traversal to a pdf-looking name",
			in:   "../../evil.pdf",
			want: "evil.pdf",
		},
		{
			name: "null byte is dropped",
			in:   "re\x00sume.pdf",
			want: "resume.pdf",
		},
		{
			name: "control characters are dropped",
			in:   "re\x01\x1b\nsume\x7f.pdf",
			want: "resume.pdf",
		},
		{
			name: "windows-reserved characters are replaced",
			in:   "re:sume?.pdf",
			want: "re_sume_.pdf",
		},
		{
			name: "empty name falls back",
			in:   "",
			want: "resume.pdf",
		},
		{
			name: "whitespace-only name falls back",
			in:   "   \t ",
			want: "resume.pdf",
		},
		{
			name: "dot falls back",
			in:   ".",
			want: "resume.pdf",
		},
		{
			name: "dotdot falls back",
			in:   "..",
			want: "resume.pdf",
		},
		{
			name: "overlong name is clipped to its tail",
			in:   longName,
			want: longName[len(longName)-128:],
		},
		{
			name: "unicode is preserved",
			in:   "简历 – final.pdf",
			want: "简历 – final.pdf",
		},
		{
			name: "unicode with traversal is preserved but isolated",
			in:   "../../资料/简历.pdf",
			want: "简历.pdf",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := SanitizeFilename(tc.in)
			if got != tc.want {
				t.Fatalf("SanitizeFilename(%q) = %q, want %q", tc.in, got, tc.want)
			}
			// Whatever the input, the result must be a bare leaf name that
			// cannot escape the directory it is displayed in.
			if strings.ContainsAny(got, `/\`) {
				t.Errorf("SanitizeFilename(%q) = %q, still contains a path separator", tc.in, got)
			}
			if got == "." || got == ".." {
				t.Errorf("SanitizeFilename(%q) = %q, want a usable fallback", tc.in, got)
			}
			for _, r := range got {
				if r < 0x20 || r == 0x7F {
					t.Errorf("SanitizeFilename(%q) = %q, still contains control character %q", tc.in, got, r)
				}
			}
		})
	}
}

// lastRunes renders the tail of s for a failure message without dumping a whole
// resume into the test log.
func lastRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "..." + string(r[len(r)-n:])
}
