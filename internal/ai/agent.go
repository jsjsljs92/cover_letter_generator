package ai

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"coverletter/internal/ai/prompts"
	"coverletter/internal/apperror"
	"coverletter/internal/job"
	"coverletter/internal/obs"
	"coverletter/internal/resume"
)

// Input is one cover letter generation request, expressed in domain terms. The
// agent never sees an HTTP request or a PDF path.
type Input struct {
	Job     job.Description
	Resume  resume.Resume
	Tone    string
	Company string
}

// Agent orchestrates the multi-stage generation workflow.
type Agent interface {
	Run(ctx context.Context, in Input) (*Result, error)
}

// AgentConfig tunes the generation loop.
type AgentConfig struct {
	// MaxAttempts caps generate/validate cycles. Each retry costs a full
	// generation call, so the default is a small number.
	MaxAttempts int
	// MaxResumeRunes bounds the resume text sent to the model.
	MaxResumeRunes int
	// MaxJobRunes bounds the job description sent to the model.
	MaxJobRunes int
	// DefaultTone applies when the caller supplies none.
	DefaultTone string
}

// agent implements the five-stage workflow:
//
//	job analysis -> resume analysis -> matching -> generation -> validation
//
// Generation and validation are looped: a rejected draft is regenerated with
// the validator's objections as feedback, bounded by MaxAttempts.
type agent struct {
	provider Provider
	config   AgentConfig
	metrics  *obs.Registry
	logger   *slog.Logger
}

// NewAgent builds the workflow agent.
func NewAgent(provider Provider, cfg AgentConfig, metrics *obs.Registry, logger *slog.Logger) Agent {
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 2
	}
	if cfg.MaxResumeRunes <= 0 {
		cfg.MaxResumeRunes = 12000
	}
	if cfg.MaxJobRunes <= 0 {
		cfg.MaxJobRunes = 12000
	}
	if cfg.DefaultTone == "" {
		cfg.DefaultTone = "professional"
	}
	return &agent{provider: provider, config: cfg, metrics: metrics, logger: logger}
}

// Prompt data structures. These mirror the templates' expectations; they are
// declared here rather than in the prompts package so the dependency direction
// stays one-way.
type jobPromptData struct {
	TitleHint   string
	CompanyHint string
	Text        string
}

type resumePromptData struct {
	Text string
}

type matchPromptData struct {
	Job    JobAnalysis
	Resume ResumeAnalysis
}

type letterPromptData struct {
	Job      JobAnalysis
	Resume   ResumeAnalysis
	Match    MatchResult
	Tone     string
	Feedback []string
}

type validatePromptData struct {
	Job    JobAnalysis
	Resume ResumeAnalysis
	Match  MatchResult
	Letter GeneratedLetter
}

// Run executes the workflow.
func (a *agent) Run(ctx context.Context, in Input) (*Result, error) {
	start := time.Now()
	defer func() { a.metrics.Observe("ai.workflow.duration", time.Since(start)) }()

	jobAnalysis, err := a.analyzeJob(ctx, in.Job)
	if err != nil {
		return nil, err
	}

	resumeAnalysis, err := a.analyzeResume(ctx, in.Resume)
	if err != nil {
		return nil, err
	}

	// The parsed page often knows the company; the model rarely can.
	if jobAnalysis.Company == "" && in.Job.Company != "" {
		jobAnalysis.Company = in.Job.Company
	}

	match, err := a.matchCandidate(ctx, jobAnalysis, resumeAnalysis)
	if err != nil {
		return nil, err
	}

	tone := strings.TrimSpace(in.Tone)
	if tone == "" {
		tone = a.config.DefaultTone
	}

	result := &Result{
		Job:    jobAnalysis,
		Resume: resumeAnalysis,
		Match:  match,
	}

	for attempt := 1; attempt <= a.config.MaxAttempts; attempt++ {
		result.Attempts = attempt

		letter, err := a.generateLetter(ctx, jobAnalysis, resumeAnalysis, match, tone, result.Validation.Issues)
		if err != nil {
			return nil, err
		}
		result.Letter = letter

		validation, err := a.validateLetter(ctx, jobAnalysis, resumeAnalysis, match, letter)
		if err != nil {
			return nil, err
		}
		// The deterministic check is an independent guard: it catches a
		// missing requirement mentioned in the letter even when the model
		// validator waves it through.
		validation = mergeValidation(validation, checkMissingRequirements(letter, match.MissingRequirements))
		result.Validation = validation

		if validation.Valid {
			a.metrics.Inc("ai.workflow.valid")
			a.logger.Info("cover letter generated",
				slog.Int("attempts", attempt),
				slog.Int("strong_matches", len(match.StrongMatches)),
				slog.Int("missing_requirements", len(match.MissingRequirements)),
				slog.Duration("duration", time.Since(start)),
			)
			return result, nil
		}

		a.metrics.Inc("ai.workflow.invalid")
		a.logger.Warn("cover letter failed validation",
			slog.Int("attempt", attempt),
			slog.Int("unsupported_claims", len(validation.UnsupportedClaims)),
			slog.Int("issues", len(validation.Issues)),
		)
	}

	// Every attempt failed validation. The letter is still returned, with the
	// objections attached, rather than failing the whole request: the user can
	// read the issues and decide. Silently retrying forever or discarding the
	// work would both be worse.
	a.logger.Warn("cover letter still invalid after all attempts",
		slog.Int("attempts", result.Attempts),
		slog.Int("issues", len(result.Validation.Issues)),
	)
	return result, nil
}

