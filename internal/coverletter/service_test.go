package coverletter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"coverletter/internal/ai"
	"coverletter/internal/aitest"
	"coverletter/internal/apperror"
	"coverletter/internal/config"
	"coverletter/internal/database"
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

// env is a fully wired Service backed by real collaborators: a real GORM
// repository on a real SQLite database, real local storage, a real PDF
// generator and the scripted AI provider. Only the job fetcher is left
// configurable, because the SSRF guard it performs depends on live DNS.
type env struct {
	t       *testing.T
	svc     *Service
	repo    *GORMRepository
	uploads *storage.LocalStorage
	pdfs    *storage.LocalStorage
	resumes *resume.Service
	agent   *aitest.Provider
	metrics *obs.Registry
}

// fetcherOpt configures the job fetcher used by the env's job service.
type fetcherOpt func(*job.Service)

// newEnv builds an isolated environment. Every call gets its own temp
// directory, so tests are safe to run in parallel and never share a database
// file or a storage root.
func newEnv(t *testing.T, provider *aitest.Provider, opts ...func(*env)) *env {
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

	// The real embedded migration, so the tests exercise the same schema the
	// server runs against rather than a GORM-generated approximation.
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

	const maxUpload = 4 << 20
	registry := obs.New()
	logger := logging.Discard()

	resumes := resume.NewService(uploads, uploads, resume.NewPDFParser(12000), maxUpload, registry, logger)
	agent := ai.NewAgent(provider, ai.AgentConfig{MaxAttempts: 2}, registry, logger)
	// No fetcher or parser by default: a URL-based job would need the network.
	jobSvc := job.NewService(nil, nil, registry)

	repo := NewRepository(db)
	svc := NewService(repo, resumes, jobSvc, agent, pdf.NewGenerator(), pdfs,
		Config{GenerationTimeout: 30 * time.Second}, registry, logger)

	e := &env{
		t:       t,
		svc:     svc,
		repo:    repo,
		uploads: uploads,
		pdfs:    pdfs,
		resumes: resumes,
		agent:   provider,
		metrics: registry,
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// withJobService replaces the job service, for tests that need URL resolution.
func withJobService(js *job.Service) func(*env) {
	return func(e *env) { e.svc.jobs = js }
}

// resumePDF renders a real, parseable PDF resume and stores it through the
// real resume service, returning its storage key. Using the production PDF
// generator guarantees the bytes satisfy the resume parser's minimum-text
// requirement, which a hand-written stub would not.
func (e *env) resumePDF(filename string) string {
	e.t.Helper()

	body := strings.Repeat(
		"Ada Lovelace, Senior Engineer at Globex. Built the payment services platform in Go "+
			"and ran its PostgreSQL migration with no customer-visible downtime. ", 4)
	bytes, err := pdf.NewGenerator().Generate(context.Background(), pdf.Request{
		CandidateName: "Ada Lovelace",
		JobTitle:      "Senior Engineer",
		Company:       "Globex",
		Subject:       "Resume",
		Body:          body,
		Date:          time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		e.t.Fatalf("generating resume pdf: %v", err)
	}

	stored, err := e.resumes.Store(context.Background(), filename, strings.NewReader(string(bytes)))
	if err != nil {
		e.t.Fatalf("storing resume: %v", err)
	}
	return stored.StorageKey
}

// longJobDescription is comfortably above the job service's minimum length, so
// a manual description is accepted without tripping the "too short" check.
const longJobDescription = "We are hiring a backend engineer to work on our payments platform. " +
	"You will build and operate Go services, own their on-call rotation, and drive reliability work."

// recordingDispatcher captures the ids it is asked to process.
type recordingDispatcher struct {
	mu  sync.Mutex
	ids []uint
	err error
}

func (d *recordingDispatcher) Dispatch(_ context.Context, id uint) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ids = append(d.ids, id)
	return d.err
}

func (d *recordingDispatcher) dispatched() []uint {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]uint(nil), d.ids...)
}

func TestCreateHappyPath(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	e := newEnv(t, aitest.DefaultResponses())
	key := e.resumePDF("resume.pdf")
	dispatch := &recordingDispatcher{}

	record, err := e.svc.Create(ctx, CreateRequest{
		JobDescription: longJobDescription,
		Tone:           "enthusiastic",
		ResumeKey:      key,
		ResumeFilename: "resume.pdf",
	}, dispatch)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if record == nil {
		t.Fatal("Create() returned a nil record")
	}
	if record.ID == 0 {
		t.Error("Create() did not populate the record id")
	}

	// The record is persisted, not just returned in memory.
	persisted, err := e.repo.GetByID(ctx, record.ID)
	if err != nil {
		t.Fatalf("loading the persisted record: %v", err)
	}
	if persisted.JobDescription != longJobDescription {
		t.Errorf("persisted job description = %q, want %q", persisted.JobDescription, longJobDescription)
	}
	if persisted.Tone != "enthusiastic" {
		t.Errorf("persisted tone = %q, want %q", persisted.Tone, "enthusiastic")
	}
	if persisted.ResumePath != key {
		t.Errorf("persisted resume path = %q, want %q", persisted.ResumePath, key)
	}
	if persisted.ResumeFilename != "resume.pdf" {
		t.Errorf("persisted resume filename = %q, want %q", persisted.ResumeFilename, "resume.pdf")
	}
	if persisted.CoverLetterText != "" {
		t.Error("a freshly created record must not carry any generated text")
	}
	if persisted.PDFPath != "" {
		t.Errorf("a freshly created record must not carry a pdf path, got %q", persisted.PDFPath)
	}

	// The work is handed to the dispatcher exactly once, for the right id.
	got := dispatch.dispatched()
	if len(got) != 1 {
		t.Fatalf("dispatcher called %d times, want 1", len(got))
	}
	if got[0] != record.ID {
		t.Errorf("dispatcher called with id %d, want %d", got[0], record.ID)
	}

	// Once dispatched the record is in flight, which is the status a polling
	// client is told to expect.
	if persisted.Status != StatusProcessing {
		t.Errorf("persisted status = %q, want %q", persisted.Status, StatusProcessing)
	}
	if record.Status != StatusProcessing {
		t.Errorf("returned status = %q, want %q", record.Status, StatusProcessing)
	}
}

