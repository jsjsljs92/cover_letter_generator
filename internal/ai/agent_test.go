package ai_test

import (
	"context"
	"strings"
	"testing"

	"coverletter/internal/ai"
	"coverletter/internal/aitest"
	"coverletter/internal/apperror"
	"coverletter/internal/job"
	"coverletter/internal/logging"
	"coverletter/internal/obs"
	"coverletter/internal/resume"
)

func testInput() ai.Input {
	return ai.Input{
		Job: job.Description{
			Source:  job.SourceURL,
			URL:     "https://jobs.example.com/1",
			Title:   "Backend Engineer",
			Company: "Acme",
			Text:    "We are hiring a backend engineer. Requirements include experience with Go and Kubernetes.",
		},
		Resume: resume.Resume{
			Filename: "resume.pdf",
			Text:     "Ada Lovelace, Senior Engineer at Globex. Built payment services in Go for six years.",
		},
		Tone: "professional",
	}
}

func newTestAgent(provider ai.Provider, maxAttempts int) ai.Agent {
	return ai.NewAgent(provider, ai.AgentConfig{MaxAttempts: maxAttempts}, obs.New(), logging.Discard())
}

func TestAgentRunHappyPath(t *testing.T) {
	t.Parallel()

	provider := aitest.DefaultResponses()
	agent := newTestAgent(provider, 2)

	result, err := agent.Run(context.Background(), testInput())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.Job.JobTitle != "Backend Engineer" {
		t.Errorf("job title = %q, want %q", result.Job.JobTitle, "Backend Engineer")
	}
	if result.Resume.CandidateName != "Ada Lovelace" {
		t.Errorf("candidate name = %q, want %q", result.Resume.CandidateName, "Ada Lovelace")
	}
	if len(result.Match.StrongMatches) != 1 {
		t.Errorf("strong matches = %d, want 1", len(result.Match.StrongMatches))
	}
	if !result.Valid() {
		t.Errorf("Valid() = false, issues = %v", result.Validation.Issues)
	}
	if result.Letter.Body != aitest.LetterBody {
		t.Errorf("letter body was not passed through unchanged")
	}
	if result.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1: a valid first draft should not be regenerated", result.Attempts)
	}

	// Every stage must have been invoked with a schema, in order.
	wantStages := []string{
		aitest.StageAnalyzeJob,
		aitest.StageAnalyzeResume,
		aitest.StageMatchCandidate,
		aitest.StageGenerateLetter,
		aitest.StageValidateLetter,
	}
	calls := provider.Calls()
	if len(calls) != len(wantStages) {
		t.Fatalf("provider calls = %d, want %d", len(calls), len(wantStages))
	}
	for i, want := range wantStages {
		if calls[i].Stage != want {
			t.Errorf("call %d stage = %q, want %q", i, calls[i].Stage, want)
		}
		if !calls[i].HasSchema {
			t.Errorf("call %d (%s) had no schema; structured output is mandatory", i, calls[i].Stage)
		}
	}
}

func TestAgentRetriesAfterValidationFailure(t *testing.T) {
	t.Parallel()

	provider := aitest.DefaultResponses().
		On(aitest.StageValidateLetter,
			aitest.Response{Text: aitest.ValidationJSON(
				[]string{"Claims hands-on Kubernetes experience"},
				[]string{"Unsupported skill claim"},
			)},
			aitest.Response{Text: aitest.ValidationValidJSON},
		)
	agent := newTestAgent(provider, 2)

	result, err := agent.Run(context.Background(), testInput())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !result.Valid() {
		t.Fatalf("Valid() = false after a valid retry, issues = %v", result.Validation.Issues)
	}
	if result.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2", result.Attempts)
	}

	// The retry prompt must carry the validator's objections back to the model,
	// otherwise the second attempt has no reason to differ from the first.
	var retryPrompt string
	for _, call := range provider.Calls() {
		if call.Stage == aitest.StageGenerateLetter && strings.Contains(call.Prompt, "rejected by validation") {
			retryPrompt = call.Prompt
		}
	}
	if retryPrompt == "" {
		t.Fatal("the second generation attempt did not include validation feedback")
	}
	if !strings.Contains(retryPrompt, "Unsupported skill claim") {
		t.Errorf("retry prompt does not carry the validator's issues:\n%s", retryPrompt)
	}
}

func TestAgentReturnsLetterWhenEveryAttemptFailsValidation(t *testing.T) {
	t.Parallel()

	provider := aitest.DefaultResponses().
		On(aitest.StageValidateLetter,
			aitest.Response{Text: aitest.ValidationJSON([]string{"Invented metric"}, nil)},
			aitest.Response{Text: aitest.ValidationJSON([]string{"Still invented"}, nil)},
		)
	agent := newTestAgent(provider, 2)

	result, err := agent.Run(context.Background(), testInput())
	if err != nil {
		t.Fatalf("Run() error = %v; an unvalidatable letter should be returned, not fail the request", err)
	}
	if result.Valid() {
		t.Error("Valid() = true, want false")
	}
	if result.Letter.Body == "" {
		t.Error("letter body is empty; the user should still receive the draft with its issues")
	}
	if result.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2", result.Attempts)
	}
	if provider.CallsTo(aitest.StageGenerateLetter) != 2 {
		t.Errorf("generate calls = %d, want 2", provider.CallsTo(aitest.StageGenerateLetter))
	}
}

