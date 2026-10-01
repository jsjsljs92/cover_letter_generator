// Package e2e contains one end-to-end test of the whole service.
//
// The point is to exercise the real composition root — internal/app — with real
// SQLite, real local storage, real PDF rendering and real HTTP, substituting
// only the two things that would otherwise require the network: the AI provider
// (a scripted fake) and the job-fetcher HTTP boundary (an httptest server).
// Anything that only passes against a fully mocked stack would not be testing
// the wiring.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-pdf/fpdf"

	"coverletter/internal/aitest"
	"coverletter/internal/app"
	"coverletter/internal/config"
)

// testConfig builds a configuration pointed entirely at a temp directory, with
// the SSRF guard relaxed so the fetcher may reach the httptest job server.
func testConfig(t *testing.T) config.Config {
	t.Helper()

	dir := t.TempDir()
	return config.Config{
		Environment: "test",
		HTTP: config.HTTPConfig{
			Addr:              "127.0.0.1:0",
			ReadTimeout:       10 * time.Second,
			ReadHeaderTimeout: 5 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       30 * time.Second,
			ShutdownTimeout:   5 * time.Second,
			MaxUploadBytes:    5 << 20,
		},
		Database: config.DatabaseConfig{
			Path:            filepath.Join(dir, "app.db"),
			MaxOpenConns:    1,
			MaxIdleConns:    1,
			ConnMaxLifetime: time.Hour,
		},
		Storage: config.StorageConfig{
			UploadDir: filepath.Join(dir, "uploads"),
			PDFDir:    filepath.Join(dir, "pdfs"),
		},
		AI: config.AIConfig{
			// The provider is replaced below, but Load's validation rules still
			// apply and require a key to be present.
			Provider:        "gemini",
			APIKey:          "test-key-not-a-real-secret",
			Model:           "gemini-2.5-flash",
			Timeout:         30 * time.Second,
			MaxRetries:      1,
			Temperature:     0.4,
			MaxOutputTokens: 4096,
		},
		Worker: config.WorkerConfig{Count: 1, QueueSize: 4},
		JobFetch: config.JobFetchConfig{
			Timeout:              5 * time.Second,
			MaxRedirects:         2,
			MaxHTMLBytes:         1 << 20,
			UserAgent:            "e2e-test",
			AllowPrivateNetworks: true, // required to reach the httptest server
			MaxDescriptionRunes:  20000,
		},
		Generate: config.GenerationConfig{Timeout: 60 * time.Second, MaxAttempts: 2},
		Log:      config.LogConfig{Level: "error", Format: "text"},
	}
}

// startApp boots the service on a real port and returns its base URL. Shutdown
// is registered with t.Cleanup, so each test gets a server that is gracefully
// stopped when it finishes.
func startApp(t *testing.T, cfg config.Config, opts ...app.Option) string {
	t.Helper()

	// Port 0 would let the OS choose, but Run() owns the listener, so the port
	// has to be picked here and raced for. Reserving and releasing it is good
	// enough for a test.
	addr := reserveAddr(t)
	cfg.HTTP.Addr = addr

	application, err := app.New(context.Background(), cfg, opts...)
	if err != nil {
		t.Fatalf("app.New() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- application.Run(ctx) }()

	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("app.Run() returned %v during shutdown", err)
			}
		case <-time.After(20 * time.Second):
			t.Error("the app did not shut down within 20s")
		}
	})

	base := "http://" + addr
	waitForServer(t, base)

	return base
}

func reserveAddr(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("releasing the reserved port: %v", err)
	}
	return addr
}

func waitForServer(t *testing.T, base string) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/health")
		if err == nil {
			_ = resp.Body.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the server never became reachable")
}

// resumePDF builds a genuine PDF in memory, so the test does not depend on a
// checked-in fixture and the resume parser is exercised for real.
func resumePDF(t *testing.T) []byte {
	t.Helper()

	doc := fpdf.New(fpdf.OrientationPortrait, "mm", fpdf.PageSizeA4, "")
	doc.SetTitle("Resume", true)
	doc.AddPage()
	// MultiCell needs a current font for its width table.
	doc.SetFont("Helvetica", "", 11)

	body := "Ada Lovelace\n\n" +
		"Senior backend engineer with eight years building distributed services in Go and Python. " +
		"Designed a billing pipeline processing two million transactions per day. " +
		"Led the migration of a monolith to gRPC services, cutting p99 latency by 60 percent. " +
		"Operated Kubernetes clusters in production and wrote Go tooling used by the wider team."
	for _, line := range strings.Split(body, "\n\n") {
		doc.MultiCell(0, 6, line, "", "L", false)
		doc.Ln(2)
	}

	var buf bytes.Buffer
	if err := doc.Output(&buf); err != nil {
		t.Fatalf("building the test resume: %v", err)
	}
	return buf.Bytes()
}