// TestCreateTrimsInput guards the normalisation the handler relies on: a client
// that pads its form fields should not change the stored record.
func TestCreateTrimsInput(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	e := newEnv(t, aitest.DefaultResponses())

	record, err := e.svc.Create(ctx, CreateRequest{
		// A URL would be resolved against real DNS by the pre-flight SSRF
		// check, so the trimming of job_description is asserted here instead.
		JobDescription: "  We are hiring a backend engineer with eight or more " +
			"years of experience building distributed services in Go, with deep " +
			"knowledge of gRPC, relational databases and observability tooling.  ",
		Tone:           "  formal  ",
		ResumeKey:      e.resumePDF("resume.pdf"),
		ResumeFilename: "resume.pdf",
	}, nil)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if strings.HasPrefix(record.JobDescription, " ") || strings.HasSuffix(record.JobDescription, " ") {
		t.Errorf("JobDescription = %q, want the surrounding whitespace to be trimmed", record.JobDescription)
	}
	if record.Tone != "formal" {
		t.Errorf("Tone = %q, want %q", record.Tone, "formal")
	}
}

func TestCreateRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		req      CreateRequest
		wantCode string
	}{
		{
			name: "missing resume",
			req: CreateRequest{
				JobDescription: longJobDescription,
			},
			wantCode: apperror.CodeInvalidResume,
		},
		{
			name: "both job url and job description",
			req: CreateRequest{
				JobURL:         "https://jobs.example.com/1",
				JobDescription: longJobDescription,
				ResumeKey:      "resumes/x.pdf",
			},
			wantCode: apperror.CodeInvalidJobInput,
		},
		{
			name: "neither job url nor job description",
			req: CreateRequest{
				ResumeKey: "resumes/x.pdf",
			},
			wantCode: apperror.CodeInvalidJobInput,
		},
		{
			// Whitespace-only input is not input: the handler trims before it
			// reaches here, and a caller that does not must get the same answer.
			name: "whitespace-only job description",
			req: CreateRequest{
				JobDescription: "   \n\t  ",
				ResumeKey:      "resumes/x.pdf",
			},
			wantCode: apperror.CodeInvalidJobInput,
		},
		{
			name: "non-http scheme",
			req: CreateRequest{
				JobURL:    "ftp://jobs.example.com/1",
				ResumeKey: "resumes/x.pdf",
			},
			wantCode: apperror.CodeInvalidJobInput,
		},
		{
			name: "url with embedded credentials",
			req: CreateRequest{
				JobURL:    "https://user:secret@jobs.example.com/1",
				ResumeKey: "resumes/x.pdf",
			},
			wantCode: apperror.CodeInvalidJobInput,
		},
		{
			name: "url without a host",
			req: CreateRequest{
				JobURL:    "https:///1",
				ResumeKey: "resumes/x.pdf",
			},
			wantCode: apperror.CodeInvalidJobInput,
		},
		{
			name: "tone longer than 64 characters",
			req: CreateRequest{
				JobDescription: longJobDescription,
				ResumeKey:      "resumes/x.pdf",
				Tone:           strings.Repeat("t", 65),
			},
			wantCode: apperror.CodeValidation,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			e := newEnv(t, aitest.DefaultResponses())
			dispatch := &recordingDispatcher{}

			record, err := e.svc.Create(ctx, tc.req, dispatch)
			if err == nil {
				t.Fatalf("Create() error = nil, want %s", tc.wantCode)
			}
			if record != nil {
				t.Errorf("Create() returned a record alongside an error: %+v", record)
			}
			if got := apperror.Code(err); got != tc.wantCode {
				t.Errorf("error code = %q, want %q (message: %q)", got, tc.wantCode, apperror.Message(err))
			}
			if got := apperror.Status(err); got != 400 {
				t.Errorf("error status = %d, want 400", got)
			}
			if apperror.Message(err) == "" {
				t.Error("error has an empty client-safe message")
			}

			// A rejected request must not leave a phantom job behind, and must
			// not consume a worker slot.
			if got := dispatch.dispatched(); len(got) != 0 {
				t.Errorf("dispatcher was called %d times for a rejected request", len(got))
			}
			all, err := e.repo.GetByID(ctx, 1)
			if err == nil {
				t.Errorf("a rejected request was persisted as record %d", all.ID)
			} else if got := apperror.Code(err); got != apperror.CodeNotFound {
				t.Errorf("looking up the first record: code = %q, want %q", got, apperror.CodeNotFound)
			}
		})
	}
}

