// Package coverletter owns the generation workflow's domain: the persisted
// request, its state machine, and the orchestration of extraction, AI and PDF
// generation.
//
// The GORM model in this package is never serialised into an API response.
// Handlers speak DTOs (see dto.go) so the database schema can change without
// breaking clients, and so fields like internal paths are not leaked by accident.
package coverletter

import (
	"encoding/json"
	"strings"
	"time"

	"coverletter/internal/ai"
)

// Status is the lifecycle state of a generation request.
type Status string

// Lifecycle states.
const (
	// StatusCreated means the request is durably recorded but not yet queued
	// for work.
	StatusCreated Status = "created"
	// StatusProcessing means a worker has picked the request up.
	StatusProcessing Status = "processing"
	// StatusCompleted means a PDF is available for download.
	StatusCompleted Status = "completed"
	// StatusFailed means generation stopped with an error.
	StatusFailed Status = "failed"
)

// AllStatuses lists every valid status.
func AllStatuses() []Status {
	return []Status{StatusCreated, StatusProcessing, StatusCompleted, StatusFailed}
}

// IsValid reports whether s is a known status.
func (s Status) IsValid() bool {
	for _, known := range AllStatuses() {
		if s == known {
			return true
		}
	}
	return false
}

// IsTerminal reports whether no further transition is possible.
func (s Status) IsTerminal() bool {
	return s == StatusCompleted || s == StatusFailed
}

// allowedTransitions encodes the state machine. Transitions are checked in the
// repository with a conditional UPDATE, so two concurrent updates cannot both
// win: the loser sees zero affected rows and reports a conflict.
var allowedTransitions = map[Status][]Status{
	StatusCreated:    {StatusProcessing, StatusFailed},
	StatusProcessing: {StatusCompleted, StatusFailed},
	StatusCompleted:  {},
	StatusFailed:     {},
}

// CanTransition reports whether from -> to is a legal transition.
func CanTransition(from, to Status) bool {
	for _, allowed := range allowedTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// CoverLetter is the persisted generation request.
//
// This type is the database shape only. It carries storage paths and internal
// error text that must not reach a client.
type CoverLetter struct {
	ID uint `gorm:"primaryKey"`

	// Input.
	JobURL         string `gorm:"index"`
	JobDescription string

	// Tone is the requested writing style, echoed back for correlation.
	Tone string

	// Resume artefact.
	ResumePath     string
	ResumeFilename string
	// ResumeText is the extracted text; kept so a job can be re-run without
	// re-uploading, and never returned in full to a client.
	ResumeText string

	// Output.
	CoverLetterText string
	CandidateName   string
	PDFPath         string

	// Lifecycle.
	Status       Status `gorm:"index;not null"`
	ErrorMessage string
	// Validation is the JSON-encoded AI validation verdict.
	Validation string `gorm:"type:text"`

	CreatedAt time.Time `gorm:"index"`
	UpdatedAt time.Time
}

// TableName pins the table name so renaming the Go type does not rename the
// table.
func (CoverLetter) TableName() string { return "cover_letters" }

// ResumeStorageKey is the storage key of the uploaded PDF.
//
// The row stores a key relative to the upload directory rather than an absolute
// path, so moving or re-mounting a deployment does not orphan every stored
// resume.
func (c *CoverLetter) ResumeStorageKey() string { return c.ResumePath }

// PDFStorageKey is the storage key of the generated PDF, relative to the PDF
// directory.
func (c *CoverLetter) PDFStorageKey() string { return c.PDFPath }

// ValidationSummary decodes the stored validation verdict for presentation.
//
// A corrupt or absent payload yields nil rather than an error: validation is
// advisory metadata, and a row with unreadable metadata should still be
// downloadable.
func (c *CoverLetter) ValidationSummary() *ValidationDTO {
	if strings.TrimSpace(c.Validation) == "" {
		return nil
	}
	var verdict ai.Validation
	if err := json.Unmarshal([]byte(c.Validation), &verdict); err != nil {
		return nil
	}
	return &ValidationDTO{
		Valid:             verdict.Valid,
		UnsupportedClaims: verdict.UnsupportedClaims,
		Issues:            verdict.Issues,
	}
}