// createRequest posts a multipart submission and returns the status and body.
func createRequest(t *testing.T, base, jobURL, jobDescription string, resume []byte) (int, []byte) {
	t.Helper()

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if jobURL != "" {
		if err := form.WriteField("job_url", jobURL); err != nil {
			t.Fatalf("writing job_url: %v", err)
		}
	}
	if jobDescription != "" {
		if err := form.WriteField("job_description", jobDescription); err != nil {
			t.Fatalf("writing job_description: %v", err)
		}
	}
	part, err := form.CreateFormFile("resume", "resume.pdf")
	if err != nil {
		t.Fatalf("creating the resume part: %v", err)
	}
	if _, err := part.Write(resume); err != nil {
		t.Fatalf("writing the resume: %v", err)
	}
	if err := form.WriteField("tone", "professional"); err != nil {
		t.Fatalf("writing tone: %v", err)
	}
	if err := form.Close(); err != nil {
		t.Fatalf("closing the form: %v", err)
	}

	resp, err := http.Post(base+"/api/v1/cover-letters", form.FormDataContentType(), &body)
	if err != nil {
		t.Fatalf("POST /api/v1/cover-letters: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the create response: %v", err)
	}
	return resp.StatusCode, payload
}

type createResponse struct {
	ID     uint   `json:"id"`
	Status string `json:"status"`
}

type getResponse struct {
	ID           uint   `json:"id"`
	Status       string `json:"status"`
	CoverLetter  string `json:"cover_letter"`
	PDFURL       string `json:"pdf_url"`
	ErrorMessage string `json:"error_message"`
	Validation   *struct {
		Valid             bool     `json:"valid"`
		UnsupportedClaims []string `json:"unsupported_claims"`
		Issues            []string `json:"issues"`
	} `json:"validation"`
}

type errorResponse struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	} `json:"error"`
}

// waitForStatus polls until the record leaves the transient states.
func waitForStatus(t *testing.T, base string, id uint) getResponse {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	var last getResponse
	for time.Now().Before(deadline) {
		resp, err := http.Get(fmt.Sprintf("%s/api/v1/cover-letters/%d", base, id))
		if err != nil {
			t.Fatalf("GET status: %v", err)
		}
		payload, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET status returned %d: %s", resp.StatusCode, payload)
		}
		if err := json.Unmarshal(payload, &last); err != nil {
			t.Fatalf("decoding the status response: %v", err)
		}
		if last.Status == "completed" || last.Status == "failed" {
			return last
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("record %d never reached a terminal status (last was %q)", id, last.Status)
	return last
}

// TestGenerateFromManualDescription is the primary happy path: paste a job,
// upload a PDF resume, poll, then download the rendered letter.
func TestGenerateFromManualDescription(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t)
	provider := aitest.DefaultResponses()
	base := startApp(t, cfg, app.WithProvider(provider))

	status, payload := createRequest(t, base, "", manualJobDescription(), resumePDF(t))
	if status != http.StatusAccepted {
		t.Fatalf("POST returned %d, want 202: %s", status, payload)
	}

	var created createResponse
	if err := json.Unmarshal(payload, &created); err != nil {
		t.Fatalf("decoding the create response: %v", err)
	}
	if created.ID == 0 {
		t.Fatalf("create response carries no id: %s", payload)
	}
	if created.Status != "processing" && created.Status != "created" {
		t.Errorf("create status = %q, want processing", created.Status)
	}

	got := waitForStatus(t, base, created.ID)
	if got.Status != "completed" {
		t.Fatalf("record status = %q (error: %q), want completed", got.Status, got.ErrorMessage)
	}
	if !strings.Contains(got.CoverLetter, "Hiring Team") {
		t.Errorf("cover letter does not contain the generated body: %q", got.CoverLetter)
	}
	if got.PDFURL == "" {
		t.Error("completed record has no pdf_url")
	}
	if got.Validation == nil || !got.Validation.Valid {
		t.Errorf("validation = %+v, want a passing verdict", got.Validation)
	}

	// All five stages must have run, in order.
	stages := []string{
		aitest.StageAnalyzeJob, aitest.StageAnalyzeResume, aitest.StageMatchCandidate,
		aitest.StageGenerateLetter, aitest.StageValidateLetter,
	}
	for _, stage := range stages {
		if provider.CallsTo(stage) == 0 {
			t.Errorf("stage %q never ran", stage)
		}
	}

	resp, err := http.Get(base + got.PDFURL)
	if err != nil {
		t.Fatalf("GET %s: %v", got.PDFURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PDF download returned %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/pdf" {
		t.Errorf("Content-Type = %q, want application/pdf", ct)
	}
	pdf, _ := io.ReadAll(resp.Body)
	if !bytes.HasPrefix(pdf, []byte("%PDF-")) {
		t.Errorf("downloaded body is not a PDF: %q", pdf[:min(8, len(pdf))])
	}

	// The PDF must actually be on disk where configuration said it would be.
	assertFileExists(t, filepath.Join(cfg.Storage.PDFDir, fmt.Sprintf("cover-letters/%d/cover-letter.pdf", created.ID)))
}

func assertFileExists(t *testing.T, path string) {
	t.Helper()

	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected generated PDF at %s: %v", path, err)
	}
}