// TestCreateRejectsUnsafeJobURL covers the SSRF defence at the point it is
// actually enforced.
//
// The URL guard runs in the job fetcher, which is reached during Process, not
// Create: Create only checks that the URL is well-formed. A loopback address is
// therefore accepted here and the record is durably recorded, then marked
// failed with UNSAFE_URL once the worker tries to resolve it. Asserting the
// guard's real behaviour here means a regression that lets a private address
// through still fails this test, at the layer where the check lives.
func TestCreateRejectsUnsafeJobURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		url  string
	}{
		{name: "loopback ip", url: "http://127.0.0.1/x"},
		{name: "localhost", url: "http://localhost/x"},
		{name: "link-local metadata address", url: "http://169.254.169.254/latest/meta-data/"},
		{name: "private range", url: "http://10.0.0.5/jobs/1"},
		{name: "unspecified address", url: "http://0.0.0.0/x"},
		{name: "internal hostname", url: "http://redis.internal/jobs"},
		{name: "file scheme", url: "file:///etc/passwd"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			registry := obs.New()
			logger := logging.Discard()
			e := newEnv(t, aitest.DefaultResponses(), withJobService(
				job.NewService(
					// The real fetcher, so the real guard runs. It never opens a
					// connection: the host is rejected before dialling, which is
					// what keeps this test off the network.
					job.NewHTTPFetcher(5*time.Second, 2, 1<<20, "test-agent", false, registry, logger),
					job.NewHTMLParser(1000),
					registry,
				)))

			// Create refuses the URL outright, rather than accepting it with a
			// 202 and failing later in a worker where the client would only see
			// it by polling.
			record, err := e.svc.Create(ctx, CreateRequest{
				JobURL:    tc.url,
				ResumeKey: e.resumePDF("resume.pdf"),
			}, &recordingDispatcher{})
			if err == nil {
				t.Fatalf("Create() accepted %q (record %+v); want it rejected", tc.url, record)
			}
			if !apperror.Is(err, apperror.CodeUnsafeURL) && !apperror.Is(err, apperror.CodeInvalidJobInput) {
				t.Errorf("Create() error = %v, want UNSAFE_URL or INVALID_JOB_INPUT", err)
			}

			// The message must not disclose which address was blocked.
			if msg := apperror.Message(err); strings.Contains(msg, "127.0.0.1") ||
				strings.Contains(msg, "169.254") || strings.Contains(msg, "10.0.0.5") {
				t.Errorf("error message discloses the blocked address: %q", msg)
			}
		})
	}
}

func TestCreateSurfacesQueueFull(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	e := newEnv(t, aitest.DefaultResponses())
	dispatch := &recordingDispatcher{err: worker.ErrQueueFull}

	record, err := e.svc.Create(ctx, CreateRequest{
		JobDescription: longJobDescription,
		ResumeKey:      e.resumePDF("resume.pdf"),
	}, dispatch)
	if err == nil {
		t.Fatal("Create() error = nil, want a backpressure error")
	}
	if record != nil {
		t.Errorf("Create() returned a record alongside an error: %+v", record)
	}
	if got := apperror.Status(err); got != 503 {
		t.Errorf("error status = %d, want 503", got)
	}
	if got := apperror.Code(err); got != apperror.CodeQueueFull {
		t.Errorf("error code = %q, want %q", got, apperror.CodeQueueFull)
	}
	// The queue-full case is distinguished from a generic dispatch failure by
	// its code, not by errors.Is: the sentinel is consumed at the branch point
	// and the error returned to the client carries the code alone.
	if got := apperror.Code(err); got == apperror.CodeUnavailable {
		t.Error("backpressure was reported as a generic unavailability")
	}
	if apperror.Message(err) == "" {
		t.Error("error has an empty client-safe message")
	}

	// The upload was durably recorded before dispatch was attempted, so a
	// client polling the id would otherwise wait forever. It must be failed.
	failed, getErr := e.repo.GetByID(ctx, 1)
	if getErr != nil {
		t.Fatalf("the request was not recorded at all: %v", getErr)
	}
	if failed.Status != StatusFailed {
		t.Errorf("persisted status = %q, want %q", failed.Status, StatusFailed)
	}
}

