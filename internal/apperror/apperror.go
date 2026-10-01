// Package apperror defines the application's stable error vocabulary.
//
// Every failure that can reach a client is represented as an *Error carrying a
// machine-readable code and a client-safe message. The underlying cause is kept
// for logs and error wrapping but is never rendered into an HTTP response, so
// SQL errors, API keys and stack traces cannot leak to callers.
package apperror

import (
	"errors"
	"fmt"
	"net/http"
)

// Stable error codes. These are part of the public API contract: clients may
// branch on them, so they must not be renamed without a version bump.
const (
	CodeValidation       = "VALIDATION_ERROR"
	CodeInvalidJobInput  = "INVALID_JOB_INPUT"
	CodeInvalidResume    = "INVALID_RESUME"
	CodeUnsupportedType  = "UNSUPPORTED_FILE_TYPE"
	CodePayloadTooLarge  = "PAYLOAD_TOO_LARGE"
	CodeMalformedBody    = "MALFORMED_REQUEST"
	CodeUnsafeURL        = "UNSAFE_URL"
	CodeJobExtraction    = "JOB_EXTRACTION_FAILED"
	CodeResumeParsing    = "RESUME_PARSING_FAILED"
	CodeAIProvider       = "AI_PROVIDER_ERROR"
	CodeAIInvalidOutput  = "AI_INVALID_OUTPUT"
	CodePDFFailure       = "PDF_GENERATION_FAILED"
	CodeStorage          = "STORAGE_ERROR"
	CodeNotFound         = "NOT_FOUND"
	CodeInvalidState     = "INVALID_STATE"
	CodeQueueFull        = "QUEUE_FULL"
	CodeMethodNotAllowed = "METHOD_NOT_ALLOWED"
	CodeUnavailable      = "SERVICE_UNAVAILABLE"
	CodeInternal         = "INTERNAL_ERROR"
)

// Sentinel errors that callers may branch on with errors.Is. They exist so that
// lower layers (the worker pool, the queue) can signal a condition without
// importing an HTTP concept.
var (
	// ErrQueueFull indicates backpressure: the work queue has no capacity.
	ErrQueueFull = errors.New("queue is full")
)

// Error is a client-facing application error.
type Error struct {
	// Code is the stable, machine-readable error code.
	Code string
	// Message is the client-safe description. Never include user data or internals here.
	Message string
	// Status is the HTTP status code this error maps to.
	Status int
	// cause is the wrapped internal error. It is logged, never serialised.
	cause error
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap exposes the internal cause to errors.Is/errors.As.
func (e *Error) Unwrap() error { return e.cause }

// New builds an application error without a cause.
func New(code, message string, status int) *Error {
	return &Error{Code: code, Message: message, Status: status}
}

// Wrap builds an application error that carries an internal cause. The cause is
// recorded for logging; the message must still be safe to return to a client.
func Wrap(cause error, code, message string, status int) *Error {
	return &Error{Code: code, Message: message, Status: status, cause: cause}
}

// Validation reports invalid client input.
func Validation(code, message string) *Error {
	return New(code, message, http.StatusBadRequest)
}

// NotFound reports a missing resource.
func NotFound(message string) *Error {
	return New(CodeNotFound, message, http.StatusNotFound)
}

// Conflict reports an illegal state transition or conflicting resource state.
func Conflict(code, message string) *Error {
	return New(code, message, http.StatusConflict)
}

// Unavailable reports that the service is temporarily unable to serve the
// request; callers may retry.
func Unavailable(code, message string) *Error {
	return New(code, message, http.StatusServiceUnavailable)
}

// Internal wraps an unexpected failure. The cause must never be exposed.
func Internal(cause error, message string) *Error {
	return Wrap(cause, CodeInternal, message, http.StatusInternalServerError)
}

// From extracts the application error from an error chain.
func From(err error) (*Error, bool) {
	var appErr *Error
	if errors.As(err, &appErr) {
		return appErr, true
	}
	return nil, false
}

// Is reports whether err (or anything it wraps) is an application error with
// the given code.
func Is(err error, code string) bool {
	appErr, ok := From(err)
	return ok && appErr.Code == code
}

// Message returns the client-safe message for err, or a generic fallback when
// err is not an application error. The internal cause is deliberately dropped.
func Message(err error) string {
	if appErr, ok := From(err); ok {
		return appErr.Message
	}
	return "An unexpected error occurred."
}

// Code returns the application error code for err, or CodeInternal.
func Code(err error) string {
	if appErr, ok := From(err); ok {
		return appErr.Code
	}
	return CodeInternal
}

// Cause returns the internal cause of err for logging purposes.
func Cause(err error) error {
	if appErr, ok := From(err); ok && appErr.cause != nil {
		return appErr.cause
	}
	return nil
}

// Status returns the HTTP status for err, defaulting to 500.
func Status(err error) int {
	if appErr, ok := From(err); ok {
		return appErr.Status
	}
	return http.StatusInternalServerError
}
