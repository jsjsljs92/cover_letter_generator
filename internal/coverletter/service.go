package coverletter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"coverletter/internal/ai"
	"coverletter/internal/apperror"
	"coverletter/internal/job"
	"coverletter/internal/obs"
	"coverletter/internal/pdf"
	"coverletter/internal/resume"
	"coverletter/internal/storage"
)

// Dispatcher hands a record id to the background worker pool.
//
// It is an interface rather than a concrete pool so the service can be tested
// without running workers, and so the execution strategy (in-process pool
// today, a durable queue later) is a wiring decision.
type Dispatcher interface {
	Dispatch(ctx context.Context, id uint) error
}

// Service orchestrates the generation workflow.
//
// It owns two distinct operations with very different characteristics:
//
//   - Create is called from an HTTP handler. It validates input, stores the
//     upload, persists the record and dispatches work. It must return quickly.
//   - Process is called from a worker. It does the slow work — extraction, AI,
//     PDF — and updates the record's state as it goes.
type Service struct {
	repo     Repository
	resumes  *resume.Service
	jobs     *job.Service
	agent    ai.Agent
	pdfGen   pdf.Generator
	pdfStore storage.Storage
	metrics  *obs.Registry
	logger   *slog.Logger
	timeout  time.Duration
}

// Config configures the service.
type Config struct {
	// GenerationTimeout bounds a single Process call end to end.
	GenerationTimeout time.Duration
}

// NewService wires the service.
func NewService(
	repo Repository,
	resumes *resume.Service,
	jobs *job.Service,
	agent ai.Agent,
	pdfGen pdf.Generator,
	pdfStore storage.Storage,
	cfg Config,
	metrics *obs.Registry,
	logger *slog.Logger,
) *Service {
	if cfg.GenerationTimeout <= 0 {
		cfg.GenerationTimeout = 5 * time.Minute
	}
	return &Service{
		repo:     repo,
		resumes:  resumes,
		jobs:     jobs,
		agent:    agent,
		pdfGen:   pdfGen,
		pdfStore: pdfStore,
		metrics:  metrics,
		logger:   logger,
		timeout:  cfg.GenerationTimeout,
	}
}

// Create validates the request, persists it and dispatches background work.
//
// The record is written before dispatch so a dispatch failure still leaves an
// auditable, failed row rather than a silently dropped upload. A full queue is
// surfaced as a retryable 503 rather than being buffered indefinitely.
func (s *Service) Create(ctx context.Context, req CreateRequest, dispatch Dispatcher) (*CoverLetter, error) {
	req.JobURL = strings.TrimSpace(req.JobURL)
	req.JobDescription = strings.TrimSpace(req.JobDescription)
	req.Tone = strings.TrimSpace(req.Tone)

	if err := validateInput(req); err != nil {
		return nil, err
	}

	// Pre-flight the job URL so an SSRF attempt is refused at submit time
	// instead of being accepted with a 202 and failing later in a worker. The
	// fetcher still re-checks at dial time; this is the fast path, not the
	// guarantee.
	if req.JobURL != "" && s.jobs != nil {
		if err := s.jobs.ValidateURL(ctx, req.JobURL); err != nil {
			return nil, err
		}
	}

	record := &CoverLetter{
		JobURL:         req.JobURL,
		JobDescription: req.JobDescription,
		Tone:           req.Tone,
		ResumePath:     req.ResumeKey,
		ResumeFilename: req.ResumeFilename,
		Status:         StatusCreated,
	}
	if err := s.repo.Create(ctx, record); err != nil {
		return nil, err
	}

	if dispatch != nil {
		if err := dispatch.Dispatch(ctx, record.ID); err != nil {
			// The request is recorded but will never be processed. Mark it
			// failed so a client polling the id sees why.
			s.markFailed(context.WithoutCancel(ctx), record.ID, apperror.Message(err))
			if errors.Is(err, apperror.ErrQueueFull) {
				return nil, apperror.Unavailable(apperror.CodeQueueFull,
					"The service is busy. Please retry shortly.")
			}
			s.logger.Error("failed to dispatch cover letter",
				slog.Uint64("cover_letter_id", uint64(record.ID)),
				slog.String("error", err.Error()),
			)
			return nil, apperror.Unavailable(apperror.CodeUnavailable,
				"The request could not be queued for processing.")
		}
	}

	// Move to processing as soon as the work is queued, so the status the client
	// sees matches reality: the request is in flight.
	if err := s.repo.TransitionStatus(ctx, record.ID, []Status{StatusCreated}, StatusProcessing, ""); err != nil {
		s.logger.Warn("could not mark cover letter as processing",
			slog.Uint64("cover_letter_id", uint64(record.ID)),
			slog.String("error", err.Error()),
		)
	}
	record.Status = StatusProcessing

	return record, nil
}

