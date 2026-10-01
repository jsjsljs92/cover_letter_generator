// Package httpx holds the small HTTP primitives shared by the handlers and the
// router: request correlation and consistent JSON rendering.
//
// It exists as its own package so a handler can render an error without
// importing the router package that constructs the handler.
package httpx

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"coverletter/internal/apperror"
)

type contextKey string

const requestIDKey contextKey = "request_id"

// WithRequestID returns a context carrying the request's correlation id.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestID returns the correlation id for the request, or an empty string.
func RequestID(ctx context.Context) string {
	if id, ok := ctx.Value(requestIDKey).(string); ok {
		return id
	}
	return ""
}

// ErrorBody is the stable error envelope returned for every failed request.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail carries the machine-readable code and a safe message.
//
// The request id lets a user quote a failure to an operator, who can then find
// the full internal error in the logs without the error itself having to be
// exposed over the wire.
type ErrorDetail struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

// WriteJSON renders v as JSON with the given status code.
func WriteJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		// Encoding a response that has already been composed is a programming
		// error, not a client error; fall back to a minimal valid body.
		slog.Error("could not encode response", slog.String("error", err.Error()))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"INTERNAL_ERROR","message":"The response could not be encoded."}}`))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// WriteError renders an application error as JSON.
//
// Only the error's client-safe message is serialised; the wrapped cause is
// logged with the request id and never returned. This is the single place where
// that rule is enforced for the whole API.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	status := apperror.Status(err)
	code := apperror.Code(err)
	message := apperror.Message(err)
	requestID := RequestID(r.Context())

	attrs := []any{
		slog.String("code", code),
		slog.Int("status", status),
		slog.String("path", r.URL.Path),
	}
	if requestID != "" {
		attrs = append(attrs, slog.String("request_id", requestID))
	}
	if cause := apperror.Cause(err); cause != nil {
		attrs = append(attrs, slog.String("cause", cause.Error()))
	}

	// A 5xx is our fault and needs a stack-level trace; a 4xx is expected
	// client behaviour and is logged at a lower level to avoid noise.
	if status >= http.StatusInternalServerError {
		slog.Error("request failed", append(attrs, slog.String("error", err.Error()))...)
	} else {
		slog.Debug("request rejected", append(attrs, slog.String("error", err.Error()))...)
	}

	WriteJSON(w, r, status, ErrorBody{Error: ErrorDetail{
		Code:      code,
		Message:   message,
		RequestID: requestID,
	}})
}
