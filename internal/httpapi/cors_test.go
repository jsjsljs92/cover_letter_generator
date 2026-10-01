package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// corsHandler builds a router-shaped handler carrying only the CORS policy, so
// these tests exercise the middleware without standing up the full API.
func corsHandler(cfg CORSConfig) http.Handler {
	return cors(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
}

func TestCORSAllowsConfiguredOrigin(t *testing.T) {
	h := corsHandler(CORSConfig{AllowedOrigins: []string{"http://localhost:5173"}})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/cover-letters", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
		t.Errorf("Allow-Origin = %q, want the requesting origin echoed back", got)
	}
	// The response varies by Origin, so caches must not share it across origins.
	if vary := rec.Header().Get("Vary"); vary != "Origin" {
		t.Errorf("Vary = %q, want %q", vary, "Origin")
	}
}

// A disallowed origin must get no grant header at all, which is what makes the
// browser refuse to expose the body to the caller's JavaScript.
func TestCORSRejectsUnknownOrigin(t *testing.T) {
	h := corsHandler(CORSConfig{AllowedOrigins: []string{"http://localhost:5173"}})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/cover-letters", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Allow-Origin = %q, want empty for a disallowed origin", got)
	}
	// The request itself still runs: CORS is a browser policy, not authz.
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want the handler to still run (%d)", rec.Code, http.StatusOK)
	}
}

func TestCORSPreflightSucceeds(t *testing.T) {
	h := corsHandler(CORSConfig{
		AllowedOrigins: []string{"http://localhost:5173"},
		MaxAge:         600,
	})

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/cover-letters", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "content-type")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); got == "" {
		t.Error("Allow-Methods is empty; the browser would reject the preflight")
	}
	if got := rec.Header().Get("Access-Control-Max-Age"); got != "600" {
		t.Errorf("Max-Age = %q, want %q", got, "600")
	}
}

func TestCORSPreflightDeniedForUnknownOrigin(t *testing.T) {
	h := corsHandler(CORSConfig{AllowedOrigins: []string{"http://localhost:5173"}})

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/cover-letters", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Allow-Origin = %q, want empty on a denied preflight", got)
	}
}

// A wildcard must not be combined with credentials, since the spec forbids it
// and the browser discards such a response. Echoing the origin keeps the
// response usable.
func TestCORSWildcardWithCredentialsEchoesOrigin(t *testing.T) {
	h := corsHandler(CORSConfig{AllowedOrigins: []string{"*"}, AllowCredentials: true})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/cover-letters/1", nil)
	req.Header.Set("Origin", "https://app.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Errorf("Allow-Origin = %q, want the concrete origin echoed back", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("Allow-Credentials = %q, want %q", got, "true")
	}
}

// A same-origin request (no Origin header) must not be decorated.
func TestCORSNoOriginHeader(t *testing.T) {
	h := corsHandler(CORSConfig{AllowedOrigins: []string{"*"}})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Allow-Origin = %q, want empty for a same-origin request", got)
	}
}
