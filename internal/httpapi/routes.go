// Package httpapi assembles the HTTP layer: routing and middleware.
//
// Handlers live with their modules; this package only wires them together and
// adds the cross-cutting concerns — correlation ids, structured access logs,
// panic recovery and metrics — that should apply uniformly to every endpoint.
package httpapi

import (
	"encoding/json"
	"net/http"

	"coverletter/internal/apperror"
	"coverletter/internal/coverletter"
	"coverletter/internal/httpx"
	"coverletter/internal/obs"
)

// Middleware is the standard decorator shape.
type Middleware func(http.Handler) http.Handler

// chain applies middlewares so that the first listed is the outermost.
func chain(h http.Handler, middlewares ...Middleware) http.Handler {
	for i := len(middlewares) - 1; i >= 0; i-- {
		h = middlewares[i](h)
	}
	return h
}

// Dependencies are the collaborators the router needs. They are passed in
// rather than constructed here so tests can build a router with fakes.
type Dependencies struct {
	CoverLetters *coverletter.Handler
	Metrics      *obs.Registry
	// CORS is optional; a zero value denies every cross-origin caller, which
	// is the safe default for a same-origin deployment.
	CORS CORSConfig
}

// NewRouter builds the fully-wrapped HTTP handler.
//
// Routing uses the standard library's pattern-matching ServeMux (Go 1.22+). A
// router library would add a dependency and a layer of indirection without
// removing any meaningful complexity from five endpoints.
func NewRouter(deps Dependencies) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", health)
	mux.HandleFunc("GET /metrics", metricsHandler(deps.Metrics))

	// Each path is registered twice: once with its method, which ServeMux routes
	// to the handler, and once without, which catches the other methods so the
	// response uses this API's error envelope instead of ServeMux's plain text.
	mux.HandleFunc("POST /api/v1/cover-letters", deps.CoverLetters.Create)
	mux.HandleFunc("/api/v1/cover-letters", methodNotAllowed("POST"))

	mux.HandleFunc("GET /api/v1/cover-letters/{id}", deps.CoverLetters.Get)
	mux.HandleFunc("/api/v1/cover-letters/{id}", methodNotAllowed("GET"))

	mux.HandleFunc("GET /api/v1/cover-letters/{id}/pdf", deps.CoverLetters.DownloadPDF)
	mux.HandleFunc("/api/v1/cover-letters/{id}/pdf", methodNotAllowed("GET"))

	mux.HandleFunc("/", notFound)

	return chain(mux,
		cors(deps.CORS),
		requestID(),
		recoverPanic(),
		accessLog(),
		metrics(deps.Metrics),
	)
}

// health reports process liveness.
//
// It deliberately does not probe the database or the AI provider: those are
// dependencies that degrade independently, and a liveness probe that fails when
// a dependency is down would restart a process that is still serving traffic.
func health(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
}

// metricsHandler exposes the in-process registry as JSON for operators.
func metricsHandler(registry *obs.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(registry.Snapshot())
	}
}

// methodNotAllowed renders a 405 in the standard error envelope.
func methodNotAllowed(allowed string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", allowed)
		httpx.WriteError(w, r, apperror.New(apperror.CodeMethodNotAllowed,
			"That method is not allowed on this endpoint.", http.StatusMethodNotAllowed))
	}
}

// notFound renders a 404 in the standard error envelope.
func notFound(w http.ResponseWriter, r *http.Request) {
	httpx.WriteError(w, r, apperror.NotFound("No such endpoint."))
}