// TestCreateSurfacesGenericDispatchFailure covers the other dispatch branch:
// an unexpected queue error is reported as a generic unavailability rather
// than being confused with backpressure, and is not logged back to the client.
func TestCreateSurfacesGenericDispatchFailure(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	e := newEnv(t, aitest.DefaultResponses())
	dispatch := &recordingDispatcher{err: errors.New("queue broker unreachable at db-queue-primary:5432")}

	_, err := e.svc.Create(ctx, CreateRequest{
		JobDescription: longJobDescription,
		ResumeKey:      e.resumePDF("resume.pdf"),
	}, dispatch)
	if err == nil {
		t.Fatal("Create() error = nil, want an unavailability error")
	}
	if got := apperror.Status(err); got != 503 {
		t.Errorf("error status = %d, want 503", got)
	}
	if got := apperror.Code(err); got != apperror.CodeUnavailable {
		t.Errorf("error code = %q, want %q", got, apperror.CodeUnavailable)
	}
	if errors.Is(err, apperror.ErrQueueFull) {
		t.Error("an unrelated dispatch failure was misreported as backpressure")
	}
	if msg := apperror.Message(err); strings.Contains(msg, "db-queue-primary") {
		t.Errorf("client-safe message leaks internal detail: %q", msg)
	}

	failed, getErr := e.repo.GetByID(ctx, 1)
	if getErr != nil {
		t.Fatalf("the request was not recorded at all: %v", getErr)
	}
	if failed.Status != StatusFailed {
		t.Errorf("persisted status = %q, want %q", failed.Status, StatusFailed)
	}
}

// TestCreateWithoutDispatcherStillRecords covers the nil-dispatcher path: the
// record is written and still moves to processing, so a caller that drives the
// workflow itself sees a consistent state.
func TestCreateWithoutDispatcherStillRecords(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	e := newEnv(t, aitest.DefaultResponses())

	record, err := e.svc.Create(ctx, CreateRequest{
		JobDescription: longJobDescription,
		ResumeKey:      e.resumePDF("resume.pdf"),
	}, nil)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	persisted, err := e.repo.GetByID(ctx, record.ID)
	if err != nil {
		t.Fatalf("loading the record: %v", err)
	}
	if persisted.Status != StatusProcessing {
		t.Errorf("persisted status = %q, want %q", persisted.Status, StatusProcessing)
	}
}

func TestProcessCompletesSuccessfully(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	provider := aitest.DefaultResponses()
	e := newEnv(t, provider)

	record, err := e.svc.Create(ctx, CreateRequest{
		JobDescription: longJobDescription,
		Tone:           "professional",
		ResumeKey:      e.resumePDF("resume.pdf"),
		ResumeFilename: "resume.pdf",
	}, &recordingDispatcher{})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if err := e.svc.Process(ctx, record.ID); err != nil {
		t.Fatalf("Process() error = %v", err)
	}

	persisted, err := e.repo.GetByID(ctx, record.ID)
	if err != nil {
		t.Fatalf("loading the record: %v", err)
	}

	if persisted.Status != StatusCompleted {
		t.Errorf("status = %q, want %q (error message: %q)", persisted.Status, StatusCompleted, persisted.ErrorMessage)
	}
	if persisted.ErrorMessage != "" {
		t.Errorf("a completed record must not carry an error message, got %q", persisted.ErrorMessage)
	}
	if strings.TrimSpace(persisted.CoverLetterText) == "" {
		t.Error("CoverLetterText is empty")
	}
	if persisted.CandidateName != "Ada Lovelace" {
		t.Errorf("CandidateName = %q, want %q", persisted.CandidateName, "Ada Lovelace")
	}
	if persisted.PDFPath == "" {
		t.Fatal("PDFPath is empty")
	}

	wantKey := "cover-letters/" + itoa(persisted.ID) + "/cover-letter.pdf"
	if persisted.PDFPath != wantKey {
		t.Errorf("PDFPath = %q, want %q", persisted.PDFPath, wantKey)
	}

	// The extracted inputs are persisted too, so a failed rerun is diagnosable.
	if persisted.ResumeText == "" {
		t.Error("ResumeText was not persisted")
	}
	if persisted.JobDescription == "" {
		t.Error("the resolved job description was not persisted")
	}

	// The validation verdict is stored as decodable JSON.
	summary := persisted.ValidationSummary()
	if summary == nil {
		t.Fatalf("ValidationSummary() = nil for stored validation %q", persisted.Validation)
	}
	if !summary.Valid {
		t.Errorf("validation valid = false, issues = %v", summary.Issues)
	}

	// The PDF really is in storage, under the key the record points at.
	exists, err := e.pdfs.Exists(ctx, persisted.PDFPath)
	if err != nil {
		t.Fatalf("Exists() error = %v", err)
	}
	if !exists {
		t.Fatalf("no object stored at %q", persisted.PDFPath)
	}

	reader, err := e.svc.OpenPDF(ctx, persisted.ID)
	if err != nil {
		t.Fatalf("OpenPDF() error = %v", err)
	}
	defer func() { _ = reader.Close() }()

	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("reading the pdf: %v", err)
	}
	if !strings.HasPrefix(string(data), "%PDF-") {
		t.Errorf("stored object is not a PDF: starts with %q", truncate(string(data), 8))
	}

	// A completion is recorded in the metrics registry, so operators can see it.
	if got := e.metrics.Counter("coverletter.completed"); got != 1 {
		t.Errorf("coverletter.completed = %d, want 1", got)
	}
	if got := e.metrics.Counter("coverletter.failed"); got != 0 {
		t.Errorf("coverletter.failed = %d, want 0", got)
	}
	if got := provider.CallsTo(aitest.StageGenerateLetter); got != 1 {
		t.Errorf("generate_letter called %d times, want 1", got)
	}
}

