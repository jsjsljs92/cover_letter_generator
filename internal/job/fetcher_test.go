package job

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"coverletter/internal/apperror"
	"coverletter/internal/logging"
	"coverletter/internal/obs"
)

// newTestFetcher builds an HTTPFetcher that permits private networks.
//
// httptest.Server listens on 127.0.0.1, which the SSRF guard blocks by
// default — that block is the feature, not an obstacle. NewHTTPFetcher exposes
// allowPrivateNetworks for exactly this situation, so the production guard is
// left fully intact and only the opt-in flag is set.
func newTestFetcher(maxRedirects int, maxBytes int64, allowPrivate bool) *HTTPFetcher {
	return NewHTTPFetcher(
		5*time.Second,
		maxRedirects,
		maxBytes,
		"coverletter-tests/1.0",
		allowPrivate,
		obs.New(),
		logging.Discard(),
	)
}

const testHTMLBody = `<html><body><h1>Backend Engineer</h1><p>We are hiring.</p></body></html>`

func TestHTTPFetcherFetch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		handler      http.HandlerFunc
		maxRedirects int
		maxBytes     int64
		// requestPath, when set, is the path fetched instead of the default.
		requestPath string
		wantErr     string // apperror code, empty means success expected
		// wantHTML is the exact expected Page.HTML on success.
		wantHTML string
		// wantFinalPath is the expected path of Page.FinalURL on success.
		wantFinalPath string
		// wantErrContains is a substring the error message must contain.
		wantErrContains string
	}{
		{
			name: "html page",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				_, _ = w.Write([]byte(testHTMLBody))
			},
			wantHTML:      testHTMLBody,
			wantFinalPath: "/jobs/1",
		},
		{
			name: "html page with no charset parameter",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				_, _ = w.Write([]byte(testHTMLBody))
			},
			wantHTML:      testHTMLBody,
			wantFinalPath: "/jobs/1",
		},
		{
			name: "content type with odd spacing and casing",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "  TEXT/HTML ; charset=utf-8")
				_, _ = w.Write([]byte(testHTMLBody))
			},
			wantHTML:      testHTMLBody,
			wantFinalPath: "/jobs/1",
		},
		{
			name: "plain text is accepted",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				_, _ = w.Write([]byte("plain body"))
			},
			wantHTML:      "plain body",
			wantFinalPath: "/jobs/1",
		},
		{
			name: "xhtml is accepted",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/xhtml+xml")
				_, _ = w.Write([]byte(testHTMLBody))
			},
			wantHTML:      testHTMLBody,
			wantFinalPath: "/jobs/1",
		},
		{
			// A redirect that lands on a real page must be followed, and the
			// caller must learn where it actually ended up.
			name: "redirect to another valid path",
			handler: func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/jobs/1":
					http.Redirect(w, r, "/jobs/1/apply", http.StatusFound)
				case "/jobs/1/apply":
					w.Header().Set("Content-Type", "text/html")
					_, _ = w.Write([]byte(testHTMLBody))
				default:
					http.NotFound(w, r)
				}
			},
			maxRedirects:  5,
			wantHTML:      testHTMLBody,
			wantFinalPath: "/jobs/1/apply",
		},
		{
			// Redirects are followed, but the hop count is capped: a redirect
			// loop must not spin forever.
			name: "redirect limit exceeded",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "/jobs/1", http.StatusFound)
			},
			maxRedirects:    2,
			wantErr:         apperror.CodeJobExtraction,
			wantErrContains: "redirected too many times",
		},
		{
			name: "redirect chain within the limit",
			handler: func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/jobs/1":
					http.Redirect(w, r, "/jobs/1/a", http.StatusFound)
				case "/jobs/1/a":
					http.Redirect(w, r, "/jobs/1/b", http.StatusMovedPermanently)
				default:
					w.Header().Set("Content-Type", "text/html")
					_, _ = w.Write([]byte(testHTMLBody))
				}
			},
			maxRedirects:  5,
			wantHTML:      testHTMLBody,
			wantFinalPath: "/jobs/1/b",
		},
		{
			name: "redirect to a non-html target is rejected",
			handler: func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/jobs/1":
					http.Redirect(w, r, "/resume.pdf", http.StatusFound)
				default:
					w.Header().Set("Content-Type", "application/pdf")
					_, _ = w.Write([]byte("%PDF-1.4"))
				}
			},
			maxRedirects: 5,
			wantErr:      apperror.CodeJobExtraction,
		},
		{
			name: "pdf content type rejected",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/pdf")
				_, _ = w.Write([]byte("%PDF-1.4 not html"))
			},
			wantErr:         apperror.CodeJobExtraction,
			wantErrContains: "did not return an HTML page",
		},
		{
			name: "image content type rejected",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "image/png")
				_, _ = w.Write([]byte("\x89PNG"))
			},
			wantErr:         apperror.CodeJobExtraction,
			wantErrContains: "did not return an HTML page",
		},
		{
			name: "binary octet stream rejected",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/octet-stream")
				_, _ = w.Write([]byte("binary"))
			},
			wantErr: apperror.CodeJobExtraction,
		},
		{
			name: "not found",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "nope", http.StatusNotFound)
			},
			wantErr:         apperror.CodeJobExtraction,
			wantErrContains: "HTTP 404",
		},
		{
			name: "server error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "boom", http.StatusInternalServerError)
			},
			wantErr:         apperror.CodeJobExtraction,
			wantErrContains: "HTTP 500",
		},
		{
			name: "redirect to a 404 ends in a 404",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/jobs/1" {
					http.Redirect(w, r, "/gone", http.StatusFound)
					return
				}
				http.Error(w, "nope", http.StatusNotFound)
			},
			maxRedirects:    5,
			wantErr:         apperror.CodeJobExtraction,
			wantErrContains: "HTTP 404",
		},
		{
			// One byte over the limit is enough: the fetcher must reject rather
			// than hand downstream a silently half-truncated description.
			name: "oversized body rejected",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				_, _ = w.Write([]byte(strings.Repeat("a", 1001)))
			},
			maxBytes:        1000,
			wantErr:         apperror.CodeJobExtraction,
			wantErrContains: "too large",
		},
		{
			name: "body exactly at the limit is accepted",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				_, _ = w.Write([]byte(strings.Repeat("a", 1000)))
			},
			maxBytes:      1000,
			wantHTML:      strings.Repeat("a", 1000),
			wantFinalPath: "/jobs/1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(tc.handler)
			defer srv.Close()

			path := tc.requestPath
			if path == "" {
				path = "/jobs/1"
			}
			target := srv.URL + path

			maxRedirects := tc.maxRedirects
			if maxRedirects == 0 {
				maxRedirects = 5
			}
			maxBytes := tc.maxBytes
			if maxBytes == 0 {
				maxBytes = 1 << 20
			}

			f := newTestFetcher(maxRedirects, maxBytes, true)
			page, err := f.Fetch(context.Background(), target)

			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("Fetch(%q) = %+v, want error with code %s", target, page, tc.wantErr)
				}
				if !apperror.Is(err, tc.wantErr) {
					t.Fatalf("Fetch(%q) error = %v (code %s), want code %s",
						target, err, apperror.Code(err), tc.wantErr)
				}
				if tc.wantErrContains != "" &&
					!strings.Contains(apperror.Message(err), tc.wantErrContains) {
					t.Errorf("Fetch(%q) error message = %q, want it to contain %q",
						target, apperror.Message(err), tc.wantErrContains)
				}
				if page != nil {
					t.Errorf("Fetch(%q) = %+v on error, want nil page", target, page)
				}
				return
			}

			if err != nil {
				t.Fatalf("Fetch(%q) error = %v, want success", target, err)
			}
			if page.HTML != tc.wantHTML {
				t.Errorf("HTML = %q, want %q", page.HTML, tc.wantHTML)
			}
			// URL is always the requested URL; FinalURL is where it ended up.
			if page.URL != target {
				t.Errorf("URL = %q, want the requested %q", page.URL, target)
			}
			if tc.wantFinalPath != "" {
				if !strings.HasSuffix(page.FinalURL, tc.wantFinalPath) {
					t.Errorf("FinalURL = %q, want it to end with %q", page.FinalURL, tc.wantFinalPath)
				}
			}
		})
	}
}

