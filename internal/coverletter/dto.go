package coverletter

import (
	"strconv"
	"time"
)

// The DTOs in this file are the only shapes the API speaks. Keeping them apart
// from the GORM model means a schema change cannot silently alter the contract,
// and it makes it explicit which fields are safe to expose: note the absence of
// ResumeText, ResumePath, PDFPath and the raw database error text.

// CreateRequest is the validated form of the multipart submission.
type CreateRequest struct {
	// JobURL and JobDescription are mutually exclusive; exactly one is set.
	JobURL         string
	JobDescription string
	Tone           string
	// ResumeKey is the storage key of the already-uploaded resume PDF.
	ResumeKey      string
	ResumeFilename string
	ResumeBytes    int64
}

// CoverLetterResponse is the representation of a generation request.
type CoverLetterResponse struct {
	ID     uint   `json:"id"`
	Status Status `json:"status"`

	// Echoed input, so a client polling can correlate its request.
	JobURL   string `json:"job_url,omitempty"`
	Tone     string `json:"tone,omitempty"`
	Filename string `json:"resume_filename,omitempty"`

	// CandidateName is the name the model attributed the letter to, available
	// once generation finished.
	CandidateName string `json:"candidate_name,omitempty"`

	// Outputs, present only once status is completed.
	CoverLetter *string `json:"cover_letter,omitempty"`
	PDFURL      string  `json:"pdf_url,omitempty"`

	// Validation is the AI grounding verdict, present once it has run.
	Validation *ValidationDTO `json:"validation,omitempty"`

	// ErrorMessage is a client-safe explanation when status is failed.
	ErrorMessage string `json:"error_message,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ValidationDTO exposes the grounding check to clients so a user can see *why*
// a letter was flagged before sending it.
type ValidationDTO struct {
	Valid             bool     `json:"valid"`
	UnsupportedClaims []string `json:"unsupported_claims,omitempty"`
	Issues            []string `json:"issues,omitempty"`
}

// NewResponse maps a domain record to its API representation.
func NewResponse(record *CoverLetter) CoverLetterResponse {
	resp := CoverLetterResponse{
		ID:           record.ID,
		Status:       record.Status,
		JobURL:       record.JobURL,
		Tone:         record.Tone,
		Filename:     record.ResumeFilename,
		CreatedAt:    record.CreatedAt,
		UpdatedAt:    record.UpdatedAt,
		ErrorMessage: record.ErrorMessage,
	}

	// The letter text is only meaningful once generation finished; returning it
	// earlier would just be an empty string.
	if record.Status == StatusCompleted && record.CoverLetterText != "" {
		letter := record.CoverLetterText
		resp.CoverLetter = &letter
		resp.PDFURL = PDFURLFor(record.ID)
		resp.CandidateName = record.CandidateName
	}
	if v := record.ValidationSummary(); v != nil {
		resp.Validation = v
	}
	return resp
}

// PDFURLFor builds the download path for a record.
func PDFURLFor(id uint) string {
	return "/api/v1/cover-letters/" + strconv.FormatUint(uint64(id), 10) + "/pdf"
}
