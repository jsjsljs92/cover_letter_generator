// Package pdf renders the final cover letter document.
//
// PDF generation is deliberately separate from, and downstream of, the AI. The
// model produces text; this package decides how that text is typeset. That
// split keeps rendering deterministic, testable without a model, and free to
// change without touching the workflow.
package pdf

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-pdf/fpdf"

	"coverletter/internal/apperror"
)

// Request is everything needed to typeset one letter.
type Request struct {
	CandidateName string
	JobTitle      string
	Company       string
	Subject       string
	Body          string
	// Date is passed in rather than read from the clock, so the same request
	// always renders the same document.
	Date time.Time
}

// Generator renders a cover letter as PDF bytes.
type Generator interface {
	Generate(ctx context.Context, req Request) ([]byte, error)
}

// Layout constants, in millimetres.
const (
	pageWidth    = 210.0 // A4
	marginLeft   = 22.0
	marginRight  = 22.0
	marginTop    = 20.0
	marginBottom = 20.0

	nameSize     = 16.0
	metaSize     = 10.5
	subjectSize  = 11.5
	bodySize     = 11.0
	lineHeight   = 6.2
	paragraphGap = 3.0
	footerSize   = 8.5
)

var (
	colInk   = [3]int{17, 24, 39}
	colMuted = [3]int{107, 114, 128}
	colRule  = [3]int{209, 213, 219}
)

// FPDFGenerator renders letters with go-pdf/fpdf using its built-in fonts, so
// no font files need to be shipped or licensed.
type FPDFGenerator struct{}

// NewGenerator returns the default PDF generator.
func NewGenerator() *FPDFGenerator { return &FPDFGenerator{} }

// Generate implements Generator.
func (g *FPDFGenerator) Generate(ctx context.Context, req Request) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	body := strings.TrimSpace(req.Body)
	if body == "" {
		return nil, apperror.Validation(apperror.CodeValidation, "The cover letter body is empty.")
	}

	doc := fpdf.New(fpdf.OrientationPortrait, "mm", fpdf.PageSizeA4, "")
	doc.SetMargins(marginLeft, marginTop, marginRight)
	doc.SetAutoPageBreak(true, marginBottom)
	// A fixed creation date and a sorted catalog keep repeated renders of the
	// same request byte-identical. Without the sort, the font dictionary is
	// written in Go map iteration order, which varies per run.
	doc.SetCreationDate(req.Date)
	doc.SetCatalogSort(true)
	if name := strings.TrimSpace(req.CandidateName); name != "" {
		doc.SetAuthor(name, false)
	}
	if title := strings.TrimSpace(req.JobTitle); title != "" {
		doc.SetTitle("Cover letter: "+title, false)
	}

	doc.SetFooterFunc(func() {
		doc.SetY(-marginBottom + 6)
		doc.SetFont("Helvetica", "", footerSize)
		doc.SetTextColor(colMuted[0], colMuted[1], colMuted[2])
		doc.CellFormat(0, 5, fmt.Sprintf("Page %d of %d", doc.PageNo(), doc.PageCount()), "", 0, "C", false, 0, "")
	})

	doc.AddPage()
	contentWidth := pageWidth - marginLeft - marginRight

	// Letterhead: who is writing, and for what.
	if name := strings.TrimSpace(req.CandidateName); name != "" {
		doc.SetFont("Helvetica", "B", nameSize)
		doc.SetTextColor(colInk[0], colInk[1], colInk[2])
		doc.CellFormat(contentWidth, 8, name, "", 1, "L", false, 0, "")
	}

	role := strings.TrimSpace(req.JobTitle)
	if company := strings.TrimSpace(req.Company); company != "" {
		if role == "" {
			role = company
		} else {
			role += " — " + company
		}
	}
	if role != "" {
		doc.SetFont("Helvetica", "", metaSize)
		doc.SetTextColor(colMuted[0], colMuted[1], colMuted[2])
		doc.CellFormat(contentWidth, 5.5, "Applying for: "+role, "", 1, "L", false, 0, "")
	}

	doc.Ln(1)
	doc.SetDrawColor(colRule[0], colRule[1], colRule[2])
	doc.SetLineWidth(0.3)
	doc.Line(marginLeft, doc.GetY(), pageWidth-marginRight, doc.GetY())
	doc.Ln(4)

	doc.SetFont("Helvetica", "", metaSize)
	doc.SetTextColor(colMuted[0], colMuted[1], colMuted[2])
	dateText := ""
	if !req.Date.IsZero() {
		dateText = req.Date.Format("2 January 2006")
	}
	if subject := strings.TrimSpace(req.Subject); subject != "" {
		// Date on the left, subject reference on the right of one line.
		doc.CellFormat(contentWidth/2, 6, dateText, "", 0, "L", false, 0, "")
		doc.CellFormat(contentWidth/2, 6, subject, "", 1, "R", false, 0, "")
	} else {
		doc.CellFormat(contentWidth, 6, dateText, "", 1, "L", false, 0, "")
	}
	doc.Ln(2)

	if subject := strings.TrimSpace(req.Subject); subject != "" {
		doc.SetFont("Helvetica", "B", subjectSize)
		doc.SetTextColor(colInk[0], colInk[1], colInk[2])
		doc.MultiCell(contentWidth, lineHeight, subject, "", "L", false)
		doc.Ln(paragraphGap)
	}

	doc.SetFont("Helvetica", "", bodySize)
	doc.SetTextColor(colInk[0], colInk[1], colInk[2])
	for _, paragraph := range splitParagraphs(body) {
		doc.MultiCell(contentWidth, lineHeight, paragraph, "", "J", false)
		doc.Ln(paragraphGap)
	}

	var buf bytes.Buffer
	if err := doc.Output(&buf); err != nil {
		return nil, apperror.Wrap(err, apperror.CodePDFFailure, "The cover letter PDF could not be generated.", 500)
	}
	return buf.Bytes(), nil
}

// splitParagraphs breaks the body on blank lines and flattens single newlines
// within a paragraph, so a stray line wrap from the model does not become a
// paragraph break in the document.
func splitParagraphs(body string) []string {
	raw := strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\r", "\n")

	var paragraphs []string
	for _, block := range strings.Split(raw, "\n\n") {
		lines := strings.Split(block, "\n")
		words := make([]string, 0, len(lines))
		for _, line := range lines {
			if line = strings.TrimSpace(line); line != "" {
				words = append(words, line)
			}
		}
		if len(words) == 0 {
			continue
		}
		paragraphs = append(paragraphs, strings.Join(words, " "))
	}
	return paragraphs
}