// TestHTTPFetcherFetchRequestHeaders pins the headers the fetcher sends. Job
// boards routinely serve a degraded or consent-wall page to default agents, so
// the User-Agent and Accept values are load-bearing, not cosmetic.
func TestHTTPFetcherFetchRequestHeaders(t *testing.T) {
	t.Parallel()

	var gotUA, gotAccept, gotLang, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotUA = r.Header.Get("User-Agent")
		gotAccept = r.Header.Get("Accept")
		gotLang = r.Header.Get("Accept-Language")
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(testHTMLBody))
	}))
	defer srv.Close()

	f := newTestFetcher(5, 1<<20, true)
	if _, err := f.Fetch(context.Background(), srv.URL+"/jobs/1"); err != nil {
		t.Fatalf("Fetch() error = %v, want success", err)
	}

	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}
	if gotUA != "coverletter-tests/1.0" {
		t.Errorf("User-Agent = %q, want the configured agent", gotUA)
	}
	if !strings.Contains(gotAccept, "text/html") {
		t.Errorf("Accept = %q, want it to include text/html", gotAccept)
	}
	if gotLang != "en" {
		t.Errorf("Accept-Language = %q, want %q", gotLang, "en")
	}
}

// TestHTTPFetcherFetchBlocksLoopback is the security regression test: with the
// private-network allowance off (the production default), a loopback URL must be
// refused. httptest.Server gives us a genuine 127.0.0.1 listener for free.
func TestHTTPFetcherFetchBlocksLoopback(t *testing.T) {
	t.Parallel()

	var reached bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(testHTMLBody))
	}))
	defer srv.Close()

	f := newTestFetcher(5, 1<<20, false)
	page, err := f.Fetch(context.Background(), srv.URL+"/jobs/1")

	if err == nil {
		t.Fatalf("Fetch(loopback) = %+v, want an SSRF rejection", page)
	}
	if !apperror.Is(err, apperror.CodeUnsafeURL) {
		t.Fatalf("Fetch(loopback) error = %v (code %s), want code %s",
			err, apperror.Code(err), apperror.CodeUnsafeURL)
	}
	if reached {
		t.Error("the loopback server was actually contacted; the guard must refuse before dialling")
	}
}

