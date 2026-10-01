package job

import "context"

// Page is the raw result of retrieving a job posting.
type Page struct {
	// URL is the URL originally requested.
	URL string
	// FinalURL is the URL after redirects.
	FinalURL string
	// HTML is the decoded response body.
	HTML string
}

// Description is the normalised job posting the rest of the system works on.
// It is deliberately vendor-neutral: the AI layer never sees raw HTML.
type Description struct {
	// Source is either "url" or "manual".
	Source string
	// URL is set when Source is "url".
	URL string
	// Title is the job title, when one could be determined.
	Title string
	// Company is the hiring organisation, when one could be determined.
	Company string
	// Text is the cleaned plain-text posting body.
	Text string
}

// Fetcher retrieves a job posting page. Implementations may use plain HTTP
// (the default) or, for JavaScript-rendered boards, a headless browser.
type Fetcher interface {
	Fetch(ctx context.Context, rawURL string) (*Page, error)
}

// Parser extracts a Description from a fetched Page.
type Parser interface {
	Parse(ctx context.Context, page *Page) (*Description, error)
}

// Source values for Description.Source.
const (
	SourceManual = "manual"
	SourceURL    = "url"
)