// validateInput enforces the create contract. The job input check is
// deliberately strict: accepting both a URL and a description would leave the
// user guessing which one was actually analysed.
func validateInput(req CreateRequest) error {
	switch {
	case req.JobURL == "" && req.JobDescription == "":
		return apperror.Validation(apperror.CodeInvalidJobInput,
			"Provide either a job URL or a job description.")
	case req.JobURL != "" && req.JobDescription != "":
		return apperror.Validation(apperror.CodeInvalidJobInput,
			"Provide either a job URL or a job description, not both.")
	}

	if req.JobURL != "" {
		if _, err := job.ValidateJobURL(req.JobURL); err != nil {
			return err
		}
	}
	if req.ResumeKey == "" {
		return apperror.Validation(apperror.CodeInvalidResume, "A resume PDF is required.")
	}
	if len(req.Tone) > 64 {
		return apperror.Validation(apperror.CodeValidation, "Tone must be 64 characters or fewer.")
	}
	return nil
}

// Process runs the generation workflow for one record.
//
// It is written so that every exit path leaves the record in a terminal state:
// success marks it completed, and any failure — including a panic-free early
// return — marks it failed with a client-safe message.
func (s *Service) Process(ctx context.Context, id uint) error {
	start := time.Now()
	defer func() { s.metrics.Observe("coverletter.process.duration", time.Since(start)) }()

	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	record, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return err
	}

	if record.Status != StatusProcessing {
		// Either already finished or never started; doing the work twice would
		// overwrite a result a user may already have downloaded.
		s.logger.Debug("skipping cover letter that is not processing",
			slog.Uint64("cover_letter_id", uint64(id)),
			slog.String("status", string(record.Status)),
		)
		return nil
	}

	_, err = s.runWorkflow(ctx, record)
	if err != nil {
		s.markFailed(ctx, id, apperror.Message(err))
		s.logger.Error("cover letter generation failed",
			slog.Uint64("cover_letter_id", uint64(id)),
			slog.String("code", apperror.Code(err)),
			slog.String("error", errorDetail(err)),
			slog.Duration("duration", time.Since(start)),
		)
		return err
	}
	return nil
}

// runWorkflow performs the actual work of a generation request.
func (s *Service) runWorkflow(ctx context.Context, record *CoverLetter) (*ai.Result, error) {
	// Stage 1: resolve the job description from whichever source was supplied.
	jobCtx, cancelJob := context.WithTimeout(ctx, s.timeout)
	jobDesc, err := s.jobs.Resolve(jobCtx, record.JobURL, record.JobDescription)
	cancelJob()
	if err != nil {
		return nil, err
	}

	// Stage 2: extract the resume text from the stored upload.
	stored, err := s.resumes.StoredFromKey(record.ResumeStorageKey(), record.ResumeFilename, 0)
	if err != nil {
		return nil, err
	}
	resumeText, err := s.resumes.Parse(ctx, stored)
	if err != nil {
		return nil, err
	}

	// Persist the extracted inputs before the expensive AI call, so a failure
	// later still leaves the record diagnosable.
	record.JobDescription = jobDesc.Text
	record.ResumeText = resumeText.Text
	if err := s.repo.Update(ctx, record); err != nil {
		return nil, err
	}

	// Stages 3-5: analyse, match, generate and validate. The agent owns its own
	// retry loop, so failures here are genuinely terminal.
	s.metrics.Inc("coverletter.agent.started")
	result, err := s.agent.Run(ctx, ai.Input{
		Job:    *jobDesc,
		Resume: *resumeText,
		Tone:   record.Tone,
	})
	if err != nil {
		return nil, err
	}

	// Stage 6: render the PDF. Deterministic, and deliberately not something
	// the model is asked to produce.
	pdfCtx, cancelPDF := context.WithTimeout(ctx, 30*time.Second)
	defer cancelPDF()

	generated, err := s.pdfGen.Generate(pdfCtx, pdf.Request{
		CandidateName: result.Resume.CandidateName,
		JobTitle:      firstNonEmpty(result.Job.JobTitle, jobDesc.Title),
		Company:       firstNonEmpty(result.Job.Company, jobDesc.Company),
		Subject:       result.Letter.Subject,
		Body:          result.Letter.Body,
		Date:          time.Now().UTC(),
	})
	if err != nil {
		s.metrics.Inc("coverletter.pdf.failed")
		return nil, err
	}
	s.metrics.Inc("coverletter.pdf.succeeded")

	key := fmt.Sprintf("cover-letters/%d/cover-letter.pdf", record.ID)
	if _, err := s.pdfStore.Put(pdfCtx, key, generated); err != nil {
		return nil, apperror.Wrap(err, apperror.CodeStorage, "The cover letter PDF could not be stored.", 500)
	}

	// Stage 7: commit the outputs and mark the record complete.
	record.CoverLetterText = result.Letter.Body
	record.CandidateName = result.Resume.CandidateName
	record.PDFPath = key
	record.Validation = encodeValidation(result.Validation)
	if err := s.repo.Update(ctx, record); err != nil {
		return nil, err
	}

	if err := s.repo.TransitionStatus(ctx, record.ID, []Status{StatusProcessing}, StatusCompleted, ""); err != nil {
		return nil, err
	}

	s.metrics.Inc("coverletter.completed")
	return result, nil
}

