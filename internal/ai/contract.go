package ai

// This file defines the typed contracts exchanged between the agent and the
// model. Every stage of the workflow is a structured object rather than prose,
// which makes the pipeline validatable, loggable without leaking content, and
// testable with plain table-driven fixtures.

// JobAnalysis is stage one: what the role actually requires.
type JobAnalysis struct {
	JobTitle          string   `json:"job_title"`
	Company           string   `json:"company"`
	Requirements      []string `json:"requirements"`
	Responsibilities  []string `json:"responsibilities"`
	ImportantKeywords []string `json:"important_keywords"`
}

// Claim is a single assertion about the candidate that is anchored to a quote
// from the resume. The Evidence field is the mechanism that makes hallucination
// detectable: a claim with no supporting quote is a claim the model invented.
type Claim struct {
	Claim    string `json:"claim"`
	Evidence string `json:"evidence"`
}

// ResumeAnalysis is stage two: what the candidate has actually done.
type ResumeAnalysis struct {
	CandidateName     string   `json:"candidate_name"`
	CurrentTitle      string   `json:"current_title"`
	CurrentCompany    string   `json:"current_company"`
	YearsOfExperience string   `json:"years_of_experience"`
	Summary           string   `json:"summary"`
	Highlights        []Claim  `json:"highlights"`
	Skills            []string `json:"skills"`
	Education         []string `json:"education"`
}

// MatchEntry links a job requirement to resume evidence.
type MatchEntry struct {
	Requirement string `json:"requirement"`
	Evidence    string `json:"evidence"`
}

// MatchResult is stage three: the honest overlap between resume and role.
type MatchResult struct {
	StrongMatches       []MatchEntry `json:"strong_matches"`
	PartialMatches      []MatchEntry `json:"partial_matches"`
	MissingRequirements []string     `json:"missing_requirements"`
	MatchedKeywords     []string     `json:"matched_keywords"`
}

// GeneratedLetter is stage four.
type GeneratedLetter struct {
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

// Validation is stage five: an independent check that the letter's claims are
// supported by the evidence the earlier stages produced.
type Validation struct {
	Valid             bool     `json:"valid"`
	UnsupportedClaims []string `json:"unsupported_claims"`
	Issues            []string `json:"issues"`
}

// ValidationRequest carries everything a validator may consult. It is
// deliberately given the evidence and the missing requirements, not just the
// letter, so it can adjudicate rather than guess.
type ValidationRequest struct {
	Letter GeneratedLetter
	Resume ResumeAnalysis
	Match  MatchResult
	Job    JobAnalysis
}

// Result is the agent's output for one cover letter.
type Result struct {
	Job        JobAnalysis
	Resume     ResumeAnalysis
	Match      MatchResult
	Letter     GeneratedLetter
	Validation Validation
	// Attempts is how many generate/validate cycles ran.
	Attempts int
}

// Valid reports whether the letter passed validation, both by the model and by
// the deterministic post-check.
func (r Result) Valid() bool { return r.Validation.Valid }

// jobAnalysisSchema constrains stage one.
var jobAnalysisSchema = &Schema{
	Type: TypeObject,
	Properties: map[string]*Schema{
		"job_title":          {Type: TypeString, Description: "The role title, or an empty string if unknown."},
		"company":            {Type: TypeString, Description: "The hiring company, or an empty string if unknown."},
		"requirements":       {Type: TypeArray, Items: &Schema{Type: TypeString}, Description: "Concrete, verifiable requirements from the posting."},
		"responsibilities":   {Type: TypeArray, Items: &Schema{Type: TypeString}, Description: "Concrete responsibilities from the posting."},
		"important_keywords": {Type: TypeArray, Items: &Schema{Type: TypeString}, Description: "Technologies and domain terms that matter for this role."},
	},
	Required: []string{"job_title", "company", "requirements", "responsibilities", "important_keywords"},
}

var claimSchema = &Schema{
	Type: TypeObject,
	Properties: map[string]*Schema{
		"claim":    {Type: TypeString, Description: "A factual statement about the candidate."},
		"evidence": {Type: TypeString, Description: "A verbatim quote from the resume supporting the claim. Empty string if the resume does not state it."},
	},
	Required: []string{"claim", "evidence"},
}

// resumeAnalysisSchema constrains stage two.
var resumeAnalysisSchema = &Schema{
	Type: TypeObject,
	Properties: map[string]*Schema{
		"candidate_name":      {Type: TypeString, Description: "The candidate's full name as written on the resume."},
		"current_title":       {Type: TypeString, Description: "The candidate's current or most recent job title."},
		"current_company":     {Type: TypeString, Description: "The candidate's current or most recent employer."},
		"years_of_experience": {Type: TypeString, Description: "Total years of professional experience, as a string such as '6'. Do not compute it; use only what the resume states."},
		"summary":             {Type: TypeString, Description: "Two or three sentences summarising the candidate's background."},
		"highlights":          {Type: TypeArray, Items: claimSchema, Description: "The most relevant achievements, each anchored to a resume quote."},
		"skills":              {Type: TypeArray, Items: &Schema{Type: TypeString}},
		"education":           {Type: TypeArray, Items: &Schema{Type: TypeString}},
	},
	Required: []string{"candidate_name", "current_title", "current_company", "years_of_experience", "summary", "highlights", "skills", "education"},
}

var matchEntrySchema = &Schema{
	Type: TypeObject,
	Properties: map[string]*Schema{
		"requirement": {Type: TypeString},
		"evidence":    {Type: TypeString, Description: "The resume quote or claim that satisfies this requirement. Empty string if none exists."},
	},
	Required: []string{"requirement", "evidence"},
}

// matchSchema constrains stage three.
var matchSchema = &Schema{
	Type: TypeObject,
	Properties: map[string]*Schema{
		"strong_matches":       {Type: TypeArray, Items: matchEntrySchema, Description: "Requirements directly and fully demonstrated by the resume."},
		"partial_matches":      {Type: TypeArray, Items: matchEntrySchema, Description: "Requirements with adjacent or transferable evidence, but not a direct match."},
		"missing_requirements": {Type: TypeArray, Items: &Schema{Type: TypeString}, Description: "Requirements the resume does not demonstrate at all. Be precise and do not soften these."},
		"matched_keywords":     {Type: TypeArray, Items: &Schema{Type: TypeString}},
	},
	Required: []string{"strong_matches", "partial_matches", "missing_requirements", "matched_keywords"},
}

// letterSchema constrains stage four.
var letterSchema = &Schema{
	Type: TypeObject,
	Properties: map[string]*Schema{
		"subject": {Type: TypeString, Description: "The letter subject line."},
		"body": {
			Type:        TypeString,
			Description: "The letter body as paragraphs separated by a blank line. No markdown, no headings, no bullet points.",
		},
	},
	Required: []string{"subject", "body"},
}

// validationSchema constrains stage five.
var validationSchema = &Schema{
	Type: TypeObject,
	Properties: map[string]*Schema{
		"valid":              {Type: TypeBoolean, Description: "True only if every claim in the letter is supported by the supplied evidence."},
		"unsupported_claims": {Type: TypeArray, Items: &Schema{Type: TypeString}, Description: "Sentences in the letter that assert something not supported by the evidence."},
		"issues":             {Type: TypeArray, Items: &Schema{Type: TypeString}, Description: "Other problems: tone, generic phrasing, excessive length, failure to address the role."},
	},
	Required: []string{"valid", "unsupported_claims", "issues"},
}
