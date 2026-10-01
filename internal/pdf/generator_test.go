package pdf

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"coverletter/internal/apperror"
)

func testRequest() Request {
	return Request{
		CandidateName: "Ada Lovelace",
		JobTitle:      "Backend Engineer",
		Company:       "Acme",
		Subject:       "Application for Backend Engineer",
		Body: "Dear Hiring Team,\n\n" +
			"I am applying for the Backend Engineer role. For six years I have built " +
			"and operated backend services in Go.\n\n" +
			"Thank you for your time.\n\nKind regards,\nAda Lovelace",
		Date: time.Date(2026, time.March, 14, 0, 0, 0, 0, time.UTC),
	}
}

func TestGenerateProducesValidPDF(t *testing.T) {
	t.Parallel()

	out, err := NewGenerator().Generate(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	if !bytes.HasPrefix(out, []byte("%PDF-")) {
		t.Errorf("output does not start with a PDF header: %q", out[:min(8, len(out))])
	}
	if !bytes.Contains(out, []byte("%%EOF")) {
		t.Error("output has no PDF trailer; the document is truncated")
	}
	if len(out) < 1000 {
		t.Errorf("output is only %d bytes, which is too small to be a rendered document", len(out))
	}
}

func TestGenerateIsDeterministic(t *testing.T) {
	t.Parallel()

	generator := NewGenerator()
	req := testRequest()

	first, err := generator.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	second, err := generator.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	// Identical input must produce identical bytes: the PDF step is a pure
	// function of the letter, which is what makes it separately testable from
	// the model.
	if !bytes.Equal(first, second) {
		t.Error("two renders of the same request produced different bytes")
	}
}

func TestGeneratePaginatesLongLetters(t *testing.T) {
	t.Parallel()

	req := testRequest()
	paragraph := "This is a detailed account of a relevant project, describing the " +
		"problem, the approach taken and the measurable outcome for the team. "
	var sb strings.Builder
	for i := 0; i < 40; i++ {
		sb.WriteString(paragraph)
		sb.WriteString("\n\n")
	}
	req.Body = sb.String()

	out, err := NewGenerator().Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if pages := bytes.Count(out, []byte("/Type /Page\n")); pages < 2 {
		t.Errorf("a long letter produced %d page object(s), want a multi-page document", pages)
	}
}

func TestGenerateRejectsEmptyBody(t *testing.T) {
	t.Parallel()

	req := testRequest()
	req.Body = "   \n  "

	_, err := NewGenerator().Generate(context.Background(), req)
	if err == nil {
		t.Fatal("Generate() = nil error, want an empty body to be rejected")
	}
	if !apperror.Is(err, apperror.CodeValidation) {
		t.Errorf("Generate() error = %v, want code %s", err, apperror.CodeValidation)
	}
}

func TestGenerateHonoursContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := NewGenerator().Generate(ctx, testRequest())
	if err == nil {
		t.Fatal("Generate() = nil error, want context cancellation to abort generation")
	}
}

func TestSplitParagraphs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "blank lines separate paragraphs",
			body: "First para.\n\nSecond para.",
			want: []string{"First para.", "Second para."},
		},
		{
			name: "single newlines are joined within a paragraph",
			body: "A line\nthat wraps.\n\nNext.",
			want: []string{"A line that wraps.", "Next."},
		},
		{
			name: "windows line endings",
			body: "First.\r\n\r\nSecond.",
			want: []string{"First.", "Second."},
		},
		{
			name: "runs of blank lines collapse",
			body: "First.\n\n\n\n\nSecond.",
			want: []string{"First.", "Second."},
		},
		{
			name: "empty input",
			body: "",
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := splitParagraphs(tc.body)
			if len(got) != len(tc.want) {
				t.Fatalf("splitParagraphs(%q) = %q, want %q", tc.body, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("paragraph %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}
