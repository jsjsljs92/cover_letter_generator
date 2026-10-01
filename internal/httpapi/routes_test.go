package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"coverletter/internal/ai"
	"coverletter/internal/aitest"
	"coverletter/internal/apperror"
	"coverletter/internal/config"
	"coverletter/internal/coverletter"
	"coverletter/internal/database"
	"coverletter/internal/httpx"
	"coverletter/internal/job"
	"coverletter/internal/logging"
	"coverletter/internal/migrate"
	"coverletter/internal/obs"
	"coverletter/internal/pdf"
	"coverletter/internal/resume"
	"coverletter/internal/storage"
	"coverletter/internal/worker"

	"coverletter/migrations"
)

// The router takes a *coverletter.Handler, which needs a real *coverletter.Service.
// Rather than duplicate the application's composition root here, these tests
// build the same collaborator graph by hand: a real GORM repository on a real
// SQLite database with the real embedded migration, real local storage, the
// real PDF generator, and the scripted AI provider. That keeps the HTTP tests
// honest about status codes, headers and error envelopes while staying off the
// network and deterministic.

// dispatcher is the fake for the background work queue: it records the ids it
// is handed and, when configured to fail, reports the failure.
//
// It deliberately does not run the workflow inline. Create dispatches before
// it moves the record from "created" to "processing", so a synchronous
// dispatcher would hand Process a record still in "created" and it would
// correctly no-op. Tests drive Process themselves through api.complete.
type dispatcher struct {
	mu  sync.Mutex
	ids []uint
	err error
}

func (d *dispatcher) Dispatch(_ context.Context, id uint) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ids = append(d.ids, id)
	return d.err
}

func (d *dispatcher) dispatched() []uint {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]uint(nil), d.ids...)
}

// api is a wired router plus the handles a test needs to inspect state.
type api struct {
	t          *testing.T
	router     http.Handler
	service    *coverletter.Service
	repo       *coverletter.GORMRepository
	pdfs       *storage.LocalStorage
	dispatcher *dispatcher
	registry   *obs.Registry
	server     *httptest.Server
}

const (
	// testMaxUpload is small enough that an oversized body is cheap to build,
	// while still leaving room for the multipart framing allowance.
	testMaxUpload = 4096
	// longJobDescription clears the job service's minimum-length check.
	longJobDescription = "We are hiring a backend engineer to work on our payments platform. " +
		"You will build and operate Go services and own their on-call rotation."
)

// newAPI builds an isolated router. Each call gets its own temp directory, so
// no database file, storage root or registry is shared between tests.
func newAPI(t *testing.T, configure func(*api)) *api {
	t.Helper()

	dir := t.TempDir()
	ctx := context.Background()

	db, err := database.Open(ctx, config.DatabaseConfig{
		Path:            filepath.Join(dir, "coverletter.db"),
		MaxOpenConns:    1,
		MaxIdleConns:    1,
		ConnMaxLifetime: time.Hour,
	})
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = database.Close(db) })

	if err := migrate.Run(ctx, db, migrations.FS); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}

	uploads, err := storage.NewLocal(filepath.Join(dir, "uploads"))
	if err != nil {
		t.Fatalf("creating upload storage: %v", err)
	}
	pdfs, err := storage.NewLocal(filepath.Join(dir, "pdfs"))
	if err != nil {
		t.Fatalf("creating pdf storage: %v", err)
	}

	registry := obs.New()
	logger := logging.Discard()
	provider := aitest.DefaultResponses()

	resumes := resume.NewService(uploads, uploads, resume.NewPDFParser(12000), testMaxUpload, registry, logger)
	agent := ai.NewAgent(provider, ai.AgentConfig{MaxAttempts: 2}, registry, logger)
	jobSvc := job.NewService(nil, nil, registry)
	repo := coverletter.NewRepository(db)
	service := coverletter.NewService(repo, resumes, jobSvc, agent, pdf.NewGenerator(), pdfs,
		coverletter.Config{GenerationTimeout: 30 * time.Second}, registry, logger)

	disp := &dispatcher{}
	handler := coverletter.NewHandler(service, resumes, disp, testMaxUpload, logger)

	a := &api{
		t:          t,
		router:     NewRouter(Dependencies{CoverLetters: handler, Metrics: registry}),
		service:    service,
		repo:       repo,
		pdfs:       pdfs,
		dispatcher: disp,
		registry:   registry,
	}
	if configure != nil {
		configure(a)
	}
	return a
}

