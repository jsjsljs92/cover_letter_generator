// Package aitest provides a scripted AI provider for tests.
//
// It lives outside _test.go files on purpose: several packages need to drive
// the generation workflow end to end without a real model, and duplicating that
// fake in each of them would be worse than one shared, well-documented helper.
package aitest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"coverletter/internal/ai"
)

// Response is one scripted reply from the provider.
type Response struct {
	// Text is the raw response body, typically JSON.
	Text string
	// Err, when set, is returned instead of a response.
	Err error
}

// Call records one invocation.
type Call struct {
	Stage         string
	Prompt        string
	HasSchema     bool
	SchemaTypeSet string
}

// Provider is an ai.Provider that returns pre-programmed responses.
//
// Responses are keyed by stage name and consumed in order; the final entry for
// a stage repeats for any further calls. That makes it straightforward to
// script "fail validation once, then succeed".
type Provider struct {
	mu        sync.Mutex
	responses map[string][]Response
	used      map[string]int
	calls     []Call
	err       error
}

// New creates an empty provider.
func New() *Provider {
	return &Provider{
		responses: make(map[string][]Response),
		used:      make(map[string]int),
	}
}

// On scripts the responses for a stage, consumed in order.
//
// Scripting prepends, so a test can start from DefaultResponses and override a
// single stage without restating the others.
func (p *Provider) On(stage string, responses ...Response) *Provider {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.responses[stage] = append(append([]Response(nil), responses...), p.responses[stage]...)
	return p
}

// Fail scripts err for the next `times` calls to a stage.
func (p *Provider) Fail(stage string, times int, err error) *Provider {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry := Response{Err: err}
	failures := make([]Response, 0, times)
	for i := 0; i < times; i++ {
		failures = append(failures, entry)
	}
	p.responses[stage] = append(failures, p.responses[stage]...)
	return p
}

// FailAll scripts err for every call to a stage. The stage's queue is replaced
// outright, and because the final scripted entry repeats, the failure persists
// for every call.
func (p *Provider) FailAll(stage string, err error) *Provider {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.responses[stage] = []Response{{Err: err}}
	return p
}

// SetError makes every call fail, for testing provider-wide failures.
func (p *Provider) SetError(err error) *Provider {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.err = err
	return p
}

// Calls returns the recorded invocations.
func (p *Provider) Calls() []Call {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Call(nil), p.calls...)
}

// CallsTo returns how many times a stage was invoked.
func (p *Provider) CallsTo(stage string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	count := 0
	for _, c := range p.calls {
		if c.Stage == stage {
			count++
		}
	}
	return count
}

// Name implements ai.Provider.
func (p *Provider) Name() string { return "scripted" }

// Generate implements ai.Provider.
//
// Scripted responses are consumed in order; once the queue for a stage is
// exhausted the final entry repeats, so scripting "fail once, then succeed"
// needs only two entries.
func (p *Provider) Generate(_ context.Context, req ai.GenerateRequest) (*ai.GenerateResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.calls = append(p.calls, Call{
		Stage:     req.Stage,
		Prompt:    req.Prompt,
		HasSchema: req.Schema != nil,
	})

	if p.err != nil {
		return nil, p.err
	}

	queue := p.responses[req.Stage]
	if len(queue) == 0 {
		return nil, fmt.Errorf("aitest: no scripted response for stage %q", req.Stage)
	}

	idx := p.used[req.Stage]
	if idx >= len(queue) {
		idx = len(queue) - 1
	} else {
		p.used[req.Stage] = idx + 1
	}

	resp := queue[idx]
	if resp.Err != nil {
		return nil, resp.Err
	}
	return &ai.GenerateResponse{
		Text:     resp.Text,
		Model:    "scripted-model",
		Attempts: 1,
	}, nil
}

var _ ai.Provider = (*Provider)(nil)

// Stage names, mirrored here so tests do not need to import the prompts package
// for a string literal.
const (
	StageAnalyzeJob     = "analyze_job"
	StageAnalyzeResume  = "analyze_resume"
	StageMatchCandidate = "match_candidate"
	StageGenerateLetter = "generate_letter"
	StageValidateLetter = "validate_letter"
)