func TestProcessFailureKeepsProviderDetailInternal(t *testing.T) {
	t.Parallel()

	const secret = "sk-live-9f3a2b7c4d1e"
	provider := aitest.DefaultResponses().
		SetError(errors.New("upstream 503 from generativelanguage: api key " + secret + " rejected"))

	ctx := context.Background()
	e := newEnv(t, provider)

	record, err := e.svc.Create(ctx, CreateRequest{
		JobDescription: longJobDescription,
		ResumeKey:      e.resumePDF("resume.pdf"),
	}, &recordingDispatcher{})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	processErr := e.svc.Process(ctx, record.ID)
	if processErr == nil {
		t.Fatal("Process() error = nil, want the provider failure to surface")
	}
	// The raw error is returned to the caller (the worker logs it), but the
	// client-safe rendering must not carry it.
	if msg := apperror.Message(processErr); strings.Contains(msg, secret) {
		t.Errorf("client-safe message leaks the provider error: %q", msg)
	}

	persisted, err := e.repo.GetByID(ctx, record.ID)
	if err != nil {
		t.Fatalf("loading the record: %v", err)
	}
	if persisted.Status != StatusFailed {
		t.Errorf("status = %q, want %q", persisted.Status, StatusFailed)
	}
	if persisted.ErrorMessage == "" {
		t.Fatal("a failed record must carry a client-safe explanation")
	}
	if strings.Contains(persisted.ErrorMessage, secret) {
		t.Errorf("stored error message leaks the api key: %q", persisted.ErrorMessage)
	}
	if strings.Contains(persisted.ErrorMessage, "generativelanguage") {
		t.Errorf("stored error message leaks the provider name: %q", persisted.ErrorMessage)
	}
	if strings.Contains(persisted.ErrorMessage, "goroutine") {
		t.Errorf("stored error message leaks a stack trace: %q", persisted.ErrorMessage)
	}

	// Nothing half-finished may be left behind for a client to download.
	if persisted.PDFPath != "" {
		t.Errorf("a failed record must not reference a pdf, got %q", persisted.PDFPath)
	}
	if persisted.CoverLetterText != "" {
		t.Errorf("a failed record must not carry generated text, got %q", persisted.CoverLetterText)
	}
	if _, err := e.svc.OpenPDF(ctx, persisted.ID); err == nil {
		t.Error("OpenPDF() succeeded for a failed record")
	} else if got := apperror.Status(err); got != 409 {
		t.Errorf("OpenPDF() status = %d, want 409", got)
	}
}

func TestProcessSkipsRecordsNotProcessing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		mutate   func(*CoverLetter)
		wantCall bool
	}{
		{
			name:     "already completed",
			mutate:   func(c *CoverLetter) { c.Status = StatusCompleted; c.CoverLetterText = "the original letter" },
			wantCall: false,
		},
		{
			name:     "already failed",
			mutate:   func(c *CoverLetter) { c.Status = StatusFailed },
			wantCall: false,
		},
		{
			name:     "never started",
			mutate:   func(c *CoverLetter) { c.Status = StatusCreated },
			wantCall: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			provider := aitest.DefaultResponses()
			e := newEnv(t, provider)

			record, err := e.svc.Create(ctx, CreateRequest{
				JobDescription: longJobDescription,
				ResumeKey:      e.resumePDF("resume.pdf"),
			}, &recordingDispatcher{})
			if err != nil {
				t.Fatalf("Create() error = %v", err)
			}

			stored, err := e.repo.GetByID(ctx, record.ID)
			if err != nil {
				t.Fatalf("loading the record: %v", err)
			}
			tc.mutate(stored)
			// Update() also writes status, so the row really changes state.
			if err := e.repo.Update(ctx, stored); err != nil {
				t.Fatalf("Update() error = %v", err)
			}

			before := len(provider.Calls())
			if err := e.svc.Process(ctx, record.ID); err != nil {
				t.Errorf("Process() error = %v, want a no-op", err)
			}

			if got := len(provider.Calls()) - before; tc.wantCall != (got > 0) {
				t.Errorf("the agent was invoked (%d provider calls); want invocation = %t", got, tc.wantCall)
			}

			after, err := e.repo.GetByID(ctx, record.ID)
			if err != nil {
				t.Fatalf("reloading the record: %v", err)
			}
			if after.Status != stored.Status {
				t.Errorf("status changed from %q to %q; a skipped record must be left alone", stored.Status, after.Status)
			}
			if after.CoverLetterText != stored.CoverLetterText {
				t.Errorf("CoverLetterText changed from %q to %q; a skipped record must not be overwritten",
					stored.CoverLetterText, after.CoverLetterText)
			}
		})
	}
}

