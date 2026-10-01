// Package ai contains the model-facing abstraction and the multi-stage agent
// that drives cover letter generation.
//
// Two ideas keep this package honest:
//
//  1. The application depends on the small Provider interface, never on a
//     vendor SDK, so a local Ollama provider can be added without touching the
//     workflow.
//  2. Every stage requests a typed JSON object validated against a schema.
//     Free-form model output would make downstream parsing the least reliable
//     part of the system.
package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Provider is a text-generation backend.
//
// Implementations must honour context cancellation, bound their own latency,
// and retry only transient failures.
type Provider interface {
	// Generate produces a completion for req.
	Generate(ctx context.Context, req GenerateRequest) (*GenerateResponse, error)
	// Name identifies the provider in logs and metrics.
	Name() string
}

// GenerateRequest is a single model call.
type GenerateRequest struct {
	// Stage names the workflow step, used for logging and metrics only.
	Stage string
	// SystemPrompt sets behaviour and constraints for the model.
	SystemPrompt string
	// Prompt is the user-turn content.
	Prompt string
	// Schema, when set, constrains the response to a JSON object of this shape.
	Schema *Schema
	// Temperature overrides the provider default when non-nil.
	Temperature *float32
	// MaxOutputTokens caps the response size when non-zero.
	MaxOutputTokens int
}

// GenerateResponse is a completed model call.
type GenerateResponse struct {
	// Text is the raw response text. For schema-constrained calls it is JSON.
	Text string
	// Model is the model that served the request.
	Model string
	// Attempts is how many attempts were made, including the successful one.
	Attempts int
	// Duration is the wall time spent in the provider.
	Duration time.Duration
	// Usage is token accounting, when the provider reports it.
	Usage Usage
}

// Usage is token accounting for a single call.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// Schema is the subset of JSON Schema needed to constrain model output.
type Schema struct {
	Type        string
	Description string
	Enum        []string
	Items       *Schema
	Properties  map[string]*Schema
	Required    []string
}

// JSON Schema type names.
const (
	TypeString  = "STRING"
	TypeNumber  = "NUMBER"
	TypeBoolean = "BOOLEAN"
	TypeArray   = "ARRAY"
	TypeObject  = "OBJECT"
)

// ErrEmptyResponse indicates the model returned nothing usable.
var ErrEmptyResponse = errors.New("ai: provider returned an empty response")

// DecodeStructured extracts a JSON object from a model response and decodes it
// into dst.
//
// Models wrap JSON in prose or fences even when a schema is supplied, so the
// outermost balanced object is located and decoded. This keeps a stray "Here is
// the JSON:" prefix from failing an otherwise valid response.
func DecodeStructured(text string, dst any) error {
	payload, err := extractJSONObject(text)
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(payload), dst); err != nil {
		return fmt.Errorf("ai: decode structured response: %w", err)
	}
	return nil
}

// extractJSONObject returns the substring spanning the first '{' and its
// matching '}', ignoring markdown code fences and surrounding prose.
func extractJSONObject(text string) (string, error) {
	trimmed := stripCodeFence(text)
	if trimmed == "" {
		return "", ErrEmptyResponse
	}

	start := strings.IndexByte(trimmed, '{')
	if start < 0 {
		return "", fmt.Errorf("ai: response contained no JSON object: %w", ErrEmptyResponse)
	}

	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(trimmed); i++ {
		c := trimmed[i]
		switch {
		case escaped:
			escaped = false
		case c == '\\' && inString:
			escaped = true
		case c == '"':
			inString = !inString
		case inString:
			// Structural characters inside strings do not affect nesting.
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return trimmed[start : i+1], nil
			}
		}
	}
	return "", fmt.Errorf("ai: response contained an unterminated JSON object: %w", ErrEmptyResponse)
}

// stripCodeFence removes a surrounding ```json ... ``` fence if present.
func stripCodeFence(text string) string {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "```") {
		return trimmed
	}
	// Drop the opening fence and its optional language tag.
	if idx := strings.IndexByte(trimmed, '\n'); idx >= 0 {
		trimmed = trimmed[idx+1:]
	} else {
		return trimmed
	}
	if idx := strings.LastIndex(trimmed, "```"); idx >= 0 {
		trimmed = trimmed[:idx]
	}
	return strings.TrimSpace(trimmed)
}

// Validate reports whether a decoded structured payload has the required
// fields. The model is instructed to return exactly these keys, so a missing
// one signals a contract violation worth surfacing rather than papering over
// with a zero value.
func Validate(payload map[string]any, required ...string) error {
	var missing []string
	for _, key := range required {
		if _, ok := payload[key]; !ok {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("ai: response is missing required fields: %s", strings.Join(missing, ", "))
	}
	return nil
}