func (a *agent) analyzeJob(ctx context.Context, desc job.Description) (JobAnalysis, error) {
	var out JobAnalysis
	err := a.stage(ctx, prompts.AnalyzeJob, jobAnalysisSchema, jobPromptData{
		TitleHint:   desc.Title,
		CompanyHint: desc.Company,
		Text:        truncate(desc.Text, a.config.MaxJobRunes),
	}, &out, []string{"job_title", "requirements"})
	if err != nil {
		return JobAnalysis{}, err
	}

	// The page extraction is more trustworthy than the model for identity
	// fields, so the model's value is only used when the page had none.
	if out.JobTitle == "" {
		out.JobTitle = desc.Title
	}
	if len(out.Requirements) == 0 {
		return JobAnalysis{}, apperror.New(apperror.CodeAIInvalidOutput,
			"The job description could not be analysed.", 502)
	}
	return out, nil
}

func (a *agent) analyzeResume(ctx context.Context, res resume.Resume) (ResumeAnalysis, error) {
	var out ResumeAnalysis
	err := a.stage(ctx, prompts.AnalyzeResume, resumeAnalysisSchema, resumePromptData{
		Text: truncate(res.Text, a.config.MaxResumeRunes),
	}, &out, []string{"candidate_name", "highlights"})
	if err != nil {
		return ResumeAnalysis{}, err
	}

	// Drop highlights with no evidence: an unsupported claim is exactly the
	// failure mode this system exists to prevent, so it is removed here rather
	// than trusted onward to the matching and generation stages.
	supported := out.Highlights[:0]
	dropped := 0
	for _, h := range out.Highlights {
		if strings.TrimSpace(h.Evidence) == "" {
			dropped++
			continue
		}
		supported = append(supported, h)
	}
	out.Highlights = supported
	if dropped > 0 {
		a.metrics.Add("ai.resume.highlights_dropped", int64(dropped))
		a.logger.Warn("dropped resume claims without evidence", slog.Int("count", dropped))
	}

	if out.CandidateName == "" {
		out.CandidateName = res.Filename
	}
	return out, nil
}

func (a *agent) matchCandidate(ctx context.Context, jobAnalysis JobAnalysis, resumeAnalysis ResumeAnalysis) (MatchResult, error) {
	var out MatchResult
	err := a.stage(ctx, prompts.MatchCandidate, matchSchema, matchPromptData{
		Job:    jobAnalysis,
		Resume: resumeAnalysis,
	}, &out, []string{"strong_matches", "partial_matches", "missing_requirements"})
	if err != nil {
		return MatchResult{}, err
	}
	return out, nil
}

func (a *agent) generateLetter(ctx context.Context, jobAnalysis JobAnalysis, resumeAnalysis ResumeAnalysis, match MatchResult, tone string, feedback []string) (GeneratedLetter, error) {
	var out GeneratedLetter
	err := a.stage(ctx, prompts.GenerateLetter, letterSchema, letterPromptData{
		Job:      jobAnalysis,
		Resume:   resumeAnalysis,
		Match:    match,
		Tone:     tone,
		Feedback: feedback,
	}, &out, []string{"body"})
	if err != nil {
		return GeneratedLetter{}, err
	}

	out.Body = normalizeLetterBody(out.Body)
	if len([]rune(out.Body)) < 200 {
		return GeneratedLetter{}, apperror.New(apperror.CodeAIInvalidOutput,
			"The generated cover letter was too short to be usable.", 502)
	}
	return out, nil
}

func (a *agent) validateLetter(ctx context.Context, jobAnalysis JobAnalysis, resumeAnalysis ResumeAnalysis, match MatchResult, letter GeneratedLetter) (Validation, error) {
	var out Validation
	err := a.stage(ctx, prompts.ValidateLetter, validationSchema, validatePromptData{
		Job:    jobAnalysis,
		Resume: resumeAnalysis,
		Match:  match,
		Letter: letter,
	}, &out, []string{"valid"})
	if err != nil {
		return Validation{}, err
	}

	// Treat a verdict of "valid" alongside complaints as invalid; the two
	// disagreeing is itself a reason not to trust the letter.
	if out.Valid && (len(out.UnsupportedClaims) > 0 || len(out.Issues) > 0) {
		out.Valid = false
	}
	return out, nil
}

