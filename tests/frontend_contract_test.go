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
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	"coverletter/internal/aitest"
	"coverletter/internal/app"
)

// browserOrigin is the origin the Vite dev server serves the frontend from. It
// stands in for the browser in the CORS assertions below.
const browserOrigin = "http://localhost:5173"

// TestBrowserFlowOverCORS walks the exact sequence the frontend performs: a
// preflight, the multipart create, the poll loop and the PDF download — each
// carrying an Origin header, as a browser would.
//
// The assertions are written against the field names the frontend reads
// (cover_letter, error_message, candidate_name, pdf_url), so a rename on either
// side breaks this test rather than surfacing as "[object Object]" in a browser.
func TestBrowserFlowOverCORS(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t)
	cfg.HTTP.CORSAllowedOrigins = []string{browserOrigin}
	cfg.HTTP.CORSMaxAgeSeconds = 600
	base := startApp(t, cfg, app.WithProvider(aitest.DefaultResponses()))
	client := &http.Client{Timeout: 30 * time.Second}

	// A preflight is not strictly required for a simple multipart POST, but the
	// frontend may add headers, and the browser caches this answer.
	preflight := newRequest(t, http.MethodOptions, base+"/api/v1/cover-letters", nil)
	preflight.Header.Set("Origin", browserOrigin)
	preflight.Header.Set("Access-Control-Request-Method", "POST")
	preflight.Header.Set("Access-Control-Request-Headers", "content-type")

	preResp := do(t, client, preflight)
	if preResp.StatusCode != http.StatusNoContent {
		t.Errorf("preflight status = %d, want 204", preResp.StatusCode)
	}
	if got := preResp.Header.Get("Access-Control-Allow-Origin"); got != browserOrigin {
		t.Errorf("preflight Allow-Origin = %q, want %q", got, browserOrigin)
	}

	// Create, exactly as createCoverLetter() builds it.
	//
	// The multipart body is assembled before the request is built: http.NewRequest
	// reads the buffer's length to set Content-Length, so creating the request
	// against an empty buffer would advertise a zero-length body.
	createBuf := &bytes.Buffer{}
	form := multipart.NewWriter(createBuf)
	writeField(t, form, "job_description", manualJobDescription())
	part, err := form.CreateFormFile("resume", "resume.pdf")
	if err != nil {
		t.Fatalf("creating the resume part: %v", err)
	}
	if _, err := part.Write(resumePDF(t)); err != nil {
		t.Fatalf("writing the resume: %v", err)
	}
	writeField(t, form, "tone", "professional")
	closeForm(t, form)

	create := newRequest(t, http.MethodPost, base+"/api/v1/cover-letters", createBuf)
	create.Header.Set("Origin", browserOrigin)
	create.Header.Set("Content-Type", form.FormDataContentType())

	createResp := do(t, client, create)
	// Without this header the browser discards the body and the frontend's
	// fetch rejects with an opaque TypeError.
	if got := createResp.Header.Get("Access-Control-Allow-Origin"); got != browserOrigin {
		t.Fatalf("create Allow-Origin = %q, want %q; the browser could not read this response", got, browserOrigin)
	}
	if createResp.StatusCode != http.StatusAccepted {
		t.Fatalf("create status = %d, want 202: %s", createResp.StatusCode, createResp.Body)
	}

	var created struct {
		ID     uint   `json:"id"`
		Status string `json:"status"`
	}
	decode(t, createResp.Body, &created)
	if created.ID == 0 {
		t.Fatalf("create response carries no id: %s", createResp.Body)
	}

	got := waitForStatus(t, base, created.ID)
	if got.Status != "completed" {
		t.Fatalf("status = %q (error: %q), want completed", got.Status, got.ErrorMessage)
	}
	// The field the preview renders.
	if !strings.Contains(got.CoverLetter, "Hiring Team") {
		t.Errorf("cover_letter is empty or unexpected: %q", got.CoverLetter)
	}
	if got.PDFURL != fmt.Sprintf("/api/v1/cover-letters/%d/pdf", created.ID) {
		t.Errorf("pdf_url = %q, want the path the frontend builds from the id", got.PDFURL)
	}

	// Download, as downloadCoverLetterPdf() does.
	pdfReq := newRequest(t, http.MethodGet, base+got.PDFURL, nil)
	pdfReq.Header.Set("Origin", browserOrigin)
	pdfResp := do(t, client, pdfReq)
	if pdfResp.StatusCode != http.StatusOK {
		t.Fatalf("pdf status = %d, want 200", pdfResp.StatusCode)
	}
	if ct := pdfResp.Header.Get("Content-Type"); ct != "application/pdf" {
		t.Errorf("pdf Content-Type = %q, want application/pdf", ct)
	}
	if !bytes.HasPrefix(pdfResp.Body, []byte("%PDF-")) {
		t.Error("the downloaded body is not a PDF")
	}
}