// resumePDF is a real, parseable PDF that satisfies the magic-byte check and
// the parser's minimum-text requirement.
func resumePDF(t *testing.T) []byte {
	t.Helper()

	body := strings.Repeat(
		"Ada Lovelace, Senior Engineer at Globex. Built the payment services platform in Go. ", 4)
	data, err := pdf.NewGenerator().Generate(context.Background(), pdf.Request{
		CandidateName: "Ada Lovelace",
		Subject:       "Resume",
		Body:          body,
		Date:          time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("generating resume pdf: %v", err)
	}
	return data
}

// multipartRequest builds a submission with the given form fields and an
// optional resume part. A fileName of "" omits the file entirely.
func multipartRequest(t *testing.T, fields map[string]string, fileName string, fileBody []byte) *http.Request {
	t.Helper()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for k, v := range fields {
		if err := writer.WriteField(k, v); err != nil {
			t.Fatalf("writing form field %q: %v", k, err)
		}
	}
	if fileName != "" {
		part, err := writer.CreateFormFile("resume", fileName)
		if err != nil {
			t.Fatalf("creating the resume part: %v", err)
		}
		if _, err := part.Write(fileBody); err != nil {
			t.Fatalf("writing the resume part: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("closing the multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/cover-letters", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req
}

// submit posts a multipart submission and returns the recorded response.
func (a *api) submit(fields map[string]string, fileName string, fileBody []byte) *httptest.ResponseRecorder {
	a.t.Helper()
	rec := httptest.NewRecorder()
	a.router.ServeHTTP(rec, multipartRequest(a.t, fields, fileName, fileBody))
	return rec
}

func (a *api) get(path string) *httptest.ResponseRecorder {
	a.t.Helper()
	rec := httptest.NewRecorder()
	a.router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// complete runs the background workflow for a record, standing in for the
// worker pool. Create only queues the work, so a test that needs a finished
// record has to run it.
func (a *api) complete(id uint) {
	a.t.Helper()
	if err := a.service.Process(context.Background(), id); err != nil {
		a.t.Fatalf("Process(%d) error = %v", id, err)
	}
}

// create posts a valid submission and returns the new record's id, failing the
// test if the submission was not accepted.
func (a *api) create() uint {
	a.t.Helper()
	rec := a.submit(map[string]string{"job_description": longJobDescription}, "resume.pdf", resumePDF(a.t))
	if rec.Code != http.StatusAccepted {
		a.t.Fatalf("create status = %d, want 202 (body: %s)", rec.Code, rec.Body.String())
	}
	var body struct {
		ID uint `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		a.t.Fatalf("decoding the create response: %v (body: %s)", err, rec.Body.String())
	}
	if body.ID == 0 {
		a.t.Fatal("the create response carries no id")
	}
	return body.ID
}

// errorEnvelope is the decoded failure body every endpoint must return.
type errorEnvelope struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	} `json:"error"`
}

// assertErrorEnvelope checks the full contract of a failure response: the
// status, the stable code, a non-empty safe message, a correlation id, and —
// just as importantly — that nothing internal leaked into the body.
func assertErrorEnvelope(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode string) errorEnvelope {
	t.Helper()

	if rec.Code != wantStatus {
		t.Errorf("status = %d, want %d (body: %s)", rec.Code, wantStatus, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	body := rec.Body.String()

	// The envelope must be the only top-level key: a stray field would mean
	// something unvetted reached the response.
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &top); err != nil {
		t.Fatalf("decoding the response as JSON: %v (body: %s)", err, body)
	}
	if len(top) != 1 {
		t.Errorf("response has %d top-level keys (%v), want exactly \"error\"", len(top), keysOf(top))
	}
	if _, ok := top["error"]; !ok {
		t.Fatalf("response has no \"error\" key: %s", body)
	}

	var envelope errorEnvelope
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("decoding the error envelope: %v (body: %s)", err, body)
	}
	if envelope.Error.Code != wantCode {
		t.Errorf("error code = %q, want %q (body: %s)", envelope.Error.Code, wantCode, body)
	}
	if strings.TrimSpace(envelope.Error.Message) == "" {
		t.Error("error message is empty")
	}
	if envelope.Error.RequestID == "" {
		t.Error("error envelope carries no request_id")
	}
	if got := rec.Header().Get("X-Request-ID"); got != envelope.Error.RequestID {
		t.Errorf("X-Request-ID header = %q, want it to match the envelope's %q",
			got, envelope.Error.RequestID)
	}

	assertNoLeak(t, body)
	return envelope
}

// leakPatterns are substrings that must never appear in a client-facing body.
var leakPatterns = []string{
	"goroutine ",                    // stack trace
	".go:",                          // file:line from a panic or wrapped error
	"panic:",                        // panic value
	"SELECT ", "INSERT ", "UPDATE ", // SQL text
	"sqlite", // driver detail
	"no such table", "no such column",
	"GEMINI_API_KEY", "api_key", "apiKey", "Bearer ",
	"sk-live", "AIza",
	"coverletter/internal/", // internal package path
	"/var/folders", "/tmp/", // local filesystem layout
	// A storage key quoted as a JSON string. The download URL legitimately
	// contains "cover-letters", so the key is only recognisable in this form:
	// a JSON-quoted relative path, which never appears inside a URL path.
	"\"resumes/",
}

func assertNoLeak(t *testing.T, body string) {
	t.Helper()
	for _, pattern := range leakPatterns {
		if strings.Contains(body, pattern) {
			t.Errorf("response body leaks %q: %s", pattern, body)
		}
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestHealth(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	NewRouter(Dependencies{Metrics: obs.New()}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != `{"status":"ok"}` {
		t.Errorf("body = %q, want %q", got, `{"status":"ok"}`)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if rec.Header().Get("X-Request-ID") == "" {
		t.Error("no X-Request-ID header on the response")
	}
}

// TestHealthDoesNotProbeDependencies guards the liveness contract: /health
// must not fail just because the registry is empty, since a liveness probe
// that restarts a healthy process is worse than no probe.
func TestHealthIsIndependentOfTraffic(t *testing.T) {
	t.Parallel()

	a := newAPI(t, nil)
	// Generate some traffic so the registry is populated, then check health
	// still reports the same thing.
	a.get("/metrics")
	rec := a.get("/health")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != `{"status":"ok"}` {
		t.Errorf("body = %q, want %q", got, `{"status":"ok"}`)
	}
}

func TestMetrics(t *testing.T) {
	t.Parallel()

	a := newAPI(t, nil)
	a.get("/health")
	a.get("/api/v1/cover-letters/9999")

	rec := a.get("/metrics")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	var snapshot obs.Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snapshot); err != nil {
		t.Fatalf("decoding the metrics snapshot: %v (body: %s)", err, rec.Body.String())
	}

	// The registry always renders all three sections, so a scraper can rely on
	// the shape rather than handling absent fields.
	for name, section := range map[string]map[string]int64{
		"counters": snapshot.Counters,
		"gauges":   snapshot.Gauges,
	} {
		if section == nil {
			t.Errorf("metrics %s section is null, want an object", name)
		}
	}
	if snapshot.Timings == nil {
		t.Error("metrics timings section is null, want an object")
	}

	// Requests are counted per method and status, which is how an operator
	// distinguishes a 404 flood from a healthy endpoint.
	healthCalls := snapshot.Counters["http.requests.total.get.status_200"]
	if healthCalls < 1 {
		t.Errorf("http.requests.total.get.status_200 = %d, want at least 1", healthCalls)
	}
	notFound := snapshot.Counters["http.requests.total.get.status_404"]
	if notFound < 1 {
		t.Errorf("http.requests.total.get.status_404 = %d, want at least 1", notFound)
	}
	if _, ok := snapshot.Timings["http.request.duration"]; !ok {
		t.Error("http.request.duration was not observed")
	}
}

func TestCreateAccepted(t *testing.T) {
	t.Parallel()

	a := newAPI(t, nil)
	rec := a.submit(map[string]string{
		"job_description": longJobDescription,
		"tone":            "enthusiastic",
	}, "resume.pdf", resumePDF(t))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	var body struct {
		ID        uint   `json:"id"`
		Status    string `json:"status"`
		Tone      string `json:"tone"`
		Filename  string `json:"resume_filename"`
		CreatedAt string `json:"created_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the response: %v (body: %s)", err, rec.Body.String())
	}
	if body.ID == 0 {
		t.Error("response has no id")
	}
	// The request is durably recorded and in flight, so "processing" is what a
	// polling client must be told to expect.
	if body.Status != string(coverletter.StatusProcessing) {
		t.Errorf("status = %q, want %q", body.Status, coverletter.StatusProcessing)
	}
	if body.Tone != "enthusiastic" {
		t.Errorf("tone = %q, want %q", body.Tone, "enthusiastic")
	}
	if body.Filename != "resume.pdf" {
		t.Errorf("resume_filename = %q, want %q", body.Filename, "resume.pdf")
	}
	if body.CreatedAt == "" {
		t.Error("created_at is missing from the response")
	}

	// The id is usable immediately: a client polls it without any delay.
	follow := a.get("/api/v1/cover-letters/" + itoa(body.ID))
	if follow.Code != http.StatusOK {
		t.Errorf("follow-up GET status = %d, want 200 (body: %s)", follow.Code, follow.Body.String())
	}

	// The work reached the dispatcher exactly once.
	ids := a.dispatcher.dispatched()
	if len(ids) != 1 || ids[0] != body.ID {
		t.Errorf("dispatcher received %v, want exactly [%d]", ids, body.ID)
	}
}

// TestCreateStoresTheUploadUnderAServerGeneratedKey guards the rule that a
// client-supplied filename can never influence where the file lands.
func TestCreateStoresTheUploadUnderAServerGeneratedKey(t *testing.T) {
	t.Parallel()

	a := newAPI(t, nil)
	rec := a.submit(map[string]string{
		"job_description": longJobDescription,
	}, "../../etc/passwd", resumePDF(t))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body: %s)", rec.Code, rec.Body.String())
	}

	record, err := a.repo.GetByID(context.Background(), 1)
	if err != nil {
		t.Fatalf("loading the record: %v", err)
	}
	if strings.Contains(record.ResumePath, "..") {
		t.Errorf("stored resume key contains a traversal: %q", record.ResumePath)
	}
	if !strings.HasPrefix(record.ResumePath, "resumes/") {
		t.Errorf("stored resume key = %q, want a key under resumes/", record.ResumePath)
	}
	if !strings.HasSuffix(record.ResumePath, ".pdf") {
		t.Errorf("stored resume key = %q, want a .pdf suffix", record.ResumePath)
	}
	// The hostile name is sanitised for display, not honoured as a path.
	if record.ResumeFilename != "passwd" {
		t.Errorf("ResumeFilename = %q, want the path components stripped", record.ResumeFilename)
	}
}

func TestCreateFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		fields     map[string]string
		fileName   string
		fileBody   []byte
		wantStatus int
		wantCode   string
	}{
		{
			name:       "missing resume part",
			fields:     map[string]string{"job_description": longJobDescription},
			fileName:   "",
			wantStatus: http.StatusBadRequest,
			wantCode:   apperror.CodeInvalidResume,
		},
		{
			name:       "empty resume part",
			fields:     map[string]string{"job_description": longJobDescription},
			fileName:   "resume.pdf",
			fileBody:   nil,
			wantStatus: http.StatusBadRequest,
			wantCode:   apperror.CodeUnsupportedType,
		},
		{
			name: "both job url and job description",
			fields: map[string]string{
				"job_url":         "https://jobs.example.com/1",
				"job_description": longJobDescription,
			},
			fileName:   "resume.pdf",
			fileBody:   []byte("%PDF-1.4 placeholder"),
			wantStatus: http.StatusBadRequest,
			wantCode:   apperror.CodeInvalidJobInput,
		},
		{
			name: "neither job url nor job description",
			fields: map[string]string{
				"tone": "professional",
			},
			fileName:   "resume.pdf",
			fileBody:   []byte("%PDF-1.4 placeholder"),
			wantStatus: http.StatusBadRequest,
			wantCode:   apperror.CodeInvalidJobInput,
		},
		{
			name: "non-http job url scheme",
			fields: map[string]string{
				"job_url": "file:///etc/passwd",
			},
			fileName:   "resume.pdf",
			fileBody:   []byte("%PDF-1.4 placeholder"),
			wantStatus: http.StatusBadRequest,
			wantCode:   apperror.CodeInvalidJobInput,
		},
		{
			name: "job url with credentials",
			fields: map[string]string{
				"job_url": "https://user:secret@jobs.example.com/1",
			},
			fileName:   "resume.pdf",
			fileBody:   []byte("%PDF-1.4 placeholder"),
			wantStatus: http.StatusBadRequest,
			wantCode:   apperror.CodeInvalidJobInput,
		},
		{
			// A file that lies about its name: the magic bytes, not the
			// extension, decide whether it is a PDF.
			name: "non-pdf content named resume.pdf",
			fields: map[string]string{
				"job_description": longJobDescription,
			},
			fileName:   "resume.pdf",
			fileBody:   []byte("GIF89a this is definitely not a pdf"),
			wantStatus: http.StatusBadRequest,
			wantCode:   apperror.CodeUnsupportedType,
		},
		{
			name: "oversized upload",
			fields: map[string]string{
				"job_description": longJobDescription,
			},
			fileName: "resume.pdf",
			fileBody: bytes.Repeat([]byte("A"), testMaxUpload+16<<10),
			// 413, not 400: the code is a size failure and clients branch on
			// the status.
			wantStatus: http.StatusRequestEntityTooLarge,
			wantCode:   apperror.CodePayloadTooLarge,
		},
		{
			name: "tone longer than the limit",
			fields: map[string]string{
				"job_description": longJobDescription,
				"tone":            strings.Repeat("t", 65),
			},
			fileName:   "resume.pdf",
			fileBody:   []byte("%PDF-1.4 placeholder"),
			wantStatus: http.StatusBadRequest,
			wantCode:   apperror.CodeValidation,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := newAPI(t, nil)
			body := tc.fileBody
			if tc.fileName == "resume.pdf" && len(body) == 0 && tc.wantCode == apperror.CodeUnsupportedType {
				body = nil
			}
			rec := a.submit(tc.fields, tc.fileName, body)

			assertErrorEnvelope(t, rec, tc.wantStatus, tc.wantCode)

			// A rejected request must not leave a phantom job or consume a slot.
			if ids := a.dispatcher.dispatched(); len(ids) != 0 {
				t.Errorf("dispatcher was called for a rejected request: %v", ids)
			}
			if _, err := a.repo.GetByID(context.Background(), 1); err == nil {
				t.Error("a rejected request was persisted")
			}
		})
	}
}