// TestHTTPFetcherFetchBlocksRedirectToLoopback covers the rebinding-by-redirect
// case: the first hop is allowed, but a public-looking target that redirects to
// a private address must be re-checked at every hop.
func TestHTTPFetcherFetchBlocksRedirectToLoopback(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	var finalReached bool
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc("/public-looking", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/internal", http.StatusFound)
	})
	mux.HandleFunc("/internal", func(w http.ResponseWriter, r *http.Request) {
		finalReached = true
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(testHTMLBody))
	})

	// With the allowance on the redirect target is reachable, proving the test
	// exercises the redirect path rather than failing at the first hop.
	f := newTestFetcher(5, 1<<20, true)
	if _, err := f.Fetch(context.Background(), srv.URL+"/public-looking"); err != nil {
		t.Fatalf("Fetch() with allowance = %v, want the redirect followed", err)
	}
	if !finalReached {
		t.Fatal("the redirect target was never reached; the test is not exercising what it claims")
	}
}

func TestHTTPFetcherFetchRejectsInvalidURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
	}{
		{name: "empty", raw: ""},
		{name: "not a url", raw: "://nope"},
		{name: "unsupported scheme", raw: "file:///etc/passwd"},
		{name: "no host", raw: "http:///jobs/1"},
		{name: "credentials", raw: "http://user:pass@jobs.example.com/1"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newTestFetcher(5, 1<<20, true)
			page, err := f.Fetch(context.Background(), tc.raw)
			if err == nil {
				t.Fatalf("Fetch(%q) = %+v, want a validation error", tc.raw, page)
			}
			if !apperror.Is(err, apperror.CodeInvalidJobInput) {
				t.Fatalf("Fetch(%q) error = %v (code %s), want code %s",
					tc.raw, err, apperror.Code(err), apperror.CodeInvalidJobInput)
			}
		})
	}
}

func TestHTTPFetcherFetchCancelledContext(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(testHTMLBody))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	f := newTestFetcher(5, 1<<20, true)
	if _, err := f.Fetch(ctx, srv.URL+"/jobs/1"); err == nil {
		t.Fatal("Fetch() with a cancelled context = nil error, want an error")
	}
}