// TestFailureUsesNestedErrorEnvelope pins the envelope the frontend must unwrap.
//
// The old client read a top-level `message` and fell back to assigning the
// `error` *object* to a string, which rendered "[object Object]". Asserting the
// shape here keeps both sides honest.
func TestFailureUsesNestedErrorEnvelope(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t)
	cfg.HTTP.CORSAllowedOrigins = []string{browserOrigin}
	base := startApp(t, cfg, app.WithProvider(aitest.DefaultResponses()))

	// No job input at all: the service rejects it during validation.
	body := &bytes.Buffer{}
	form := multipart.NewWriter(body)
	part, err := form.CreateFormFile("resume", "resume.pdf")
	if err != nil {
		t.Fatalf("creating the resume part: %v", err)
	}
	if _, err := part.Write(resumePDF(t)); err != nil {
		t.Fatalf("writing the resume: %v", err)
	}
	closeForm(t, form)

	req := newRequest(t, http.MethodPost, base+"/api/v1/cover-letters", body)
	req.Header.Set("Origin", browserOrigin)
	req.Header.Set("Content-Type", form.FormDataContentType())

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	payload, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want a 4xx: %s", resp.StatusCode, payload)
	}

	var envelope struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatalf("the error body is not the expected envelope: %v (%s)", err, payload)
	}
	if envelope.Error.Message == "" {
		t.Error("envelope has no nested message; the frontend would show a blank error")
	}
	if envelope.Error.RequestID == "" {
		t.Error("envelope has no request_id; the frontend cannot cite one to an operator")
	}
}

// TestUnknownOriginIsRefused confirms the policy is a real restriction: a
// browser on an unlisted origin must not be able to read responses.
func TestUnknownOriginIsRefused(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t)
	cfg.HTTP.CORSAllowedOrigins = []string{browserOrigin}
	base := startApp(t, cfg, app.WithProvider(aitest.DefaultResponses()))

	req := newRequest(t, http.MethodGet, base+"/health", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Allow-Origin = %q for an unlisted origin, want empty", got)
	}
}

// response is a fully-read HTTP response, so callers can assert on the body
// after the connection is closed.
type response struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

// newRequest builds a request for method and url. When body is non-nil the
// caller keeps the buffer so it can be filled in before the request is sent;
// http.NewRequest wraps the writer in a ReadCloser, so the buffer is passed
// back rather than read off the request.
func newRequest(t *testing.T, method, url string, body *bytes.Buffer) *http.Request {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = body
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("building the %s %s request: %v", method, url, err)
	}
	return req
}

// do sends a request and drains the response.
func do(t *testing.T, client *http.Client, req *http.Request) response {
	t.Helper()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the response body: %v", err)
	}
	return response{StatusCode: resp.StatusCode, Header: resp.Header, Body: body}
}

func writeField(t *testing.T, form *multipart.Writer, name, value string) {
	t.Helper()
	if err := form.WriteField(name, value); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
}

func closeForm(t *testing.T, form *multipart.Writer) {
	t.Helper()
	if err := form.Close(); err != nil {
		t.Fatalf("closing the form: %v", err)
	}
}

func decode(t *testing.T, payload []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(payload, v); err != nil {
		t.Fatalf("decoding the response: %v (%s)", err, payload)
	}
}
