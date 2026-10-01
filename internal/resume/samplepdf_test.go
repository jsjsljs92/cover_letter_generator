package resume

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sampleLines mirrors the content the frontend's "Use sample resume" button
// writes into its generated PDF.
var sampleLines = []string{
	"Jia Sheng",
	"Senior Software Engineer",
	"SUMMARY",
	"Senior engineer with eight years building distributed web platforms and",
	"design systems. Focused on TypeScript, React, and API design.",
	"EXPERIENCE",
	"Staff Engineer, Northwind Systems (2021 - present)",
	"Led the migration of a monolithic dashboard to a typed component platform",
	"serving 40k daily users, cutting bundle size by 38 percent.",
	"Designed the public REST and GraphQL API layer adopted by four teams.",
	"Software Engineer, Contoso (2018 - 2021)",
	"Built an accessibility review pipeline now run on every release.",
	"SKILLS",
	"TypeScript, React, Go, PostgreSQL, Kubernetes, accessibility, performance.",
}

// buildSampleResumePDF is a line-for-line port of buildSampleResumePdf() in
// cover_letter_generator_fe/src/components/ResumeUploader.tsx.
//
// The frontend cannot be executed in this repository's toolchain, so the
// algorithm is reproduced here to prove that a PDF built this way is one the
// real parser accepts. If the TypeScript version changes, this must change with
// it.
func buildSampleResumePDF() []byte {
	var textOps strings.Builder
	for i, line := range sampleLines {
		fmt.Fprintf(&textOps, "BT /F1 11 Tf 1 0 0 1 60 %d Tm (%s) Tj ET", 740-i*30, escapePDFText(line))
		textOps.WriteString("\n")
	}
	content := textOps.String()

	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(content), content),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}

	var pdf strings.Builder
	pdf.WriteString("%PDF-1.4\n")
	offsets := make([]int, 0, len(objects))
	for i, body := range objects {
		offsets = append(offsets, pdf.Len())
		fmt.Fprintf(&pdf, "%d 0 obj\n%s\nendobj\n", i+1, body)
	}

	xrefStart := pdf.Len()
	fmt.Fprintf(&pdf, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		fmt.Fprintf(&pdf, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&pdf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xrefStart)

	return []byte(pdf.String())
}

func escapePDFText(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `(`, `\(`, `)`, `\)`)
	return r.Replace(s)
}

// TestSampleResumePDFIsParseable is the regression guard for the frontend's
// "Use sample resume" helper.
//
// A stub PDF with no text layer passes the %PDF- magic check but extracts
// nothing, and the server rejects it as image-only — which made the sample
// button useless for testing an integration. This asserts the generated file
// survives the real parser and clears the minimum extracted length.
func TestSampleResumePDFIsParseable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sample.pdf")
	if err := os.WriteFile(path, buildSampleResumePDF(), 0o600); err != nil {
		t.Fatalf("writing the sample PDF: %v", err)
	}

	parsed, err := NewPDFParser(0).Parse(t.Context(), path)
	if err != nil {
		t.Fatalf("the sample resume is not parseable: %v", err)
	}

	if runes := len([]rune(parsed.Text)); runes < minExtractedRunes {
		t.Errorf("extracted %d runes, want at least %d", runes, minExtractedRunes)
	}
	// The parser must recover the words, not just enough characters to pass.
	if !strings.Contains(parsed.Text, "Jia Sheng") {
		t.Errorf("extracted text does not contain the candidate name: %q", parsed.Text)
	}
	if !strings.Contains(parsed.Text, "TypeScript") {
		t.Errorf("extracted text does not contain the skills line: %q", parsed.Text)
	}
}