// TestCreateRejectsNonMultipartBody covers the transport-level guard: a JSON
// body is rejected with its own code rather than being misread as a form.
func TestCreateRejectsNonMultipartBody(t *testing.T) {
	t.Parallel()

	a := newAPI(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cover-letters",
		strings.NewReader(`{"job_description":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.router.ServeHTTP(rec, req)

	assertErrorEnvelope(t, rec, http.StatusBadRequest, apperror.CodeMalformedBody)
}

// TestCreateReportsBackpressure covers the queue-full path end to end: the
// client gets a retryable 503 rather than a request that appears accepted.
func TestCreateReportsBackpressure(t *testing.T) {
	t.Parallel()

	a := newAPI(t, func(a *api) { a.dispatcher.err = worker.ErrQueueFull })
	rec := a.submit(map[string]string{"job_description": longJobDescription}, "resume.pdf", resumePDF(t))

	assertErrorEnvelope(t, rec, http.StatusServiceUnavailable, apperror.CodeQueueFull)

	// The upload was recorded before dispatch was attempted, so the record is
	// marked failed rather than left dangling.
	record, err := a.repo.GetByID(context.Background(), 1)
	if err != nil {
		t.Fatalf("the request was not recorded at all: %v", err)
	}
	if record.Status != coverletter.StatusFailed {
		t.Errorf("status = %q, want %q", record.Status, coverletter.StatusFailed)
	}
}

// TestCreateReportsGenericDispatchFailure covers the other dispatch branch, and
// asserts the internal cause is not echoed to the client.
func TestCreateReportsGenericDispatchFailure(t *testing.T) {
	t.Parallel()

	a := newAPI(t, func(a *api) {
		a.dispatcher.err = worker.ErrPoolStopped
	})
	rec := a.submit(map[string]string{"job_description": longJobDescription}, "resume.pdf", resumePDF(t))

	assertErrorEnvelope(t, rec, http.StatusServiceUnavailable, apperror.CodeUnavailable)
	assertNoLeak(t, rec.Body.String())
}

func TestGetCoverLetter(t *testing.T) {
	t.Parallel()

	a := newAPI(t, nil)
	created := a.submit(map[string]string{
		"job_description": longJobDescription,
		"tone":            "professional",
	}, "resume.pdf", resumePDF(t))
	if created.Code != http.StatusAccepted {
		t.Fatalf("create status = %d, want 202 (body: %s)", created.Code, created.Body.String())
	}

	rec := a.get("/api/v1/cover-letters/1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var body struct {
		ID          uint   `json:"id"`
		Status      string `json:"status"`
		JobURL      string `json:"job_url"`
		Tone        string `json:"tone"`
		Filename    string `json:"resume_filename"`
		CoverLetter string `json:"cover_letter"`
		PDFURL      string `json:"pdf_url"`
		CreatedAt   string `json:"created_at"`
		UpdatedAt   string `json:"updated_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the response: %v (body: %s)", err, rec.Body.String())
	}
	if body.ID != 1 {
		t.Errorf("id = %d, want 1", body.ID)
	}
	if body.Status != string(coverletter.StatusProcessing) {
		t.Errorf("status = %q, want %q", body.Status, coverletter.StatusProcessing)
	}
	if body.Tone != "professional" {
		t.Errorf("tone = %q, want %q", body.Tone, "professional")
	}
	if body.CreatedAt == "" || body.UpdatedAt == "" {
		t.Error("timestamps are missing from the response")
	}
	// Nothing is generated yet, so no outputs may be advertised.
	if body.CoverLetter != "" {
		t.Errorf("cover_letter = %q, want empty before completion", body.CoverLetter)
	}
	if body.PDFURL != "" {
		t.Errorf("pdf_url = %q, want empty before completion", body.PDFURL)
	}
	assertNoLeak(t, rec.Body.String())
}

func TestGetCoverLetterFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		path       string
		wantStatus int
		wantCode   string
	}{
		{name: "non-numeric id", path: "/api/v1/cover-letters/abc", wantStatus: 400, wantCode: apperror.CodeValidation},
		{name: "empty id segment is not routable", path: "/api/v1/cover-letters/", wantStatus: 404, wantCode: apperror.CodeNotFound},
		{name: "zero id", path: "/api/v1/cover-letters/0", wantStatus: 400, wantCode: apperror.CodeValidation},
		{name: "negative id", path: "/api/v1/cover-letters/-1", wantStatus: 400, wantCode: apperror.CodeValidation},
		{name: "id overflowing uint32", path: "/api/v1/cover-letters/99999999999999", wantStatus: 400, wantCode: apperror.CodeValidation},
		{name: "unknown id", path: "/api/v1/cover-letters/4242", wantStatus: 404, wantCode: apperror.CodeNotFound},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := newAPI(t, nil)
			rec := a.get(tc.path)
			assertErrorEnvelope(t, rec, tc.wantStatus, tc.wantCode)
		})
	}
}

// TestGetCompletedCoverLetter drives the record to completion and checks the
// response advertises the outputs, and only the outputs.
func TestGetCompletedCoverLetter(t *testing.T) {
	t.Parallel()

	a := newAPI(t, nil)
	id := a.create()
	a.complete(id)

	rec := a.get("/api/v1/cover-letters/1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var body struct {
		ID          uint   `json:"id"`
		Status      string `json:"status"`
		CoverLetter string `json:"cover_letter"`
		PDFURL      string `json:"pdf_url"`
		Validation  *struct {
			Valid             bool     `json:"valid"`
			UnsupportedClaims []string `json:"unsupported_claims"`
			Issues            []string `json:"issues"`
		} `json:"validation"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the response: %v (body: %s)", err, rec.Body.String())
	}
	if body.Status != string(coverletter.StatusCompleted) {
		t.Fatalf("status = %q, want %q", body.Status, coverletter.StatusCompleted)
	}
	if strings.TrimSpace(body.CoverLetter) == "" {
		t.Error("cover_letter is empty for a completed record")
	}
	if want := coverletter.PDFURLFor(1); body.PDFURL != want {
		t.Errorf("pdf_url = %q, want %q", body.PDFURL, want)
	}
	if body.Validation == nil {
		t.Error("validation is missing for a completed record")
	} else if !body.Validation.Valid {
		t.Errorf("validation.valid = false, issues = %v", body.Validation.Issues)
	}
	assertNoLeak(t, rec.Body.String())

	// The strongest form of the leak check: the record's own internal paths and
	// extracted text must not appear anywhere in the response.
	record, err := a.repo.GetByID(context.Background(), 1)
	if err != nil {
		t.Fatalf("loading the record: %v", err)
	}
	for name, secret := range map[string]string{
		"resume storage key": record.ResumePath,
		"pdf storage key":    record.PDFPath,
		"resume text":        record.ResumeText,
		"storage root":       a.pdfs.Root(),
	} {
		if secret == "" {
			t.Fatalf("%s is empty, so the leak assertion would be vacuous", name)
		}
		if strings.Contains(rec.Body.String(), secret) {
			t.Errorf("the response leaks the %s %q: %s", name, secret, rec.Body.String())
		}
	}
}

// TestGetFailedCoverLetter asserts a failure is reported with a safe message
// and no outputs.
func TestGetFailedCoverLetter(t *testing.T) {
	t.Parallel()

	a := newAPI(t, nil)
	created := a.submit(map[string]string{"job_description": longJobDescription}, "resume.pdf", resumePDF(t))
	if created.Code != http.StatusAccepted {
		t.Fatalf("create status = %d, want 202 (body: %s)", created.Code, created.Body.String())
	}

	// Drive the record into the failed state the way a worker would.
	record, err := a.repo.GetByID(context.Background(), 1)
	if err != nil {
		t.Fatalf("loading the record: %v", err)
	}
	if err := a.repo.TransitionStatus(context.Background(), record.ID,
		[]coverletter.Status{coverletter.StatusProcessing},
		coverletter.StatusFailed, "Cover letter generation failed."); err != nil {
		t.Fatalf("marking the record failed: %v", err)
	}

	rec := a.get("/api/v1/cover-letters/1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var body struct {
		Status       string `json:"status"`
		CoverLetter  string `json:"cover_letter"`
		PDFURL       string `json:"pdf_url"`
		ErrorMessage string `json:"error_message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the response: %v (body: %s)", err, rec.Body.String())
	}
	if body.Status != string(coverletter.StatusFailed) {
		t.Errorf("status = %q, want %q", body.Status, coverletter.StatusFailed)
	}
	if body.CoverLetter != "" || body.PDFURL != "" {
		t.Errorf("a failed record advertised outputs: letter=%q pdf=%q", body.CoverLetter, body.PDFURL)
	}
	if body.ErrorMessage != "Cover letter generation failed." {
		t.Errorf("error_message = %q, want the client-safe message", body.ErrorMessage)
	}
	assertNoLeak(t, rec.Body.String())
}