// LetterBody is a plausible generated letter. It deliberately mentions no
// missing requirement, so the deterministic grounding check passes.
const LetterBody = `Dear Hiring Team,

I am writing to apply for the Backend Engineer role at Acme. For the past six years I have built and operated backend services in Go, most recently as a Senior Engineer at Globex, where I designed the payment services that still process the majority of the company's transaction volume.

The part of your posting that resonated most was the emphasis on reliability under load. In my current role I took a p99 latency problem in the order pipeline from 900ms to 120ms by introducing a bounded worker pool and moving the fan-out to a queue, which also removed a recurring on-call page we had been carrying for two quarters. Before that I led the migration of a legacy reporting stack to PostgreSQL with staged cutovers and no customer-visible downtime.

I work comfortably across the whole lifecycle: designing the interface, writing the Go, instrumenting it, and being on call for it. I am particularly interested in your team's work on developer experience, because that is the part of the job I find most satisfying and the area where I have most recently been investing my own time.

Thank you for taking the time to read my application. I would welcome the chance to talk about how the work I have described maps onto what you are building.

Kind regards,
Ada Lovelace`

// DefaultResponses scripts a fully successful workflow.
func DefaultResponses() *Provider {
	return New().
		On(StageAnalyzeJob, Response{Text: JobAnalysisJSON}).
		On(StageAnalyzeResume, Response{Text: ResumeAnalysisJSON}).
		On(StageMatchCandidate, Response{Text: MatchJSON}).
		On(StageGenerateLetter, Response{Text: LetterJSON(LetterBody)}).
		On(StageValidateLetter, Response{Text: ValidationValidJSON})
}

// Well-formed responses for each stage.
const (
	JobAnalysisJSON = `{
		"job_title": "Backend Engineer",
		"company": "Acme",
		"requirements": ["Experience with Go", "Experience with Kubernetes"],
		"responsibilities": ["Build and operate backend services"],
		"important_keywords": ["Go", "Kubernetes", "PostgreSQL"]
	}`

	ResumeAnalysisJSON = `{
		"candidate_name": "Ada Lovelace",
		"current_title": "Senior Engineer",
		"current_company": "Globex",
		"years_of_experience": "6",
		"summary": "Backend engineer focused on reliable Go services.",
		"highlights": [
			{"claim": "Reduced p99 latency from 900ms to 120ms", "evidence": "cut p99 latency from 900ms to 120ms"}
		],
		"skills": ["Go", "PostgreSQL"],
		"education": ["BSc Computer Science"]
	}`

	MatchJSON = `{
		"strong_matches": [{"requirement": "Experience with Go", "evidence": "Built payment services in Go"}],
		"partial_matches": [{"requirement": "Experience with Kubernetes", "evidence": "ran a side project with kind"}],
		"missing_requirements": ["Hands-on experience operating Kubernetes in production"],
		"matched_keywords": ["Go", "PostgreSQL"]
	}`

	ValidationValidJSON = `{"valid": true, "unsupported_claims": [], "issues": []}`
)

// LetterJSON wraps a body in the generate_letter response shape.
func LetterJSON(body string) string {
	return fmt.Sprintf(`{"subject": "Application for Backend Engineer", "body": %s}`, quote(body))
}

// ValidationJSON builds a rejecting validation verdict.
func ValidationJSON(claims, issues []string) string {
	quotedClaims := make([]string, 0, len(claims))
	for _, c := range claims {
		quotedClaims = append(quotedClaims, quote(c))
	}
	quotedIssues := make([]string, 0, len(issues))
	for _, i := range issues {
		quotedIssues = append(quotedIssues, quote(i))
	}
	return fmt.Sprintf(`{"valid": false, "unsupported_claims": [%s], "issues": [%s]}`,
		strings.Join(quotedClaims, ", "), strings.Join(quotedIssues, ", "))
}

func quote(s string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`)
	return `"` + replacer.Replace(s) + `"`
}

// ErrProvider is a convenient test error.
var ErrProvider = errors.New("aitest: provider failure")
