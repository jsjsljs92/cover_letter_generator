package job

import (
	"context"
	"fmt"
	"strings"

	"coverletter/internal/apperror"
	"coverletter/internal/obs"
)

// maxManualRunes bounds a manually pasted job description. The limit exists to
// protect the downstream model context window, not the server.
const maxManualRunes = 20000

// minManualRunes rejects a description too short to analyse.
const minManualRunes = 50

// Service resolves a job input — either a URL or pasted text — into a
// Description. It owns the branching between the two sources so callers do not
// have to.
type Service struct {
	fetcher Fetcher
	parser  Parser
	metrics *obs.Registry
	// guard performs the pre-flight SSRF check at submit time. The fetcher
	// keeps its own guard for the authoritative check at dial time.
	guard *resolveGuard
}

// NewService wires the fetcher and parser. Either may be nil when only manual
// descriptions are supported (used by tests).
func NewService(fetcher Fetcher, parser Parser, metrics *obs.Registry) *Service {
	return NewServiceWithGuard(fetcher, parser, metrics, false)
}

// NewServiceWithGuard is NewService with an explicit private-network policy.
//
// The policy must match the one the fetcher was built with. When private
// networks are permitted — only in tests — the pre-flight check is skipped, as
// is the check at dial time.
func NewServiceWithGuard(fetcher Fetcher, parser Parser, metrics *obs.Registry, allowPrivateNetworks bool) *Service {
	return &Service{
		fetcher: fetcher,
		parser:  parser,
		metrics: metrics,
		guard:   newResolveGuard(nil, allowPrivateNetworks),
	}
}

// ValidateURL reports whether a job URL is safe to accept, resolving the host
// and applying the SSRF rules.
//
// This exists so an unsafe URL is rejected when the request is submitted rather
// than minutes later in a background worker. The check is advisory, not
// authoritative: the fetcher re-validates at dial time, because a DNS answer can
// change between the two moments, and only the dial-time check closes the
// rebinding window.
func (s *Service) ValidateURL(ctx context.Context, raw string) error {
	parsed, err := ValidateJobURL(raw)
	if err != nil {
		return err
	}
	return s.guard.checkHost(ctx, parsed.Hostname())
}

// Resolve produces a job description from exactly one source.
//
// Exactly one of jobURL or jobDescription must be supplied; supplying both is
// rejected rather than silently preferring one, because a mismatch between
// them usually means the caller built the request wrong.
func (s *Service) Resolve(ctx context.Context, jobURL, jobDescription string) (*Description, error) {
	jobURL = strings.TrimSpace(jobURL)
	jobDescription = strings.TrimSpace(jobDescription)

	switch {
	case jobURL == "" && jobDescription == "":
		return nil, apperror.Validation(apperror.CodeInvalidJobInput,
			"Provide either a job URL or a job description.")
	case jobURL != "" && jobDescription != "":
		return nil, apperror.Validation(apperror.CodeInvalidJobInput,
			"Provide either a job URL or a job description, not both.")
	case jobURL != "":
		return s.resolveFromURL(ctx, jobURL)
	default:
		return resolveFromText(jobDescription)
	}
}

func (s *Service) resolveFromURL(ctx context.Context, jobURL string) (*Description, error) {
	if s.fetcher == nil || s.parser == nil {
		return nil, apperror.Internal(nil, "Job fetching is not configured.")
	}

	page, err := s.fetcher.Fetch(ctx, jobURL)
	if err != nil {
		return nil, err
	}

	desc, err := s.parser.Parse(ctx, page)
	if err != nil {
		return nil, err
	}
	desc.Source = SourceURL
	if desc.URL == "" {
		desc.URL = page.URL
	}
	return desc, nil
}

func resolveFromText(raw string) (*Description, error) {
	if len([]rune(raw)) < minManualRunes {
		return nil, apperror.Validation(apperror.CodeInvalidJobInput,
			"The job description is too short to analyse.")
	}
	// An over-long description is rejected rather than truncated. Truncating
	// would silently drop requirements from the end of the posting, and a
	// requirement the AI never saw is one it cannot include in the gap
	// analysis — so the candidate could be told to claim experience the job
	// actually asks for.
	text := cleanText(raw)
	if len([]rune(text)) > maxManualRunes {
		return nil, apperror.Validation(apperror.CodeInvalidJobInput,
			fmt.Sprintf("The job description is longer than the %d character limit; please trim it.", maxManualRunes))
	}
	return &Description{
		Source: SourceManual,
		Text:   text,
	}, nil
}