func TestDownloadPDF(t *testing.T) {
	t.Parallel()

	t.Run("serves the rendered pdf after completion", func(t *testing.T) {
		t.Parallel()

		a := newAPI(t, nil)
		a.complete(a.create())

		rec := a.get("/api/v1/cover-letters/1/pdf")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Content-Type"); got != "application/pdf" {
			t.Errorf("Content-Type = %q, want application/pdf", got)
		}
		if got := rec.Header().Get("Content-Disposition"); !strings.Contains(got, "cover-letter-1.pdf") {
			t.Errorf("Content-Disposition = %q, want it to name cover-letter-1.pdf", got)
		}
		if !strings.HasPrefix(rec.Body.String(), "%PDF-") {
			t.Errorf("body does not start with the PDF signature: %q", firstBytes(rec.Body.String()))
		}
		if rec.Body.Len() < 500 {
			t.Errorf("only %d bytes served, which is too small to be a rendered letter", rec.Body.Len())
		}
	})

	t.Run("conflicts before completion", func(t *testing.T) {
		t.Parallel()

		a := newAPI(t, nil)
		created := a.submit(map[string]string{"job_description": longJobDescription}, "resume.pdf", resumePDF(t))
		if created.Code != http.StatusAccepted {
			t.Fatalf("create status = %d, want 202 (body: %s)", created.Code, created.Body.String())
		}

		// The status check is what stops a client downloading a half-written file.
		rec := a.get("/api/v1/cover-letters/1/pdf")
		assertErrorEnvelope(t, rec, http.StatusConflict, apperror.CodeInvalidState)
	})

	t.Run("conflicts for a failed record", func(t *testing.T) {
		t.Parallel()

		a := newAPI(t, nil)
		record := &coverletter.CoverLetter{
			JobDescription: longJobDescription,
			Status:         coverletter.StatusProcessing,
		}
		if err := a.repo.Create(context.Background(), record); err != nil {
			t.Fatalf("Create() error = %v", err)
		}
		if err := a.repo.TransitionStatus(context.Background(), record.ID,
			[]coverletter.Status{coverletter.StatusProcessing},
			coverletter.StatusFailed, "generation failed"); err != nil {
			t.Fatalf("TransitionStatus() error = %v", err)
		}

		rec := a.get("/api/v1/cover-letters/1/pdf")
		assertErrorEnvelope(t, rec, http.StatusConflict, apperror.CodeInvalidState)
	})

	t.Run("unknown id", func(t *testing.T) {
		t.Parallel()

		a := newAPI(t, nil)
		rec := a.get("/api/v1/cover-letters/9999/pdf")
		assertErrorEnvelope(t, rec, http.StatusNotFound, apperror.CodeNotFound)
	})

	t.Run("non-numeric id", func(t *testing.T) {
		t.Parallel()

		a := newAPI(t, nil)
		rec := a.get("/api/v1/cover-letters/not-a-number/pdf")
		assertErrorEnvelope(t, rec, http.StatusBadRequest, apperror.CodeValidation)
	})

	// A completed record whose object vanished must produce a server error with
	// a generic message, not the storage key that is missing.
	t.Run("missing object does not leak the storage key", func(t *testing.T) {
		t.Parallel()

		a := newAPI(t, nil)
		a.complete(a.create())

		record, err := a.repo.GetByID(context.Background(), 1)
		if err != nil {
			t.Fatalf("loading the record: %v", err)
		}
		if err := a.pdfs.Delete(context.Background(), record.PDFPath); err != nil {
			t.Fatalf("Delete() error = %v", err)
		}

		rec := a.get("/api/v1/cover-letters/1/pdf")
		assertErrorEnvelope(t, rec, http.StatusInternalServerError, apperror.CodeInternal)
		if strings.Contains(rec.Body.String(), record.PDFPath) {
			t.Errorf("the response leaks the storage key %q: %s", record.PDFPath, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), a.pdfs.Root()) {
			t.Errorf("the response leaks the storage root: %s", rec.Body.String())
		}
	})
}