func TestAgentDeterministicMissingRequirementCheck(t *testing.T) {
	t.Parallel()

	// The model validator wrongly approves the letter, but it mentions a
	// requirement the match stage classified as unmet. The deterministic check
	// must catch it independently.
	body := aitest.LetterBody + "\n\nI have run production workloads on Hands-on experience operating Kubernetes in production for three years."
	provider := aitest.DefaultResponses().
		On(aitest.StageGenerateLetter, aitest.Response{Text: aitest.LetterJSON(body)})
	agent := newTestAgent(provider, 1)

	result, err := agent.Run(context.Background(), testInput())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Valid() {
		t.Fatal("Valid() = true, want false: the letter claims a missing requirement")
	}
	if len(result.Validation.UnsupportedClaims) == 0 {
		t.Fatal("UnsupportedClaims is empty, want the missing requirement to be recorded")
	}
	found := false
	for _, claim := range result.Validation.UnsupportedClaims {
		if strings.Contains(claim, "Kubernetes") {
			found = true
		}
	}
	if !found {
		t.Errorf("UnsupportedClaims = %v, want a Kubernetes claim", result.Validation.UnsupportedClaims)
	}
}

func TestAgentDropsResumeHighlightsWithoutEvidence(t *testing.T) {
	t.Parallel()

	resumeJSON := `{
		"candidate_name": "Ada Lovelace",
		"current_title": "Senior Engineer",
		"current_company": "Globex",
		"years_of_experience": "6",
		"summary": "Backend engineer.",
		"highlights": [
			{"claim": "Reduced p99 latency", "evidence": "cut p99 latency from 900ms to 120ms"},
			{"claim": "Operates Kubernetes clusters at scale", "evidence": ""}
		],
		"skills": ["Go"],
		"education": []
	}`

	provider := aitest.DefaultResponses().
		On(aitest.StageAnalyzeResume, aitest.Response{Text: resumeJSON})
	agent := newTestAgent(provider, 1)

	result, err := agent.Run(context.Background(), testInput())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(result.Resume.Highlights) != 1 {
		t.Fatalf("highlights = %d, want 1: the unsupported claim should be dropped", len(result.Resume.Highlights))
	}
	if result.Resume.Highlights[0].Evidence == "" {
		t.Error("the remaining highlight has no evidence")
	}
}

func TestAgentPropagatesProviderFailure(t *testing.T) {
	t.Parallel()

	provider := aitest.DefaultResponses().
		FailAll(aitest.StageMatchCandidate, apperror.New(apperror.CodeAIProvider, "The AI provider request failed.", 502))
	agent := newTestAgent(provider, 1)

	_, err := agent.Run(context.Background(), testInput())
	if err == nil {
		t.Fatal("Run() = nil error, want the provider failure to propagate")
	}
	if !apperror.Is(err, apperror.CodeAIProvider) {
		t.Fatalf("Run() error = %v, want code %s", err, apperror.CodeAIProvider)
	}
}

func TestAgentRejectsMalformedModelOutput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		stage string
		text  string
	}{
		{name: "not json", stage: aitest.StageAnalyzeJob, text: "I could not read that page."},
		{name: "truncated json", stage: aitest.StageAnalyzeJob, text: `{"job_title": "Backend`},
		{name: "missing required field", stage: aitest.StageAnalyzeJob, text: `{"job_title": "Backend Engineer"}`},
		{name: "wrong json type", stage: aitest.StageAnalyzeJob, text: `{"job_title": 42, "requirements": []}`},
		{name: "no requirements found", stage: aitest.StageAnalyzeJob, text: `{"job_title": "x", "company": "y", "requirements": [], "responsibilities": [], "important_keywords": []}`},
		{name: "letter too short", stage: aitest.StageGenerateLetter, text: `{"subject": "Hi", "body": "Too short."}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			provider := aitest.DefaultResponses().On(tc.stage, aitest.Response{Text: tc.text})
			agent := newTestAgent(provider, 1)

			_, err := agent.Run(context.Background(), testInput())
			if err == nil {
				t.Fatal("Run() = nil error, want malformed output to be rejected")
			}
			if !apperror.Is(err, apperror.CodeAIInvalidOutput) {
				t.Fatalf("Run() error = %v, want code %s", err, apperror.CodeAIInvalidOutput)
			}
		})
	}
}

func TestAgentFallsBackToPageTitle(t *testing.T) {
	t.Parallel()

	// The page extraction knows the title; the model returns an empty one.
	jobJSON := `{
		"job_title": "",
		"company": "",
		"requirements": ["Experience with Go"],
		"responsibilities": ["Build services"],
		"important_keywords": ["Go"]
	}`
	provider := aitest.DefaultResponses().On(aitest.StageAnalyzeJob, aitest.Response{Text: jobJSON})
	agent := newTestAgent(provider, 1)

	result, err := agent.Run(context.Background(), testInput())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Job.JobTitle != "Backend Engineer" {
		t.Errorf("job title = %q, want the page-extracted title", result.Job.JobTitle)
	}
	if result.Job.Company != "Acme" {
		t.Errorf("company = %q, want the page-extracted company", result.Job.Company)
	}
}