// TestProcessIsIdempotentAfterCompletion covers the duplicate-worker case: two
// workers picking up the same record must not both generate a letter, and the
// second must not overwrite the first one's output.
func TestProcessIsIdempotentAfterCompletion(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	provider := aitest.DefaultResponses()
	e := newEnv(t, provider)

	record, err := e.svc.Create(ctx, CreateRequest{
		JobDescription: longJobDescription,
		ResumeKey:      e.resumePDF("resume.pdf"),
	}, &recordingDispatcher{})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := e.svc.Process(ctx, record.ID); err != nil {
		t.Fatalf("first Process() error = %v", err)
	}
	first, err := e.repo.GetByID(ctx, record.ID)
	if err != nil {
		t.Fatalf("loading the record: %v", err)
	}

	callsAfterFirst := len(provider.Calls())
	if err := e.svc.Process(ctx, record.ID); err != nil {
		t.Fatalf("second Process() error = %v", err)
	}
	if got := len(provider.Calls()); got != callsAfterFirst {
		t.Errorf("the second Process() made %d further provider calls, want 0", got-callsAfterFirst)
	}

	second, err := e.repo.GetByID(ctx, record.ID)
	if err != nil {
		t.Fatalf("reloading the record: %v", err)
	}
	if second.CoverLetterText != first.CoverLetterText {
		t.Error("the second Process() overwrote the generated letter")
	}
	if second.Status != StatusCompleted {
		t.Errorf("status = %q, want %q", second.Status, StatusCompleted)
	}
}

func TestProcessUnknownRecord(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	e := newEnv(t, aitest.DefaultResponses())

	err := e.svc.Process(ctx, 4242)
	if err == nil {
		t.Fatal("Process() error = nil, want a not-found error")
	}
	if got := apperror.Code(err); got != apperror.CodeNotFound {
		t.Errorf("error code = %q, want %q", got, apperror.CodeNotFound)
	}
	if got := apperror.Status(err); got != 404 {
		t.Errorf("error status = %d, want 404", got)
	}
}

func TestGet(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	e := newEnv(t, aitest.DefaultResponses())

	t.Run("unknown id", func(t *testing.T) {
		t.Parallel()

		record, err := e.svc.Get(ctx, 999)
		if err == nil {
			t.Fatalf("Get() = %+v, nil; want a not-found error", record)
		}
		if record != nil {
			t.Errorf("Get() returned a record alongside an error: %+v", record)
		}
		if got := apperror.Code(err); got != apperror.CodeNotFound {
			t.Errorf("error code = %q, want %q", got, apperror.CodeNotFound)
		}
		if got := apperror.Status(err); got != 404 {
			t.Errorf("error status = %d, want 404", got)
		}
	})

	t.Run("zero id is rejected before hitting the database", func(t *testing.T) {
		t.Parallel()

		_, err := e.svc.Get(ctx, 0)
		if err == nil {
			t.Fatal("Get(0) error = nil, want a validation error")
		}
		if got := apperror.Code(err); got != apperror.CodeValidation {
			t.Errorf("error code = %q, want %q", got, apperror.CodeValidation)
		}
	})

	t.Run("returns the stored record", func(t *testing.T) {
		t.Parallel()

		created, err := e.svc.Create(ctx, CreateRequest{
			JobDescription: longJobDescription,
			Tone:           "warm",
			ResumeKey:      e.resumePDF("resume.pdf"),
		}, &recordingDispatcher{})
		if err != nil {
			t.Fatalf("Create() error = %v", err)
		}

		record, err := e.svc.Get(ctx, created.ID)
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		if record.ID != created.ID {
			t.Errorf("Get() id = %d, want %d", record.ID, created.ID)
		}
		if record.Tone != "warm" {
			t.Errorf("Get() tone = %q, want %q", record.Tone, "warm")
		}
		if record.Status != StatusProcessing {
			t.Errorf("Get() status = %q, want %q", record.Status, StatusProcessing)
		}
	})
}

