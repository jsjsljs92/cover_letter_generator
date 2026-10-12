// Package ai contains the model-facing abstraction and the multi-stage agent
// that drives cover letter generation.
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"coverletter/internal/apperror"
	"coverletter/internal/obs"
	"log/slog"
)

// GroqConfig configures the Groq provider.
type GroqConfig struct {
	APIKey      string
	BaseURL     string
	Model       string
	Timeout     time.Duration
	MaxRetries  int
	Temperature float32
	MaxTokens   int
}

// GroqProvider implements Provider on top of the Groq API
// (OpenAI-compatible).
type GroqProvider struct {
	client    *http.Client
	baseURL   string
	model     string
	apiKey    string
	timeout   time.Duration
	retries   int
	temp      float32
	maxTokens int
	metrics   *obs.Registry
	logger    *slog.Logger
}

// groqRequest represents the OpenAI-compatible chat completion request.
type groqRequest struct {
	Model       string          `json:"model"`
	Messages    []groqMessage   `json:"messages"`
	Temperature *float32        `json:"temperature,omitempty"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	ResponseFormat *groqResponseFormat `json:"response_format,omitempty"`
}

type groqMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type groqResponseFormat struct {
	Type string `json:"type"`
}

// groqResponse represents the OpenAI-compatible chat completion response.
type groqResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []groqChoice   `json:"choices"`
	Usage   groqUsage      `json:"usage"`
}

type groqChoice struct {
	Index        int         `json:"index"`
	Message      groqMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type groqUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// groqError represents an error response from Groq.
type groqError struct {
	Error groqErrorDetail `json:"error"`
}

type groqErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// NewGroqProvider builds a Groq provider.
func NewGroqProvider(ctx context.Context, cfg GroqConfig, metrics *obs.Registry, logger *slog.Logger) (*GroqProvider, error) {
	if cfg.APIKey == "" {
		return nil, apperror.Validation(apperror.CodeValidation, "An API key is required for the Groq provider.")
	}
	if cfg.Model == "" {
		return nil, apperror.Validation(apperror.CodeValidation, "A model name is required for the Groq provider.")
	}

	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = "https://api.groq.com/openai/v1"
	}
	baseURL = strings.TrimSuffix(baseURL, "/")

	client := &http.Client{
		Timeout: cfg.Timeout,
	}

	return &GroqProvider{
		client:    client,
		baseURL:   baseURL,
		model:     cfg.Model,
		apiKey:    cfg.APIKey,
		timeout:   cfg.Timeout,
		retries:   cfg.MaxRetries,
		temp:      cfg.Temperature,
		maxTokens: cfg.MaxTokens,
		metrics:   metrics,
		logger:    logger,
	}, nil
}

// Name implements Provider.
func (p *GroqProvider) Name() string { return "groq" }

// Generate implements Provider.
func (p *GroqProvider) Generate(ctx context.Context, req GenerateRequest) (*GenerateResponse, error) {
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

func (p *GroqProvider) withRetry(ctx context.Context, req GenerateRequest) (*GenerateResponse, int, error) {
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

func (p *GroqProvider) call(ctx context.Context, req GenerateRequest) (*GenerateResponse, error) {
	messages := []groqMessage{
		{Role: "system", Content: req.SystemPrompt},
		{Role: "user", Content: req.Prompt},
	}

	requestBody := groqRequest{
		Model:    p.model,
		Messages: messages,
	}

	temperature := p.temp
	if req.Temperature != nil {
		temperature = *req.Temperature
	}
	requestBody.Temperature = &temperature

	if tokens := req.MaxOutputTokens; tokens > 0 {
		requestBody.MaxTokens = tokens
	} else if p.maxTokens > 0 {
		requestBody.MaxTokens = p.maxTokens
	}

	if req.Schema != nil {
		requestBody.ResponseFormat = &groqResponseFormat{Type: "json_object"}
	}

	bodyBytes, err := json.Marshal(requestBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.baseURL+"/chat/completions", bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

	httpResp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if httpResp.StatusCode != http.StatusOK {
		var apiErr groqError
		if err := json.Unmarshal(respBody, &apiErr); err == nil && apiErr.Error.Message != "" {
			return nil, &nonRetryableError{err: fmt.Errorf("groq: %s (code: %d)", apiErr.Error.Message, httpResp.StatusCode)}
		}
		return nil, &nonRetryableError{err: fmt.Errorf("groq: status %d: %s", httpResp.StatusCode, string(respBody))}
	}

	var result groqResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	if len(result.Choices) == 0 {
		return nil, ErrEmptyResponse
	}

	choice := result.Choices[0]

	if choice.FinishReason != "" && choice.FinishReason != "stop" {
		if choice.FinishReason == "content_filter" || choice.FinishReason == "safety" {
			return nil, &nonRetryableError{err: fmt.Errorf("ai: response blocked by provider safety filters")}
		}
		return nil, &nonRetryableError{err: fmt.Errorf("ai: response truncated (finish reason %s)", choice.FinishReason)}
	}

	text := strings.TrimSpace(choice.Message.Content)
	if text == "" {
		return nil, ErrEmptyResponse
	}

	out := &GenerateResponse{
		Text:  text,
		Model: result.Model,
	}
	if result.Usage.PromptTokens > 0 || result.Usage.CompletionTokens > 0 {
		out.Usage.InputTokens = result.Usage.PromptTokens
		out.Usage.OutputTokens = result.Usage.CompletionTokens
	}
	return out, nil
}

// wrap converts a provider error into an application error with a safe message.
func (p *GroqProvider) wrap(err error, stage string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return apperror.Wrap(err, apperror.CodeAIProvider, "The AI provider timed out.", 504)
	}
	if errors.Is(err, context.Canceled) {
		return apperror.Wrap(err, apperror.CodeAIProvider, "The request was cancelled.", 499)
	}

	errStr := err.Error()
	if strings.Contains(errStr, "401") || strings.Contains(errStr, "403") || strings.Contains(errStr, "unauthorized") || strings.Contains(errStr, "invalid_api_key") {
		p.logger.Error("ai provider rejected credentials", slog.String("error", errStr))
		return apperror.Wrap(err, apperror.CodeAIProvider, "The AI provider rejected the configured credentials.", 502)
	}
	if strings.Contains(errStr, "429") || strings.Contains(errStr, "rate limit") {
		return apperror.Wrap(err, apperror.CodeAIProvider, "The AI provider rate limit was exceeded. Please retry later.", 503)
	}
	if strings.Contains(errStr, "400") {
		p.logger.Warn("ai provider rejected request", slog.String("stage", stage), slog.String("error", errStr))
		return apperror.Wrap(err, apperror.CodeAIProvider, "The AI provider rejected the request.", 502)
	}
	if strings.Contains(errStr, "500") || strings.Contains(errStr, "502") || strings.Contains(errStr, "503") || strings.Contains(errStr, "504") {
		return apperror.Wrap(err, apperror.CodeAIProvider, "The AI provider is temporarily unavailable.", 503)
	}
	return apperror.Wrap(err, apperror.CodeAIProvider, "The AI provider request failed.", 502)
}