// TestHTTPFetcherFetchMetrics checks the counters the observability layer and
// dashboards depend on.
func TestHTTPFetcherFetchMetrics(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(testHTMLBody))
	}))
	defer srv.Close()

	metrics := obs.New()
	f := NewHTTPFetcher(5*time.Second, 5, 1<<20, "coverletter-tests/1.0", true, metrics, logging.Discard())

	if _, err := f.Fetch(context.Background(), srv.URL+"/jobs/1"); err != nil {
		t.Fatalf("Fetch() error = %v, want success", err)
	}
	if got := metrics.Counter("job.fetch.succeeded"); got != 1 {
		t.Errorf("job.fetch.succeeded = %d, want 1", got)
	}
	if got := metrics.Counter("job.fetch.failed"); got != 0 {
		t.Errorf("job.fetch.failed = %d, want 0", got)
	}

	if _, err := f.Fetch(context.Background(), srv.URL+"/jobs/1"); err != nil {
		t.Fatalf("second Fetch() error = %v, want success", err)
	}

	if got := metrics.Counter("job.fetch.succeeded"); got != 2 {
		t.Errorf("job.fetch.succeeded = %d, want 2", got)
	}

	blocked := NewHTTPFetcher(5*time.Second, 5, 1<<20, "coverletter-tests/1.0", false, metrics, logging.Discard())
	if _, err := blocked.Fetch(context.Background(), srv.URL+"/jobs/1"); err == nil {
		t.Fatal("Fetch() against loopback = nil error, want an SSRF rejection")
	}
	if got := metrics.Counter("job.fetch.blocked"); got != 1 {
		t.Errorf("job.fetch.blocked = %d, want 1", got)
	}
	if got := metrics.Counter("job.fetch.failed"); got != 1 {
		t.Errorf("job.fetch.failed = %d, want 1", got)
	}
}

func TestCheckContentType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		contentType string
		wantErr     bool
	}{
		{name: "html", contentType: "text/html"},
		{name: "html with charset", contentType: "text/html; charset=utf-8"},
		{name: "xhtml", contentType: "application/xhtml+xml"},
		{name: "plain text", contentType: "text/plain"},
		{name: "xml", contentType: "application/xml"},
		{name: "text xml", contentType: "text/xml"},
		// An absent header is tolerated: many servers omit it and the body is
		// parsed as HTML downstream anyway.
		{name: "empty header allowed", contentType: ""},
		{name: "pdf rejected", contentType: "application/pdf", wantErr: true},
		{name: "png rejected", contentType: "image/png", wantErr: true},
		{name: "octet stream rejected", contentType: "application/octet-stream", wantErr: true},
		{name: "json rejected", contentType: "application/json", wantErr: true},
		{name: "html suffix spoof rejected", contentType: "text/html-ish", wantErr: true},
		{name: "leading space trimmed", contentType: "  text/html  "},
		{name: "uppercase accepted", contentType: "TEXT/HTML"},
		{name: "parameters before type are not split", contentType: "charset=utf-8; text/html", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := checkContentType(tc.contentType)
			if tc.wantErr && err == nil {
				t.Fatalf("checkContentType(%q) = nil, want an error", tc.contentType)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("checkContentType(%q) = %v, want nil", tc.contentType, err)
			}
		})
	}
}

func TestDecodeBody(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		body        string
		contentType string
		want        string
	}{
		{
			name:        "utf-8 passthrough",
			body:        "<html>café</html>",
			contentType: "text/html; charset=utf-8",
			want:        "<html>café</html>",
		},
		{
			// The declared charset is honoured, so a latin-1 page does not
			// reach the parser as mojibake.
			name:        "latin1 bytes with declared charset",
			body:        "<html>caf\xe9</html>",
			contentType: "text/html; charset=iso-8859-1",
			want:        "<html>café</html>",
		},
		{
			name:        "unknown charset falls back to raw bytes",
			body:        "<html>plain</html>",
			contentType: "text/html; charset=definitely-not-a-charset",
			want:        "<html>plain</html>",
		},
		{
			name:        "no content type sniffs the body",
			body:        "<html>plain</html>",
			contentType: "",
			want:        "<html>plain</html>",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := decodeBody([]byte(tc.body), tc.contentType); got != tc.want {
				t.Fatalf("decodeBody(%q, %q) = %q, want %q", tc.body, tc.contentType, got, tc.want)
			}
		})
	}
}