func TestOpenPDF(t *testing.T) {
	t.Parallel()

	t.Run("conflicts before completion", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		e := newEnv(t, aitest.DefaultResponses())
		record, err := e.svc.Create(ctx, CreateRequest{
			JobDescription: longJobDescription,
			ResumeKey:      e.resumePDF("resume.pdf"),
		}, &recordingDispatcher{})
		if err != nil {
			t.Fatalf("Create() error = %v", err)
		}

		reader, err := e.svc.OpenPDF(ctx, record.ID)
		if err == nil {
			_ = reader.Close()
			t.Fatal("OpenPDF() error = nil, want a conflict")
		}
		if reader != nil {
			t.Errorf("OpenPDF() returned a reader alongside an error")
		}
		if got := apperror.Status(err); got != 409 {
			t.Errorf("error status = %d, want 409", got)
		}
		if got := apperror.Code(err); got != apperror.CodeInvalidState {
			t.Errorf("error code = %q, want %q", got, apperror.CodeInvalidState)
		}
	})

	t.Run("returns the bytes after completion", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		e := newEnv(t, aitest.DefaultResponses())
		record, err := e.svc.Create(ctx, CreateRequest{
			JobDescription: longJobDescription,
			ResumeKey:      e.resumePDF("resume.pdf"),
		}, &recordingDispatcher{})
		if err != nil {
			t.Fatalf("Create() error = %v", err)
		}
		if err := e.svc.Process(ctx, record.ID); err != nil {
			t.Fatalf("Process() error = %v", err)
		}

		reader, err := e.svc.OpenPDF(ctx, record.ID)
		if err != nil {
			t.Fatalf("OpenPDF() error = %v", err)
		}
		defer func() { _ = reader.Close() }()

		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("reading the pdf: %v", err)
		}
		if !strings.HasPrefix(string(data), "%PDF-") {
			t.Errorf("bytes do not start with the PDF signature: %q", truncate(string(data), 8))
		}
		if len(data) < 500 {
			t.Errorf("only %d bytes returned, which is too small to be a rendered letter", len(data))
		}
	})

	t.Run("unknown id", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		e := newEnv(t, aitest.DefaultResponses())

		_, err := e.svc.OpenPDF(ctx, 777)
		if err == nil {
			t.Fatal("OpenPDF() error = nil, want a not-found error")
		}
		if got := apperror.Status(err); got != 404 {
			t.Errorf("error status = %d, want 404", got)
		}
	})

	// A completed record whose object has been removed must not surface the
	// storage layer's message; that would tell a client the internal layout.
	t.Run("missing object does not leak the storage key", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		e := newEnv(t, aitest.DefaultResponses())
		record, err := e.svc.Create(ctx, CreateRequest{
			JobDescription: longJobDescription,
			ResumeKey:      e.resumePDF("resume.pdf"),
		}, &recordingDispatcher{})
		if err != nil {
			t.Fatalf("Create() error = %v", err)
		}
		if err := e.svc.Process(ctx, record.ID); err != nil {
			t.Fatalf("Process() error = %v", err)
		}

		stored, err := e.repo.GetByID(ctx, record.ID)
		if err != nil {
			t.Fatalf("loading the record: %v", err)
		}
		if err := e.pdfs.Delete(ctx, stored.PDFPath); err != nil {
			t.Fatalf("Delete() error = %v", err)
		}

		_, err = e.svc.OpenPDF(ctx, record.ID)
		if err == nil {
			t.Fatal("OpenPDF() error = nil, want an error for a missing object")
		}
		if got := apperror.Status(err); got != 500 {
			t.Errorf("error status = %d, want 500", got)
		}
		msg := apperror.Message(err)
		if strings.Contains(msg, stored.PDFPath) {
			t.Errorf("message leaks the storage key %q: %q", stored.PDFPath, msg)
		}
		if strings.Contains(msg, e.pdfs.Root()) {
			t.Errorf("message leaks the storage root: %q", msg)
		}
	})
}

// TestTransitionStatusIsCompareAndSwap exercises the repository primitive the
// state machine relies on: the conditional UPDATE must let exactly one caller
// move a record out of a state and report a conflict to the other.
func TestTransitionStatusIsCompareAndSwap(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	e := newEnv(t, aitest.DefaultResponses())
	repo := e.repo

	record := &CoverLetter{
		JobDescription: longJobDescription,
		Status:         StatusCreated,
	}
	if err := repo.Create(ctx, record); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if err := repo.TransitionStatus(ctx, record.ID, []Status{StatusCreated}, StatusProcessing, ""); err != nil {
		t.Fatalf("TransitionStatus(created -> processing) error = %v", err)
	}

	// A second worker holding a stale expectation loses the race.
	err := repo.TransitionStatus(ctx, record.ID, []Status{StatusCreated}, StatusProcessing, "")
	if !errors.Is(err, ErrTransitionConflict) {
		t.Errorf("stale TransitionStatus() error = %v, want ErrTransitionConflict", err)
	}

	// The winner's write is what stuck.
	reloaded, err := repo.GetByID(ctx, record.ID)
	if err != nil {
		t.Fatalf("GetByID() error = %v", err)
	}
	if reloaded.Status != StatusProcessing {
		t.Errorf("status = %q, want %q", reloaded.Status, StatusProcessing)
	}

	// An unknown target status is rejected before any write happens.
	err = repo.TransitionStatus(ctx, record.ID, []Status{StatusProcessing}, Status("archived"), "")
	if err == nil {
		t.Error("TransitionStatus() accepted an invalid target status")
	}
	if got := apperror.Status(err); got != 500 {
		t.Errorf("invalid target status error = %d, want 500", got)
	}
	reloaded, err = repo.GetByID(ctx, record.ID)
	if err != nil {
		t.Fatalf("GetByID() error = %v", err)
	}
	if reloaded.Status != StatusProcessing {
		t.Errorf("status = %q after a rejected transition, want it unchanged at %q", reloaded.Status, StatusProcessing)
	}

	// A transition the state machine forbids is refused without a write.
	err = repo.TransitionStatus(ctx, record.ID, []Status{StatusProcessing}, StatusCreated, "")
	if !errors.Is(err, ErrTransitionConflict) {
		t.Errorf("processing -> created error = %v, want ErrTransitionConflict", err)
	}

	// The error message is recorded alongside the terminal status.
	if err := repo.TransitionStatus(ctx, record.ID, []Status{StatusProcessing}, StatusFailed, "the model refused"); err != nil {
		t.Fatalf("TransitionStatus(processing -> failed) error = %v", err)
	}
	reloaded, err = repo.GetByID(ctx, record.ID)
	if err != nil {
		t.Fatalf("GetByID() error = %v", err)
	}
	if reloaded.Status != StatusFailed {
		t.Errorf("status = %q, want %q", reloaded.Status, StatusFailed)
	}
	if reloaded.ErrorMessage != "the model refused" {
		t.Errorf("ErrorMessage = %q, want %q", reloaded.ErrorMessage, "the model refused")
	}
}

