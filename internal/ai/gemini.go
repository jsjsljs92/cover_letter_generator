package ai

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"strings"
	"time"

	"google.golang.org/genai"

	"coverletter/internal/apperror"
	"coverletter/internal/obs"
)

// GeminiConfig configures the Gemini provider.
type GeminiConfig struct {
	APIKey          string
	Model           string
	Timeout         time.Duration
	MaxRetries      int
	Temperature     float32
	MaxOutputTokens int
}

// GeminiProvider implements Provider on top of the Gemini API.
type GeminiProvider struct {
	client    *genai.Client
	model     string
	timeout   time.Duration
	retries   int
	temp      float32
	maxTokens int
	metrics   *obs.Registry
	logger    *slog.Logger
}

// NewGeminiProvider builds a provider. The SDK client is created eagerly so a
// bad API key or network configuration fails at startup rather than on the
// first user request.
func NewGeminiProvider(ctx context.Context, cfg GeminiConfig, metrics *obs.Registry, logger *slog.Logger) (*GeminiProvider, error) {
	if cfg.APIKey == "" {
		return nil, apperror.Validation(apperror.CodeValidation, "An API key is required for the Gemini provider.")
	}
	if cfg.Model == "" {
		return nil, apperror.Validation(apperror.CodeValidation, "A model name is required for the Gemini provider.")
	}

	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  cfg.APIKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		// The SDK error can embed configuration detail; keep it out of the
		// message that would reach a client.
		return nil, apperror.Wrap(err, apperror.CodeAIProvider, "The AI provider could not be initialised.", 500)
	}

	return &GeminiProvider{
		client:    client,
		model:     cfg.Model,
		timeout:   cfg.Timeout,
		retries:   cfg.MaxRetries,
		temp:      cfg.Temperature,
		maxTokens: cfg.MaxOutputTokens,
		metrics:   metrics,
		logger:    logger,
	}, nil
}

// Name implements Provider.
func (p *GeminiProvider) Name() string { return "gemini" }

// Generate implements Provider.
func (p *GeminiProvider) Generate(ctx context.Context, req GenerateRequest) (*GenerateResponse, error) {
	// Bound every call independently: the worker's overall deadline may allow
	// several sequential model calls, each of which must also have its own cap.
	callCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	start := time.Now()
	resp, attempts, err := p.withRetry(callCtx, req)

	duration := time.Since(start)
	p.metrics.Inc("ai.requests.total")
	p.metrics.Add("ai.requests.attempts", int64(attempts))
	p.metrics.Observe("ai.request.duration", duration)
	if err != nil {
		p.metrics.Inc("ai.requests.failed")
	}

	attrs := []any{
		slog.String("stage", req.Stage),
		slog.String("model", p.model),
		slog.Int("attempts", attempts),
		slog.Duration("duration", duration),
	}
	if err != nil {
		p.logger.Error("ai request failed", append(attrs, slog.String("error", err.Error()))...)
	} else {
		p.logger.Debug("ai request completed", attrs...)
	}

	return resp, err
}

func (p *GeminiProvider) withRetry(ctx context.Context, req GenerateRequest) (*GenerateResponse, int, error) {
	maxAttempts := p.retries + 1
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		resp, err := p.call(ctx, req)
		if err == nil {
			return resp, attempt, nil
		}
		lastErr = err

		if !isRetryable(err) {
			return nil, attempt, err
		}
		if attempt == maxAttempts {
			break
		}

		// Exponential backoff with jitter, so a burst of concurrent retries
		// does not synchronise into another thundering herd.
		delay := backoff(attempt)
		p.logger.Warn("retrying ai request",
			slog.String("stage", req.Stage),
			slog.Int("attempt", attempt),
			slog.Duration("backoff", delay),
			slog.String("error", err.Error()),
		)

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, attempt, ctx.Err()
		case <-timer.C:
		}
	}

	p.metrics.Inc("ai.retries.exhausted")
	return nil, maxAttempts, p.wrap(lastErr, req.Stage)
}

func (p *GeminiProvider) call(ctx context.Context, req GenerateRequest) (*GenerateResponse, error) {
	config := &genai.GenerateContentConfig{
		// A system instruction keeps the behavioural rules separate from the
		// data, which measurably improves adherence on long documents.
		SystemInstruction: &genai.Content{
			Role:  genai.RoleUser,
			Parts: []*genai.Part{{Text: req.SystemPrompt}},
		},
	}
	// The SDK takes temperature by pointer to distinguish "unset" from "zero".
	temperature := p.temp
	if req.Temperature != nil {
		temperature = *req.Temperature
	}
	config.Temperature = &temperature
	if tokens := req.MaxOutputTokens; tokens > 0 {
		config.MaxOutputTokens = int32(tokens)
	} else if p.maxTokens > 0 {
		config.MaxOutputTokens = int32(p.maxTokens)
	}
	if req.Schema != nil {
		config.ResponseMIMEType = "application/json"
		config.ResponseSchema = toGeminiSchema(req.Schema)
	}

	contents := []*genai.Content{{
		Role:  genai.RoleUser,
		Parts: []*genai.Part{{Text: req.Prompt}},
	}}

	result, err := p.client.Models.GenerateContent(ctx, p.model, contents, config)
	if err != nil {
		return nil, err
	}
	return responseFromGemini(result)
}