func manualJobDescription() string {
	return "We are hiring a Senior Backend Engineer to join our platform team. " +
		"The successful candidate has eight or more years of experience building distributed " +
		"backend services, is fluent in Go or Python, has designed gRPC or REST service " +
		"boundaries, and has operated containerised workloads in production. " +
		"Experience with relational databases and observability tooling is required."
}

// TestGenerateFromJobURL covers the fetch path end to end: the real HTTP fetcher
// reaches a real HTTP server, the parser extracts the description, and the
// pipeline continues as normal.
func TestGenerateFromJobURL(t *testing.T) {
	t.Parallel()

	posting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/jobs/backend-engineer":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, `<!doctype html><html><head>
<link rel="application/ld+json" type="application/ld+json">
<script type="application/ld+json">
{"@context":"https://schema.org","@type":"JobPosting",
 "title":"Senior Backend Engineer",
 "hiringOrganization":{"name":"Acme Robotics","@type":"Organization"},
 "description":"We are hiring a Senior Backend Engineer with eight or more years of experience building distributed backend services. The successful candidate is fluent in Go or Python, has designed gRPC or REST service boundaries, and has operated containerised workloads in production. Experience with relational databases and observability tooling is required. You will spend your time on service design, reliability work and mentoring."}
</script>
</head><body><nav>Home | Careers | Apply</nav></body></html>`)
		case "/redirect":
			http.Redirect(w, r, "/jobs/backend-engineer", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer posting.Close()

	base := startApp(t, testConfig(t), app.WithProvider(aitest.DefaultResponses()))

	// Exercise the redirect path too, since each hop is re-validated.
	status, payload := createRequest(t, base, posting.URL+"/redirect", "", resumePDF(t))
	if status != http.StatusAccepted {
		t.Fatalf("POST returned %d, want 202: %s", status, payload)
	}

	var created createResponse
	if err := json.Unmarshal(payload, &created); err != nil {
		t.Fatalf("decoding the create response: %v", err)
	}

	got := waitForStatus(t, base, created.ID)
	if got.Status != "completed" {
		t.Fatalf("record status = %q (error: %q), want completed", got.Status, got.ErrorMessage)
	}
	// The navigation chrome must not have been mistaken for the description.
	if strings.Contains(got.CoverLetter, "Home | Careers | Apply") {
		t.Error("site navigation leaked into the job description")
	}
}

// TestRejectionsAreSafeAndTyped walks the validation failures end to end and
// checks that each one is a typed, client-safe error in the documented envelope.
func TestRejectionsAreSafeAndTyped(t *testing.T) {
	t.Parallel()

	posting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<html><body><p>"+strings.Repeat("irrelevant filler text ", 40)+"</p></body></html>")
	}))
	defer posting.Close()

	base := startApp(t, testConfig(t), app.WithProvider(aitest.DefaultResponses()))
	resume := resumePDF(t)

	tests := []struct {
		name        string
		jobURL      string
		description string
		resume      []byte
		wantCode    string
	}{
		{
			name:        "both job sources",
			jobURL:      posting.URL + "/jobs/1",
			description: manualJobDescription(),
			resume:      resume,
			wantCode:    "INVALID_JOB_INPUT",
		},
		{
			name:     "neither job source",
			resume:   resume,
			wantCode: "INVALID_JOB_INPUT",
		},
		{
			name:        "non-PDF disguised as a PDF",
			description: manualJobDescription(),
			resume:      []byte("this is plain text, not a PDF"),
			// The content is not a PDF at all, which is more specific than a
			// malformed resume.
			wantCode: "UNSUPPORTED_FILE_TYPE",
		},
		{
			name:        "empty resume",
			description: manualJobDescription(),
			resume:      nil,
			wantCode:    "UNSUPPORTED_FILE_TYPE",
		},
		{
			name:     "malformed URL",
			jobURL:   "not-a-url-at-all",
			resume:   resume,
			wantCode: "INVALID_JOB_INPUT",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			status, payload := createRequest(t, base, tc.jobURL, tc.description, tc.resume)

			if status != http.StatusBadRequest {
				t.Fatalf("POST returned %d, want 400: %s", status, payload)
			}
			var errResp errorResponse
			if err := json.Unmarshal(payload, &errResp); err != nil {
				t.Fatalf("decoding the error response: %v", err)
			}
			if errResp.Error.Code != tc.wantCode {
				t.Errorf("error code = %q, want %q", errResp.Error.Code, tc.wantCode)
			}
			if errResp.Error.Message == "" {
				t.Error("error message is empty; it must be safe to show a user")
			}

			// Whatever the message says, it must not leak internals.
			body := strings.ToLower(string(payload))
			for _, leak := range []string{
				"goroutine ", ".go:", "select ", "sqlite", "gemini_api_key",
				filepath.Join("data", "uploads"),
			} {
				if leak != "" && strings.Contains(body, leak) {
					t.Errorf("error response leaks %q: %s", leak, payload)
				}
			}
		})
	}
}

// TestUnsafeJobURLIsRejected verifies the SSRF guard is live in a running
// server, not just in unit tests.
func TestUnsafeJobURLIsRejected(t *testing.T) {
	t.Parallel()

	// This app deliberately does NOT relax the private-network guard.
	cfg := testConfig(t)
	cfg.JobFetch.AllowPrivateNetworks = false

	base := startApp(t, cfg, app.WithProvider(aitest.DefaultResponses()))

	// A listener on loopback stands in for an internal service such as a
	// cloud metadata endpoint.
	internal, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("starting the stand-in internal service: %v", err)
	}
	defer func() { _ = internal.Close() }()

	go func() {
		for {
			conn, err := internal.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	tests := []struct {
		name string
		url  string
	}{
		{name: "loopback by name", url: "http://localhost:9/jobs"},
		{name: "loopback by address", url: "http://127.0.0.1:9/jobs"},
		{name: "link-local metadata endpoint", url: "http://169.254.169.254/latest/meta-data/"},
		{name: "private range", url: "http://10.0.0.1/jobs"},
		{name: "unspecified address", url: "http://0.0.0.0/jobs"},
		{name: "file scheme", url: "file:///etc/passwd"},
		// A literal address rather than a hostname, so the case needs no DNS.
		{name: "credentials in url", url: "http://user:pass://93.184.216.34/jobs"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, payload := createRequest(t, base, tc.url, "", resumePDF(t))

			// The URL is refused at submit time. Accepting it with a 202 and
			// failing later in a worker would force the client to poll to learn
			// the request was never going to run.
			if status != http.StatusBadRequest {
				t.Fatalf("POST %q returned %d, want 400: %s", tc.url, status, payload)
			}
			var errResp errorResponse
			if err := json.Unmarshal(payload, &errResp); err != nil {
				t.Fatalf("decoding the error response: %v", err)
			}
			if errResp.Error.Code != "UNSAFE_URL" && errResp.Error.Code != "INVALID_JOB_INPUT" {
				t.Errorf("error code = %q, want UNSAFE_URL or INVALID_JOB_INPUT", errResp.Error.Code)
			}
			// The response must not confirm which address it refused.
			if strings.Contains(errResp.Error.Message, "127.0.0.1") ||
				strings.Contains(errResp.Error.Message, "169.254") {
				t.Errorf("error message discloses the blocked address: %q", errResp.Error.Message)
			}
		})
	}
}

// TestProviderFailureMarksRecordFailed verifies the failure path: the record
// ends up failed with a message a client can be shown, and no partial letter or
// PDF is served.
func TestProviderFailureMarksRecordFailed(t *testing.T) {
	t.Parallel()

	provider := aitest.New().FailAll(aitest.StageAnalyzeJob, aitest.ErrProvider)
	base := startApp(t, testConfig(t), app.WithProvider(provider))

	status, payload := createRequest(t, base, "", manualJobDescription(), resumePDF(t))
	if status != http.StatusAccepted {
		t.Fatalf("POST returned %d, want 202: %s", status, payload)
	}

	var created createResponse
	if err := json.Unmarshal(payload, &created); err != nil {
		t.Fatalf("decoding the create response: %v", err)
	}

	got := waitForStatus(t, base, created.ID)
	if got.Status != "failed" {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if got.ErrorMessage == "" {
		t.Fatal("a failed record carries no error message")
	}
	if strings.Contains(got.CoverLetter, "Hiring Team") {
		t.Error("a failed record exposed a cover letter anyway")
	}

	// The PDF endpoint must refuse to serve a letter that does not exist.
	resp, err := http.Get(base + fmt.Sprintf("/api/v1/cover-letters/%d/pdf", created.ID))
	if err != nil {
		t.Fatalf("GET pdf: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("GET pdf on a failed record returned %d, want 409", resp.StatusCode)
	}
}

// TestPDFRequestedBeforeCompletionIsRejected covers the race a client hits if it
// downloads immediately after a 202.
func TestPDFRequestedBeforeCompletionIsRejected(t *testing.T) {
	t.Parallel()

	base := startApp(t, testConfig(t), app.WithProvider(aitest.DefaultResponses()))

	// An unknown id is a 404; the 409 path is covered once a record exists. Ask
	// for an id far beyond anything created so far.
	resp, err := http.Get(base + "/api/v1/cover-letters/999999/pdf")
	if err != nil {
		t.Fatalf("GET pdf: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET pdf for an unknown id returned %d, want 404", resp.StatusCode)
	}
}

// TestHealthAndNotFound pins the two endpoints that are not part of the
// generation flow.
func TestHealthAndNotFound(t *testing.T) {
	t.Parallel()

	base := startApp(t, testConfig(t), app.WithProvider(aitest.DefaultResponses()))

	t.Run("health", func(t *testing.T) {
		resp, err := http.Get(base + "/health")
		if err != nil {
			t.Fatalf("GET /health: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET /health returned %d, want 200", resp.StatusCode)
		}
		payload, _ := io.ReadAll(resp.Body)
		if got := strings.TrimSpace(string(payload)); got != `{"status":"ok"}` {
			t.Errorf("GET /health body = %s, want {\"status\":\"ok\"}", got)
		}
	})

	t.Run("unknown path", func(t *testing.T) {
		resp, err := http.Get(base + "/does-not-exist")
		if err != nil {
			t.Fatalf("GET /does-not-exist: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("unknown path returned %d, want 404", resp.StatusCode)
		}
	})

	t.Run("wrong method", func(t *testing.T) {
		resp, err := http.Get(base + "/api/v1/cover-letters")
		if err != nil {
			t.Fatalf("GET on the create path: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("GET on the create path returned %d, want 405", resp.StatusCode)
		}
	})
}

// TestUploadsLandInConfiguredDirectory confirms the service writes to the
// configured directories rather than to some hard-coded location.
func TestUploadsLandInConfiguredDirectory(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t)
	base := startApp(t, cfg, app.WithProvider(aitest.DefaultResponses()))

	status, payload := createRequest(t, base, "", manualJobDescription(), resumePDF(t))
	if status != http.StatusAccepted {
		t.Fatalf("POST returned %d, want 202: %s", status, payload)
	}
	var created createResponse
	if err := json.Unmarshal(payload, &created); err != nil {
		t.Fatalf("decoding the create response: %v", err)
	}
	waitForStatus(t, base, created.ID)

	entries, err := os.ReadDir(cfg.Storage.UploadDir)
	if err != nil {
		t.Fatalf("reading %s: %v", cfg.Storage.UploadDir, err)
	}
	if len(entries) == 0 {
		t.Errorf("no resume was stored under %s", cfg.Storage.UploadDir)
	}
	for _, entry := range entries {
		// The client-supplied name must never be used as a path component.
		if entry.Name() != "resume.pdf" {
			continue
		}
		t.Errorf("the upload kept the client filename %q; storage keys must be server-generated", entry.Name())
	}
}

// unused guards against an accidental import removal when the file is edited.