func TestMethodNotAllowed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		method      string
		path        string
		wantAllowed string
	}{
		{name: "delete a collection", method: http.MethodDelete, path: "/api/v1/cover-letters", wantAllowed: "POST"},
		{name: "put a record", method: http.MethodPut, path: "/api/v1/cover-letters/1", wantAllowed: "GET"},
		{name: "post a record", method: http.MethodPost, path: "/api/v1/cover-letters/1", wantAllowed: "GET"},
		{name: "patch a record", method: http.MethodPatch, path: "/api/v1/cover-letters/1", wantAllowed: "GET"},
		{name: "options a record", method: http.MethodOptions, path: "/api/v1/cover-letters/1", wantAllowed: "GET"},
		{name: "put a pdf", method: http.MethodPut, path: "/api/v1/cover-letters/1/pdf", wantAllowed: "GET"},
		{name: "delete a pdf", method: http.MethodDelete, path: "/api/v1/cover-letters/1/pdf", wantAllowed: "GET"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := newAPI(t, nil)
			rec := httptest.NewRecorder()
			a.router.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))

			// The 405 must use the same envelope as every other failure, not
			// ServeMux's plain text.
			assertErrorEnvelope(t, rec, http.StatusMethodNotAllowed, apperror.CodeMethodNotAllowed)
			if got := rec.Header().Get("Allow"); got != tc.wantAllowed {
				t.Errorf("Allow header = %q, want %q", got, tc.wantAllowed)
			}
		})
	}
}

func TestUnknownPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		method string
		path   string
	}{
		{name: "unrelated path", method: http.MethodGet, path: "/does-not-exist"},
		{name: "root", method: http.MethodGet, path: "/"},
		{name: "extra path segment under a known resource", method: http.MethodGet, path: "/api/v1/cover-letters/1/pdf/extra"},
		{name: "unknown api version", method: http.MethodGet, path: "/api/v2/cover-letters/1"},
		{name: "wrong method on a bare path", method: http.MethodPost, path: "/health"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := newAPI(t, nil)
			rec := httptest.NewRecorder()
			a.router.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))

			// A 404 for a path with no method pattern must still carry the
			// envelope, so a client parses every failure the same way.
			assertErrorEnvelope(t, rec, http.StatusNotFound, apperror.CodeNotFound)
		})
	}
}

// TestRequestIDIsAssignedAndEchoed covers the correlation contract: a client
// can quote a failure to an operator, who finds the matching log line.
func TestRequestIDIsAssignedAndEchoed(t *testing.T) {
	t.Parallel()

	t.Run("generated when absent", func(t *testing.T) {
		t.Parallel()

		a := newAPI(t, nil)
		rec := a.get("/nope")
		envelope := assertErrorEnvelope(t, rec, http.StatusNotFound, apperror.CodeNotFound)
		if strings.TrimSpace(envelope.Error.RequestID) == "" {
			t.Error("no request id was generated")
		}
	})

	t.Run("inbound id is reused", func(t *testing.T) {
		t.Parallel()

		a := newAPI(t, nil)
		req := httptest.NewRequest(http.MethodGet, "/nope", nil)
		req.Header.Set("X-Request-ID", "trace-abc-123")
		rec := httptest.NewRecorder()
		a.router.ServeHTTP(rec, req)

		envelope := assertErrorEnvelope(t, rec, http.StatusNotFound, apperror.CodeNotFound)
		if envelope.Error.RequestID != "trace-abc-123" {
			t.Errorf("request_id = %q, want the inbound id to be reused", envelope.Error.RequestID)
		}
	})

	// An oversized inbound id is replaced rather than echoed, so a client
	// cannot inject an arbitrary string into the logs.
	t.Run("absurdly long inbound id is replaced", func(t *testing.T) {
		t.Parallel()

		a := newAPI(t, nil)
		req := httptest.NewRequest(http.MethodGet, "/nope", nil)
		req.Header.Set("X-Request-ID", strings.Repeat("x", 500))
		rec := httptest.NewRecorder()
		a.router.ServeHTTP(rec, req)

		envelope := assertErrorEnvelope(t, rec, http.StatusNotFound, apperror.CodeNotFound)
		if len(envelope.Error.RequestID) > 128 {
			t.Errorf("request_id is %d characters, want it capped at 128", len(envelope.Error.RequestID))
		}
	})
}

// TestErrorsAreConsistentAcrossEndpoints is the envelope-shape guarantee: a
// client should be able to parse any failure with one code path, so every
// endpoint's failure is decoded into the same struct.
func TestErrorsAreConsistentAcrossEndpoints(t *testing.T) {
	t.Parallel()

	a := newAPI(t, nil)

	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{name: "unknown path", method: http.MethodGet, path: "/nope"},
		{name: "unknown record", method: http.MethodGet, path: "/api/v1/cover-letters/1"},
		{name: "bad id", method: http.MethodGet, path: "/api/v1/cover-letters/xyz"},
		{name: "pdf not ready", method: http.MethodGet, path: "/api/v1/cover-letters/1/pdf"},
		{name: "bad method", method: http.MethodDelete, path: "/api/v1/cover-letters"},
		{name: "malformed create", method: http.MethodPost, path: "/api/v1/cover-letters", body: `not multipart`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			if tc.method == http.MethodPost {
				req.Header.Set("Content-Type", "application/json")
			}
			rec := httptest.NewRecorder()
			a.router.ServeHTTP(rec, req)

			// Decode into the exact type httpx.ErrorBody produces, proving the
			// wire shape is the one the shared renderer defines.
			var body httpx.ErrorBody
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decoding into httpx.ErrorBody: %v (body: %s)", err, rec.Body.String())
			}
			if body.Error.Code == "" {
				t.Error("code is empty")
			}
			if body.Error.Message == "" {
				t.Error("message is empty")
			}
			if body.Error.RequestID == "" {
				t.Error("request_id is empty")
			}
			if rec.Code < 400 {
				t.Errorf("status = %d, want a 4xx or 5xx", rec.Code)
			}
			assertNoLeak(t, rec.Body.String())
		})
	}
}

// TestPanicBecomesInternalError drives the recovery middleware directly: the
// handler chain is fixed by NewRouter, so the only way to prove a panic is
// converted rather than dropped is to wrap a panicking handler.
func TestPanicBecomesInternalError(t *testing.T) {
	t.Parallel()

	handler := recoverPanic()(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom: api_key=sk-live-should-never-be-logged-to-the-client")
	}))

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req = req.WithContext(httpx.WithRequestID(req.Context(), "panic-test"))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (body: %s)", rec.Code, rec.Body.String())
	}
	var envelope errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decoding the error envelope: %v (body: %s)", err, rec.Body.String())
	}
	if envelope.Error.Code != apperror.CodeInternal {
		t.Errorf("code = %q, want %q", envelope.Error.Code, apperror.CodeInternal)
	}
	if envelope.Error.Message != "An unexpected error occurred." {
		t.Errorf("message = %q, want the generic message", envelope.Error.Message)
	}
	// The panic value carries the secret; none of it may reach the client.
	assertNoLeak(t, rec.Body.String())
	if strings.Contains(rec.Body.String(), "boom") {
		t.Errorf("the panic value leaked into the response: %s", rec.Body.String())
	}
}