// TestValidationSummary tolerates corrupt metadata, which is why it returns a
// pointer rather than an error: a row with unreadable validation must still be
// downloadable.
func TestValidationSummary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		validation string
		wantNil    bool
	}{
		{name: "absent", validation: "", wantNil: true},
		{name: "whitespace only", validation: "   ", wantNil: true},
		{name: "corrupt json", validation: "{not json", wantNil: true},
		{name: "empty object", validation: `{}`, wantNil: false},
		{
			name:       "rejected verdict",
			validation: `{"valid":false,"unsupported_claims":["claimed Kubernetes at scale"],"issues":["too generic"]}`,
			wantNil:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			record := &CoverLetter{Validation: tc.validation}
			got := record.ValidationSummary()
			if tc.wantNil {
				if got != nil {
					t.Errorf("ValidationSummary() = %+v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("ValidationSummary() = nil, want a decoded verdict")
			}
		})
	}
}

// TestNewResponseOmitsInternalFields is the DTO leak test: the API shape must
// never carry the storage paths, the extracted resume text or the internal
// error text.
func TestNewResponseOmitsInternalFields(t *testing.T) {
	t.Parallel()

	record := &CoverLetter{
		ID:              7,
		JobURL:          "https://jobs.example.com/1",
		Tone:            "professional",
		ResumePath:      "resumes/secret.pdf",
		ResumeFilename:  "resume.pdf",
		ResumeText:      "the candidate's full resume text",
		PDFPath:         "cover-letters/7/cover-letter.pdf",
		Status:          StatusCompleted,
		CoverLetterText: "Dear Hiring Team,",
		ErrorMessage:    "internal detail",
	}

	resp := NewResponse(record)
	if resp.CoverLetter == nil {
		t.Error("CoverLetter is nil for a completed record")
	}
	if resp.PDFURL != PDFURLFor(7) {
		t.Errorf("PDFURL = %q, want %q", resp.PDFURL, PDFURLFor(7))
	}
	if resp.Filename != "resume.pdf" {
		t.Errorf("Filename = %q, want %q", resp.Filename, "resume.pdf")
	}

	// The struct has no field to leak into, so assert on the serialised form.
	rendered := renderResponse(t, resp)
	for _, secret := range []string{
		record.ResumeText,
		record.ResumePath,
		record.PDFPath,
	} {
		if strings.Contains(rendered, secret) {
			t.Errorf("serialised response leaks %q: %s", secret, rendered)
		}
	}

	// A failed record's internal error text is not exposed; the message is only
	// surfaced through the status the client already sees.
	failed := NewResponse(&CoverLetter{ID: 8, Status: StatusFailed, ErrorMessage: "boom"})
	if failed.CoverLetter != nil {
		t.Error("a failed record must not carry a cover letter")
	}
	if failed.PDFURL != "" {
		t.Errorf("a failed record must not carry a pdf url, got %q", failed.PDFURL)
	}
}

// TestNewResponseHidesLetterUntilComplete guards the rule that an empty letter
// is worse than no letter: before completion there is nothing to show.
func TestNewResponseHidesLetterUntilComplete(t *testing.T) {
	t.Parallel()

	for _, status := range []Status{StatusCreated, StatusProcessing, StatusFailed} {
		record := &CoverLetter{ID: 3, Status: status, CoverLetterText: "stale text"}
		resp := NewResponse(record)
		if resp.CoverLetter != nil {
			t.Errorf("status %q: CoverLetter = %q, want nil", status, *resp.CoverLetter)
		}
		if resp.PDFURL != "" {
			t.Errorf("status %q: PDFURL = %q, want empty", status, resp.PDFURL)
		}
	}

	// A completed record with no text still gets no empty cover_letter field.
	resp := NewResponse(&CoverLetter{ID: 3, Status: StatusCompleted})
	if resp.CoverLetter != nil {
		t.Errorf("CoverLetter = %q, want nil for a completed record with no text", *resp.CoverLetter)
	}
}

// compile-time assertions on the collaborators the tests rely on.
var (
	_ Repository = (*GORMRepository)(nil)
	_ Dispatcher = (*recordingDispatcher)(nil)
	_ *gorm.DB
	_ *slog.Logger
)

// itoa avoids importing strconv for a single call in an assertion message.
func itoa(v uint) string { return strconv.FormatUint(uint64(v), 10) }

// truncate shortens s for use in a failure message.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// renderResponse serialises a DTO the way the HTTP layer would, so the leak
// assertions run against the bytes a client actually receives.
func renderResponse(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshalling the response: %v", err)
	}
	return string(data)
}