// Get returns a record by id.
func (s *Service) Get(ctx context.Context, id uint) (*CoverLetter, error) {
	if id == 0 {
		return nil, apperror.Validation(apperror.CodeValidation, "A cover letter id is required.")
	}
	return s.repo.GetByID(ctx, id)
}

// OpenPDF streams the generated PDF for a record.
//
// The status check is what stops a client downloading a half-written file: the
// PDF path is only set in the same commit that marks the record completed.
func (s *Service) OpenPDF(ctx context.Context, id uint) (io.ReadCloser, error) {
	record, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if record.Status != StatusCompleted || record.PDFPath == "" {
		return nil, apperror.Conflict(apperror.CodeInvalidState,
			"The cover letter PDF is not ready yet.")
	}

	reader, err := s.pdfStore.Open(ctx, record.PDFStorageKey())
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			s.logger.Error("cover letter record references a missing pdf",
				slog.Uint64("cover_letter_id", uint64(id)),
				slog.String("key", record.PDFStorageKey()),
			)
			return nil, apperror.Internal(err, "The cover letter PDF is missing from storage.")
		}
		return nil, apperror.Wrap(err, apperror.CodeStorage, "The cover letter PDF could not be read.", 500)
	}
	return reader, nil
}

// markFailed records a terminal failure with a message that is safe to show a
// user.
func (s *Service) markFailed(ctx context.Context, id uint, message string) {
	if message == "" {
		message = "Cover letter generation failed."
	}
	// Use a fresh context: the workflow context is often already cancelled by
	// the time this runs, and the status update must still happen.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	err := s.repo.TransitionStatus(ctx, id, []Status{StatusCreated, StatusProcessing}, StatusFailed, message)
	switch {
	case err == nil:
		s.metrics.Inc("coverletter.failed")
		return
	case errors.Is(err, ErrTransitionConflict):
		// Already terminal; nothing to do and nothing is wrong.
		s.logger.Debug("cover letter already in a terminal state",
			slog.Uint64("cover_letter_id", uint64(id)),
			slog.String("error", err.Error()),
		)
	default:
		s.logger.Error("could not record cover letter failure",
			slog.Uint64("cover_letter_id", uint64(id)),
			slog.String("error", err.Error()),
		)
	}
}

// encodeValidation serialises the validation verdict for storage. A failure to
// marshal is not worth failing the request over: the letter itself is intact.
func encodeValidation(verdict ai.Validation) string {
	payload, err := json.Marshal(verdict)
	if err != nil {
		return ""
	}
	return string(payload)
}

// errorDetail returns the most descriptive message available for logs.
func errorDetail(err error) string {
	if cause := apperror.Cause(err); cause != nil {
		return fmt.Sprintf("%v: %v", apperror.Message(err), cause)
	}
	return apperror.Message(err)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