// TestStatusRecorderReportsTheFinalStatus guards the middleware's bookkeeping:
// metrics and the access log both read the status from this recorder, so a
// recorder that reported the wrong code would corrupt both.
func TestStatusRecorderReportsTheFinalStatus(t *testing.T) {
	t.Parallel()

	t.Run("defaults to 200 when nothing is written", func(t *testing.T) {
		t.Parallel()

		rec := &statusRecorder{ResponseWriter: httptest.NewRecorder(), status: http.StatusOK}
		if rec.status != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.status)
		}
		if rec.written != 0 {
			t.Errorf("written = %d, want 0", rec.written)
		}
	})

	t.Run("the first WriteHeader wins", func(t *testing.T) {
		t.Parallel()

		rec := &statusRecorder{ResponseWriter: httptest.NewRecorder(), status: http.StatusOK}
		rec.WriteHeader(http.StatusCreated)
		rec.WriteHeader(http.StatusTeapot)
		if rec.status != http.StatusCreated {
			t.Errorf("status = %d, want the first call to win (%d)", rec.status, http.StatusCreated)
		}
	})

	t.Run("an implicit Write counts as 200", func(t *testing.T) {
		t.Parallel()

		rec := &statusRecorder{ResponseWriter: httptest.NewRecorder(), status: http.StatusOK}
		if _, err := rec.Write([]byte("hello")); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
		if rec.status != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.status)
		}
		if rec.written != 5 {
			t.Errorf("written = %d, want 5", rec.written)
		}
	})
}

func TestStatusMetricName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		method string
		status int
		want   string
	}{
		{method: http.MethodGet, status: 200, want: "http.requests.total.get.status_200"},
		{method: http.MethodPost, status: 202, want: "http.requests.total.post.status_202"},
		{method: http.MethodDelete, status: 405, want: "http.requests.total.delete.status_405"},
		// The method is lower-cased so the name set stays bounded; an unbounded
		// label would be a cardinality problem in a real registry.
		{method: "GET", status: 500, want: "http.requests.total.get.status_500"},
	}

	for _, tc := range tests {
		if got := statusMetricName(tc.method, tc.status); got != tc.want {
			t.Errorf("statusMetricName(%q, %d) = %q, want %q", tc.method, tc.status, got, tc.want)
		}
	}
}

// TestClientIPIgnoresForwardedHeaders guards the logging decision: a forged
// X-Forwarded-For must not be able to poison the access log.
func TestClientIPIgnoresForwardedHeaders(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.RemoteAddr = "10.1.2.3:54321"
	req.Header.Set("X-Forwarded-For", "203.0.113.9")

	if got := clientIP(req); got != "10.1.2.3" {
		t.Errorf("clientIP() = %q, want the peer address %q", got, "10.1.2.3")
	}
}

func TestChainAppliesOutermostFirst(t *testing.T) {
	t.Parallel()

	// The first middleware listed must be the outermost, otherwise the recovery
	// middleware could be applied inside a logger that would then be skipped on
	// a panic.
	var order []string
	mw := func(name string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}

	handler := chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		mw("outer"), mw("middle"), mw("inner"))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	want := []string{"outer", "middle", "inner"}
	if len(order) != len(want) {
		t.Fatalf("middleware ran %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("middleware %d ran %q, want %q", i, order[i], want[i])
		}
	}
}

// TestServerSideRequestsUseTheSameHandler drives the router over a real
// connection rather than a recorder, which is the only way to confirm the
// wrapped handler is usable as a server handler.
func TestServerSideRequestsUseTheSameHandler(t *testing.T) {
	t.Parallel()

	a := newAPI(t, nil)
	server := httptest.NewServer(a.router)
	t.Cleanup(server.Close)
	a.server = server

	resp, err := http.Get(server.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the body: %v", err)
	}
	if string(body) != `{"status":"ok"}` {
		t.Errorf("body = %q, want %q", body, `{"status":"ok"}`)
	}
	if resp.Header.Get("X-Request-ID") == "" {
		t.Error("no X-Request-ID header over a real connection")
	}

	// A failed request over the wire must still be a clean JSON envelope.
	bad, err := http.Get(server.URL + "/api/v1/cover-letters/9999")
	if err != nil {
		t.Fatalf("GET an unknown record: %v", err)
	}
	defer func() { _ = bad.Body.Close() }()
	if bad.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", bad.StatusCode)
	}
	var envelope errorEnvelope
	if err := json.NewDecoder(bad.Body).Decode(&envelope); err != nil {
		t.Fatalf("decoding the error envelope: %v", err)
	}
	if envelope.Error.Code != apperror.CodeNotFound {
		t.Errorf("code = %q, want %q", envelope.Error.Code, apperror.CodeNotFound)
	}
}

// itoa avoids importing strconv for a single path segment.
func itoa(v uint) string { return strconv.FormatUint(uint64(v), 10) }

// firstBytes shortens a body for a failure message.
func firstBytes(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[:8]
}

// Compile-time assertions that the test fakes satisfy the production ports.
var (
	_ coverletter.Dispatcher = (*dispatcher)(nil)
	_ *slog.Logger
)
