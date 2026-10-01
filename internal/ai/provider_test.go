package ai

import (
	"context"
	"errors"
	"testing"
	"time"

	"coverletter/internal/apperror"
)

func TestDecodeStructured(t *testing.T) {
	t.Parallel()

	type payload struct {
		Title string   `json:"title"`
		Items []string `json:"items"`
	}

	tests := []struct {
		name    string
		text    string
		want    payload
		wantErr bool
	}{
		{
			name: "plain json",
			text: `{"title": "Backend", "items": ["a"]}`,
			want: payload{Title: "Backend", Items: []string{"a"}},
		},
		{
			name: "fenced json",
			text: "```json\n{\"title\": \"Backend\", \"items\": [\"a\"]}\n```",
			want: payload{Title: "Backend", Items: []string{"a"}},
		},
		{
			name: "fenced without language",
			text: "```\n{\"title\": \"Backend\", \"items\": []}\n```",
			want: payload{Title: "Backend", Items: []string{}},
		},
		{
			name: "leading prose",
			text: `Here is the analysis you asked for: {"title": "Backend", "items": ["a"]} — let me know if you need more.`,
			want: payload{Title: "Backend", Items: []string{"a"}},
		},
		{
			name: "nested objects",
			text: `{"title": "Backend", "items": ["a"], "meta": {"nested": {"deep": true}}}`,
			want: payload{Title: "Backend", Items: []string{"a"}},
		},
		{
			name: "braces inside strings do not break nesting",
			text: `{"title": "a { brace } in a string", "items": []}`,
			want: payload{Title: "a { brace } in a string", Items: []string{}},
		},
		{
			name: "escaped quotes inside strings",
			text: `{"title": "say \"hi\"", "items": []}`,
			want: payload{Title: `say "hi"`, Items: []string{}},
		},

		{name: "empty response", text: "", wantErr: true},
		{name: "whitespace only", text: "   \n  ", wantErr: true},
		{name: "no json object", text: "I cannot help with that.", wantErr: true},
		{name: "unterminated object", text: `{"title": "Backend"`, wantErr: true},
		{name: "wrong field type", text: `{"title": 42, "items": []}`, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var got payload
			err := DecodeStructured(tc.text, &got)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("DecodeStructured(%q) = nil error, want an error", tc.text)
				}
				return
			}
			if err != nil {
				t.Fatalf("DecodeStructured(%q) error = %v", tc.text, err)
			}
			if got.Title != tc.want.Title {
				t.Errorf("Title = %q, want %q", got.Title, tc.want.Title)
			}
			if len(got.Items) != len(tc.want.Items) {
				t.Fatalf("Items = %v, want %v", got.Items, tc.want.Items)
			}
		})
	}
}

func TestValidateRequiredFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		payload  map[string]any
		required []string
		wantErr  bool
	}{
		{name: "all present", payload: map[string]any{"a": 1, "b": 2}, required: []string{"a", "b"}},
		{name: "one missing", payload: map[string]any{"a": 1}, required: []string{"a", "b"}, wantErr: true},
		{name: "none required", payload: map[string]any{}, required: nil},
		{name: "nil payload", payload: nil, required: []string{"a"}, wantErr: true},
		{
			// A present-but-empty field satisfies the contract; emptiness is the
			// caller's business, not the decoder's.
			name:     "empty value is present",
			payload:  map[string]any{"a": ""},
			required: []string{"a"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := Validate(tc.payload, tc.required...)
			if tc.wantErr && err == nil {
				t.Fatal("Validate() = nil, want an error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Validate() error = %v, want nil", err)
			}
		})
	}
}

func TestIsRetryable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "rate limited", err: &nonRetryableError{}, want: false},
		{name: "nil", err: nil, want: false},
		{name: "context cancelled", err: context.Canceled, want: false},
		{name: "empty response is transient", err: ErrEmptyResponse, want: true},
		{name: "deadline exceeded", err: context.DeadlineExceeded, want: true},
		{name: "explicitly non-retryable", err: &nonRetryableError{err: errors.New("blocked")}, want: false},
		{name: "transport failure", err: errors.New("connection reset by peer"), want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := isRetryable(tc.err); got != tc.want {
				t.Errorf("isRetryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestBackoffIsBounded(t *testing.T) {
	t.Parallel()

	for attempt := 1; attempt <= 10; attempt++ {
		delay := backoff(attempt)
		if delay <= 0 {
			t.Fatalf("backoff(%d) = %v, want a positive delay", attempt, delay)
		}
		if delay > 8*time.Second {
			t.Fatalf("backoff(%d) = %v, want at most 8s", attempt, delay)
		}
	}
}

func TestNewGeminiProviderRequiresCredentials(t *testing.T) {
	t.Parallel()

	// A missing key or model is a configuration error and must surface at
	// startup, not on the first user request.
	if _, err := NewGeminiProvider(context.Background(), GeminiConfig{Model: "gemini-2.5-flash"}, nil, nil); !apperror.Is(err, apperror.CodeValidation) {
		t.Errorf("NewGeminiProvider() error = %v, want %s", err, apperror.CodeValidation)
	}
	if _, err := NewGeminiProvider(context.Background(), GeminiConfig{APIKey: "key"}, nil, nil); !apperror.Is(err, apperror.CodeValidation) {
		t.Errorf("NewGeminiProvider() error = %v, want %s", err, apperror.CodeValidation)
	}
}

func TestResponseFromGeminiRejectsEmptyCandidates(t *testing.T) {
	t.Parallel()

	if _, err := responseFromGemini(nil); !errors.Is(err, ErrEmptyResponse) {
		t.Errorf("responseFromGemini(nil) error = %v, want ErrEmptyResponse", err)
	}
}

func TestToGeminiSchema(t *testing.T) {
	t.Parallel()

	out := toGeminiSchema(&Schema{
		Type:     TypeObject,
		Required: []string{"a"},
		Properties: map[string]*Schema{
			"a": {Type: TypeArray, Items: &Schema{Type: TypeString}},
		},
	})
	if out == nil {
		t.Fatal("toGeminiSchema() = nil")
	}
	if out.Properties["a"].Items == nil {
		t.Error("nested item schema was not converted")
	}
	if len(out.Required) != 1 || out.Required[0] != "a" {
		t.Errorf("Required = %v, want [a]", out.Required)
	}
	if toGeminiSchema(nil) != nil {
		t.Error("toGeminiSchema(nil) should be nil")
	}
}
