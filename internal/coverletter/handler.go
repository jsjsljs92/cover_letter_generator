package coverletter

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"coverletter/internal/apperror"
	"coverletter/internal/httpx"
	"coverletter/internal/resume"
)

// multipartOverheadAllowance covers the multipart framing (boundaries, part
// headers) on top of the file size limit. Without it a resume of exactly the
// maximum size would be rejected because the framing pushes the body over.
const multipartOverheadAllowance = 8 << 10 // 8 KiB

// Handler exposes the cover letter API over HTTP.
//
// The handler is transport-only: decode the request, enforce the limits the
// transport owns, delegate to the service, render the result. There is no
// business logic here.
type Handler struct {
	service   *Service
	resumes   *resume.Service
	dispatch  Dispatcher
	logger    *slog.Logger
	maxUpload int64
}

// NewHandler builds the HTTP handler.
func NewHandler(service *Service, resumes *resume.Service, dispatch Dispatcher, maxUploadBytes int64, logger *slog.Logger) *Handler {
	return &Handler{
		service:   service,
		resumes:   resumes,
		dispatch:  dispatch,
		logger:    logger,
		maxUpload: maxUploadBytes,
	}
}

// Create handles POST /api/v1/cover-letters.
//
// It responds 202 Accepted: the request is durably recorded and will be
// processed in the background, so the client polls the GET endpoint for the
// result.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Bound the body before parsing so an oversized upload is rejected while
	// streaming, not after buffering.
	r.Body = http.MaxBytesReader(w, r.Body, h.maxUpload+multipartOverheadAllowance)

	if err := r.ParseMultipartForm(4 << 20); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			// 413, not 400: the code identifies a size failure, so it gets the
			// status clients and proxies branch on.
			httpx.WriteError(w, r, apperror.New(apperror.CodePayloadTooLarge,
				"The uploaded resume exceeds the maximum allowed size.", http.StatusRequestEntityTooLarge))
			return
		}
		httpx.WriteError(w, r, apperror.Validation(apperror.CodeMalformedBody,
			"The request must be sent as multipart/form-data."))
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	upload, header, err := r.FormFile("resume")
	if err != nil {
		httpx.WriteError(w, r, apperror.Validation(apperror.CodeInvalidResume,
			"A resume PDF is required."))
		return
	}
	defer func() { _ = upload.Close() }()

	if header.Size > h.maxUpload {
		httpx.WriteError(w, r, apperror.New(apperror.CodePayloadTooLarge,
			"The uploaded resume exceeds the maximum allowed size.", http.StatusRequestEntityTooLarge))
		return
	}

	// Validate and store the upload before creating the record, so a bad file
	// fails immediately without leaving a phantom job behind.
	stored, err := h.resumes.Store(ctx, header.Filename, upload)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	record, err := h.service.Create(ctx, CreateRequest{
		JobURL:         strings.TrimSpace(r.FormValue("job_url")),
		JobDescription: strings.TrimSpace(r.FormValue("job_description")),
		Tone:           strings.TrimSpace(r.FormValue("tone")),
		ResumeKey:      stored.StorageKey,
		ResumeFilename: stored.Filename,
		ResumeBytes:    stored.Bytes,
	}, h.dispatch)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	httpx.WriteJSON(w, r, http.StatusAccepted, NewResponse(record))
}

// Get handles GET /api/v1/cover-letters/{id}.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	record, err := h.service.Get(r.Context(), id)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, NewResponse(record))
}

// DownloadPDF handles GET /api/v1/cover-letters/{id}/pdf.
func (h *Handler) DownloadPDF(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	reader, err := h.service.OpenPDF(r.Context(), id)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	defer func() { _ = reader.Close() }()

	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition",
		`inline; filename="cover-letter-`+strconv.FormatUint(uint64(id), 10)+`.pdf"`)
	w.WriteHeader(http.StatusOK)

	if _, err := io.Copy(w, reader); err != nil {
		// The status line is already sent, so the client sees a truncated
		// download. Recording it is the only useful action left.
		h.logger.Error("failed while streaming cover letter pdf",
			slog.Uint64("cover_letter_id", uint64(id)),
			slog.String("error", err.Error()),
		)
	}
}

// parseID reads and validates the {id} path value.
func parseID(r *http.Request) (uint, error) {
	raw := r.PathValue("id")
	id, err := strconv.ParseUint(raw, 10, 32)
	if err != nil || id == 0 {
		return 0, apperror.Validation(apperror.CodeValidation, "The cover letter id is not valid.")
	}
	return uint(id), nil
}