func responseFromGemini(result *genai.GenerateContentResponse) (*GenerateResponse, error) {
	if result == nil || len(result.Candidates) == 0 {
		return nil, ErrEmptyResponse
	}
	candidate := result.Candidates[0]

	// A finish reason other than STOP means the text is absent or truncated;
	// treating a truncated JSON document as valid would produce silent
	// data loss downstream.
	if candidate.FinishReason != "" && candidate.FinishReason != genai.FinishReasonStop {
		if candidate.FinishReason == genai.FinishReasonSafety || candidate.FinishReason == genai.FinishReasonRecitation {
			return nil, &nonRetryableError{err: fmt.Errorf("ai: response blocked by provider safety filters")}
		}
		return nil, &nonRetryableError{err: fmt.Errorf("ai: response truncated (finish reason %s)", candidate.FinishReason)}
	}

	var sb strings.Builder
	for _, part := range candidate.Content.Parts {
		if part.Text != "" {
			sb.WriteString(part.Text)
		}
	}
	text := strings.TrimSpace(sb.String())
	if text == "" {
		return nil, ErrEmptyResponse
	}

	out := &GenerateResponse{
		Text:  text,
		Model: result.ModelVersion,
	}
	if result.UsageMetadata != nil {
		out.Usage.InputTokens = int(result.UsageMetadata.PromptTokenCount)
		out.Usage.OutputTokens = int(result.UsageMetadata.CandidatesTokenCount)
	}
	return out, nil
}

// nonRetryableError marks failures that will not improve with another attempt.
type nonRetryableError struct{ err error }

func (e *nonRetryableError) Error() string { return e.err.Error() }
func (e *nonRetryableError) Unwrap() error { return e.err }

// isRetryable classifies a provider failure.
//
// Only transport hiccups, rate limiting and server-side errors are retried.
// Retrying a bad request or an invalid key wastes quota and delays the real
// error, so those fail immediately.
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	var nonRetryable *nonRetryableError
	if errors.As(err, &nonRetryable) {
		return false
	}
	if errors.Is(err, ErrEmptyResponse) {
		// An empty response is often a transient truncation; one retry is
		// reasonable.
		return true
	}

	var apiErr *genai.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.Code {
		case 429: // resource exhausted
			return true
		case 500, 502, 503, 504: // transient upstream
			return true
		default:
			return false
		}
	}
	// Context deadline reached mid-flight is a timeout worth one more attempt.
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// Transport-level failures (connection reset, DNS blips) have no status
	// code and are worth retrying.
	return true
}

// backoff returns an exponentially increasing, jittered delay.
func backoff(attempt int) time.Duration {
	const base = 500 * time.Millisecond
	const max = 8 * time.Second

	exp := math.Pow(2, float64(attempt-1))
	delay := time.Duration(float64(base) * exp)
	if delay > max {
		delay = max
	}
	// Full jitter avoids synchronised retries across workers.
	jitter := time.Duration(rand.Int64N(int64(delay/2) + 1))
	return delay/2 + jitter
}

// wrap converts a provider error into an application error with a safe message.
func (p *GeminiProvider) wrap(err error, stage string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return apperror.Wrap(err, apperror.CodeAIProvider, "The AI provider timed out.", 504)
	}
	if errors.Is(err, context.Canceled) {
		return apperror.Wrap(err, apperror.CodeAIProvider, "The request was cancelled.", 499)
	}

	var apiErr *genai.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.Code {
		case 401, 403:
			p.logger.Error("ai provider rejected credentials", slog.Int("code", apiErr.Code))
			return apperror.Wrap(err, apperror.CodeAIProvider, "The AI provider rejected the configured credentials.", 502)
		case 429:
			return apperror.Wrap(err, apperror.CodeAIProvider, "The AI provider rate limit was exceeded. Please retry later.", 503)
		case 400:
			p.logger.Warn("ai provider rejected request", slog.String("stage", stage), slog.Int("code", apiErr.Code))
			return apperror.Wrap(err, apperror.CodeAIProvider, "The AI provider rejected the request.", 502)
		}
	}
	return apperror.Wrap(err, apperror.CodeAIProvider, "The AI provider request failed.", 502)
}

// toGeminiSchema converts the internal schema into the SDK representation.
func toGeminiSchema(s *Schema) *genai.Schema {
	if s == nil {
		return nil
	}
	out := &genai.Schema{
		Type:        genai.Type(s.Type),
		Description: s.Description,
		Enum:        s.Enum,
	}
	if s.Items != nil {
		out.Items = toGeminiSchema(s.Items)
	}
	if len(s.Properties) > 0 {
		out.Properties = make(map[string]*genai.Schema, len(s.Properties))
		for name, prop := range s.Properties {
			out.Properties[name] = toGeminiSchema(prop)
		}
	}
	if len(s.Required) > 0 {
		out.Required = s.Required
	}
	return out
}