// stage runs one model call and decodes its structured output.
func (a *agent) stage(ctx context.Context, promptStage string, schema *Schema, data any, dst any, required []string) error {
	system, err := prompts.System(promptStage)
	if err != nil {
		return apperror.Internal(err, "The prompt for this stage could not be loaded.")
	}
	user, err := prompts.User(promptStage, data)
	if err != nil {
		return apperror.Internal(err, "The prompt for this stage could not be rendered.")
	}

	start := time.Now()
	resp, err := a.provider.Generate(ctx, GenerateRequest{
		Stage:        promptStage,
		SystemPrompt: system,
		Prompt:       user,
		Schema:       schema,
	})
	if err != nil {
		return err
	}
	a.metrics.Observe("ai.stage.duration."+promptStage, time.Since(start))

	// Decode into a generic map first so a structurally incomplete response is
	// reported as a model contract violation rather than a silent zero value.
	var fields map[string]any
	if err := DecodeStructured(resp.Text, &fields); err != nil {
		a.metrics.Inc("ai.stage.invalid_output." + promptStage)
		return apperror.Wrap(err, apperror.CodeAIInvalidOutput,
			"The AI provider returned a response that could not be parsed.", 502)
	}
	if err := Validate(fields, required...); err != nil {
		a.metrics.Inc("ai.stage.invalid_output." + promptStage)
		return apperror.Wrap(err, apperror.CodeAIInvalidOutput,
			"The AI provider returned an incomplete response.", 502)
	}
	if err := DecodeStructured(resp.Text, dst); err != nil {
		a.metrics.Inc("ai.stage.invalid_output." + promptStage)
		return apperror.Wrap(err, apperror.CodeAIInvalidOutput,
			"The AI provider returned a response that could not be parsed.", 502)
	}
	a.metrics.Inc("ai.stage.success." + promptStage)
	return nil
}

// checkMissingRequirements is a deterministic guard, independent of the model.
//
// If the letter mentions a requirement that the matching stage classified as
// unmet, the letter is claiming experience the candidate does not have. The
// match is done on the normalised requirement text, which keeps false positives
// low: a requirement is only flagged when the whole phrase appears.
func checkMissingRequirements(letter GeneratedLetter, missing []string) []string {
	haystack := normalizeForMatch(letter.Body + " " + letter.Subject)
	var found []string

	for _, requirement := range missing {
		requirement = strings.TrimSpace(requirement)
		if len([]rune(requirement)) < 8 {
			// Too short to match meaningfully; the model validator covers it.
			continue
		}
		needle := normalizeForMatch(requirement)
		if needle != "" && strings.Contains(haystack, needle) {
			found = append(found, requirement)
		}
	}
	return found
}

// normalizeForMatch lowercases text and reduces punctuation and whitespace to
// single spaces so that typographic differences between the requirement and
// the letter do not hide a real match.
func normalizeForMatch(s string) string {
	lower := strings.ToLower(s)
	var sb strings.Builder
	sb.Grow(len(lower))

	lastSpace := true
	for _, r := range lower {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			sb.WriteRune(r)
			lastSpace = false
		default:
			if !lastSpace {
				sb.WriteByte(' ')
				lastSpace = true
			}
		}
	}
	return strings.TrimSpace(sb.String())
}

// mergeValidation folds the deterministic findings into the model's verdict.
func mergeValidation(validation Validation, deterministic []string) Validation {
	for _, claim := range deterministic {
		already := false
		for _, existing := range validation.UnsupportedClaims {
			if existing == claim {
				already = true
				break
			}
		}
		if !already {
			validation.UnsupportedClaims = append(validation.UnsupportedClaims, claim)
		}
	}
	if len(deterministic) > 0 {
		validation.Valid = false
	}
	return validation
}

// normalizeLetterBody strips stray markdown fences and normalises line breaks.
// The model is instructed to return plain paragraphs; this keeps a stray fence
// from reaching the PDF.
func normalizeLetterBody(body string) string {
	trimmed := strings.TrimSpace(body)
	if strings.HasPrefix(trimmed, "```") {
		if idx := strings.IndexByte(trimmed, '\n'); idx >= 0 {
			trimmed = trimmed[idx+1:]
		}
		if idx := strings.LastIndex(trimmed, "```"); idx >= 0 {
			trimmed = trimmed[:idx]
		}
		trimmed = strings.TrimSpace(trimmed)
	}
	trimmed = strings.ReplaceAll(trimmed, "\r\n", "\n")
	trimmed = strings.ReplaceAll(trimmed, "\r", "\n")

	// Collapse runs of blank lines to a single paragraph break.
	paragraphs := strings.Split(trimmed, "\n\n")
	cleaned := make([]string, 0, len(paragraphs))
	for _, p := range paragraphs {
		if p = strings.TrimSpace(p); p != "" {
			cleaned = append(cleaned, p)
		}
	}
	return strings.Join(cleaned, "\n\n")
}

func truncate(s string, max int) string {
	runes := []rune(s)
	if max <= 0 || len(runes) <= max {
		return s
	}
	// Keep the head, which carries the role summary and requirements; the tail
	// of a posting is usually legal boilerplate.
	return string(runes[:max]) + "\n[content truncated]"
}

// String renders the letter for logging and PDF purposes without exposing the
// candidate's data more than necessary.
func (l GeneratedLetter) String() string {
	return fmt.Sprintf("subject=%q body_runes=%d", l.Subject, len([]rune(l.Body)))
}
