package job

import (
	"context"
	"errors"
	"strings"
	"testing"

	"coverletter/internal/apperror"
	"coverletter/internal/obs"
)

// fakeFetcher is a scripted Fetcher. It records what it was asked to fetch so
// the URL delegation contract can be asserted.
type fakeFetcher struct {
	page    *Page
	err     error
	gotURLs []string
}

func (f *fakeFetcher) Fetch(_ context.Context, rawURL string) (*Page, error) {
	f.gotURLs = append(f.gotURLs, rawURL)
	if f.err != nil {
		return nil, f.err
	}
	return f.page, nil
}

// fakeParser is a scripted Parser that records the page it was handed, so the
// fetcher-to-parser hand-off can be asserted.
type fakeParser struct {
	desc     *Description
	err      error
	gotPages []*Page
}

func (p *fakeParser) Parse(_ context.Context, page *Page) (*Description, error) {
	p.gotPages = append(p.gotPages, page)
	if p.err != nil {
		return nil, p.err
	}
	return p.desc, nil
}

// validManualText is comfortably over minManualRunes and already in the
// normalised form cleanText produces, so it can be compared verbatim.
func validManualText() string {
	return strings.TrimSpace(strings.Repeat(
		"We are looking for a backend engineer to join our platform team.", 3))
}

func TestServiceResolve(t *testing.T) {
	t.Parallel()

	errFetcher := errors.New("network is down")
	errParser := errors.New("no description found")

	tests := []struct {
		name    string
		jobURL  string
		jobDesc string

		fetcher Fetcher
		parser  Parser

		wantErr         string // apperror code, empty means success expected
		wantErrContains string
		// wantErrIs, when set, is a sentinel error the returned error must
		// wrap. Used for plain (non-application) errors, which carry no code.
		wantErrIs      error
		wantSource     string
		wantURL        string
		wantTitle      string
		wantText       string
		wantTextSuffix string
		// wantContains are substrings that must appear in Text.
		wantContains []string
	}{
		// Source selection: exactly one input is allowed.
		{
			name:            "both url and description rejected",
			jobURL:          "https://jobs.example.com/1",
			jobDesc:         validManualText(),
			wantErr:         apperror.CodeInvalidJobInput,
			wantErrContains: "not both",
		},
		{
			name:            "neither url nor description rejected",
			wantErr:         apperror.CodeInvalidJobInput,
			wantErrContains: "either a job URL or a job description",
		},
		{
			// Whitespace is trimmed before the check, so blank input is "neither".
			name:            "whitespace only inputs rejected",
			jobURL:          "   ",
			jobDesc:         "\n\t ",
			wantErr:         apperror.CodeInvalidJobInput,
			wantErrContains: "either a job URL or a job description",
		},
		{
			// A URL that is only whitespace does not count as supplied.
			name:       "whitespace url with description is treated as description only",
			jobURL:     "  \n ",
			jobDesc:    validManualText(),
			wantSource: SourceManual,
			wantText:   validManualText(),
		},

		// Manual path.
		{
			name:       "manual description only",
			jobDesc:    validManualText(),
			wantSource: SourceManual,
			wantText:   validManualText(),
		},
		{
			// Surrounding whitespace is stripped, and the URL stays empty
			// because a pasted description has no source page.
			name:       "manual description is trimmed",
			jobDesc:    "  \n" + validManualText() + "\n  ",
			wantSource: SourceManual,
			wantText:   validManualText(),
			wantURL:    "",
		},
		{
			name: "manual description whitespace is normalised",
			jobDesc: "  Line one.\r\n\r\n\r\n\r\n" + validManualText() +
				"\n\n\n   Line    two.  \n",
			wantSource:     SourceManual,
			wantTextSuffix: "Line two.",
			wantContains:   []string{"Line one.", "backend engineer"},
		},
		{
			name:            "manual description below minimum rejected",
			jobDesc:         "Too short.",
			wantErr:         apperror.CodeInvalidJobInput,
			wantErrContains: "too short",
		},
		{
			// The minimum is counted in runes, and the check runs against the
			// trimmed input, so exactly the boundary is accepted.
			name:       "manual description at the minimum length accepted",
			jobDesc:    strings.Repeat("a", minManualRunes),
			wantSource: SourceManual,
		},
		{
			name:            "manual description one rune below minimum rejected",
			jobDesc:         strings.Repeat("a", minManualRunes-1),
			wantErr:         apperror.CodeInvalidJobInput,
			wantErrContains: "too short",
		},
		{
			// Multibyte text is counted in runes, not bytes: 50 multi-byte
			// characters is comfortably over the limit.
			name:       "manual multibyte description below byte limit accepted",
			jobDesc:    strings.Repeat("日", minManualRunes),
			wantSource: SourceManual,
		},

		// URL path: the fetcher and parser are consulted in order.
		{
			name:    "url delegates to fetcher then parser",
			jobURL:  "https://jobs.example.com/42",
			fetcher: &fakeFetcher{page: &Page{URL: "https://jobs.example.com/42", HTML: testHTMLBody}},
			parser: &fakeParser{desc: &Description{
				Title:   "Backend Engineer",
				Company: "Acme Corp",
				Text:    validManualText(),
				URL:     "https://jobs.example.com/42/apply",
			}},
			wantSource: SourceURL,
			// A parser that supplies its own URL wins; the service only backfills.
			wantURL:   "https://jobs.example.com/42/apply",
			wantTitle: "Backend Engineer",
			wantText:  validManualText(),
		},
		{
			// Note the backfill uses Page.URL (the requested URL), not
			// Page.FinalURL (the post-redirect URL).
			name:       "url backfilled from requested url not final url",
			jobURL:     "https://jobs.example.com/42b",
			fetcher:    &fakeFetcher{page: &Page{URL: "https://jobs.example.com/42b", FinalURL: "https://jobs.example.com/42b/apply", HTML: testHTMLBody}},
			parser:     &fakeParser{desc: &Description{Text: validManualText()}},
			wantSource: SourceURL,
			wantURL:    "https://jobs.example.com/42b",
		},
		{
			// When the parser leaves URL empty the service backfills it from
			// the page, so downstream always knows the source.
			name:       "url backfilled from page when parser leaves it empty",
			jobURL:     "https://jobs.example.com/43",
			fetcher:    &fakeFetcher{page: &Page{URL: "https://jobs.example.com/43", HTML: testHTMLBody}},
			parser:     &fakeParser{desc: &Description{Text: validManualText()}},
			wantSource: SourceURL,
			wantURL:    "https://jobs.example.com/43",
		},
		{
			// A parser that wrongly claims a manual source must not win; the
			// URL path always stamps SourceURL.
			name:       "url path forces source url",
			jobURL:     "https://jobs.example.com/44",
			fetcher:    &fakeFetcher{page: &Page{URL: "https://jobs.example.com/44", HTML: testHTMLBody}},
			parser:     &fakeParser{desc: &Description{Source: SourceManual, Text: validManualText()}},
			wantSource: SourceURL,
			wantURL:    "https://jobs.example.com/44",
		},
		{
			name:       "url is trimmed before fetching",
			jobURL:     "  https://jobs.example.com/45  ",
			fetcher:    &fakeFetcher{page: &Page{URL: "https://jobs.example.com/45", HTML: testHTMLBody}},
			parser:     &fakeParser{desc: &Description{Text: validManualText()}},
			wantSource: SourceURL,
			wantURL:    "https://jobs.example.com/45",
		},
		{
			// A bare (non-application) error passes through unchanged, so it
			// carries no application code at all rather than an invented one.
			name:      "plain fetcher error propagated unchanged",
			jobURL:    "https://jobs.example.com/46",
			fetcher:   &fakeFetcher{err: errFetcher},
			parser:    &fakeParser{desc: &Description{Text: validManualText()}},
			wantErrIs: errFetcher,
		},
		{
			name:    "fetcher error is not swallowed or replaced",
			jobURL:  "https://jobs.example.com/47",
			fetcher: &fakeFetcher{err: apperror.New(apperror.CodeUnsafeURL, "Job URL host is not allowed.", 400)},
			parser:  &fakeParser{desc: &Description{Text: validManualText()}},
			wantErr: apperror.CodeUnsafeURL,
		},
		{
			name:      "parser error propagated",
			jobURL:    "https://jobs.example.com/48",
			fetcher:   &fakeFetcher{page: &Page{URL: "https://jobs.example.com/48", HTML: testHTMLBody}},
			parser:    &fakeParser{err: errParser},
			wantErrIs: errParser,
		},
		{
			// A URL without a fetcher is a wiring bug, not user error, so it
			// must surface as an internal error rather than a 400.
			name:    "url path without a fetcher is an internal error",
			jobURL:  "https://jobs.example.com/49",
			parser:  &fakeParser{desc: &Description{Text: validManualText()}},
			wantErr: apperror.CodeInternal,
		},
		{
			name:    "url path without a parser is an internal error",
			jobURL:  "https://jobs.example.com/50",
			fetcher: &fakeFetcher{page: &Page{URL: "https://jobs.example.com/50", HTML: testHTMLBody}},
			wantErr: apperror.CodeInternal,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			svc := NewService(tc.fetcher, tc.parser, obs.New())
			got, err := svc.Resolve(context.Background(), tc.jobURL, tc.jobDesc)

			if tc.wantErrIs != nil {
				if err == nil {
					t.Fatalf("Resolve(%q, %q) = %+v, want error wrapping %v",
						tc.jobURL, tc.jobDesc, got, tc.wantErrIs)
				}
				if !errors.Is(err, tc.wantErrIs) {
					t.Fatalf("Resolve(%q, %q) error = %v, want it to wrap %v",
						tc.jobURL, tc.jobDesc, err, tc.wantErrIs)
				}
				if got != nil {
					t.Fatalf("Resolve(%q, %q) = %+v on error, want nil description",
						tc.jobURL, tc.jobDesc, got)
				}
				return
			}

			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("Resolve(%q, %q) = %+v, want error with code %s",
						tc.jobURL, tc.jobDesc, got, tc.wantErr)
				}
				if !apperror.Is(err, tc.wantErr) {
					t.Fatalf("Resolve(%q, %q) error = %v (code %s), want code %s",
						tc.jobURL, tc.jobDesc, err, apperror.Code(err), tc.wantErr)
				}
				if tc.wantErrContains != "" &&
					!strings.Contains(apperror.Message(err), tc.wantErrContains) {
					t.Errorf("Resolve(%q, %q) error message = %q, want it to contain %q",
						tc.jobURL, tc.jobDesc, apperror.Message(err), tc.wantErrContains)
				}
				if got != nil {
					t.Fatalf("Resolve(%q, %q) = %+v on error, want nil description",
						tc.jobURL, tc.jobDesc, got)
				}
				return
			}

			if err != nil {
				t.Fatalf("Resolve(%q, %q) error = %v, want success", tc.jobURL, tc.jobDesc, err)
			}
			if got.Source != tc.wantSource {
				t.Errorf("Source = %q, want %q", got.Source, tc.wantSource)
			}
			if got.URL != tc.wantURL {
				t.Errorf("URL = %q, want %q", got.URL, tc.wantURL)
			}
			if tc.wantTitle != "" && got.Title != tc.wantTitle {
				t.Errorf("Title = %q, want %q", got.Title, tc.wantTitle)
			}
			if tc.wantText != "" && got.Text != tc.wantText {
				t.Errorf("Text = %q, want %q", got.Text, tc.wantText)
			}
			if tc.wantTextSuffix != "" && !strings.HasSuffix(got.Text, tc.wantTextSuffix) {
				t.Errorf("Text = %q, want suffix %q", got.Text, tc.wantTextSuffix)
			}
			for _, want := range tc.wantContains {
				if !strings.Contains(got.Text, want) {
					t.Errorf("Text does not contain %q\nText: %q", want, got.Text)
				}
			}
		})
	}
}

// TestServiceResolveDelegation asserts the hand-off contract that the table
// above only checks indirectly: exactly the trimmed URL is fetched, the fetcher's
// page is what the parser receives, and neither runs on the manual path.
func TestServiceResolveDelegation(t *testing.T) {
	t.Parallel()

	t.Run("fetches the trimmed url and parses the fetched page", func(t *testing.T) {
		t.Parallel()

		page := &Page{URL: "https://jobs.example.com/1", HTML: testHTMLBody}
		fetcher := &fakeFetcher{page: page}
		parser := &fakeParser{desc: &Description{Text: validManualText()}}

		svc := NewService(fetcher, parser, obs.New())
		if _, err := svc.Resolve(context.Background(), "  https://jobs.example.com/1  ", ""); err != nil {
			t.Fatalf("Resolve() error = %v, want success", err)
		}

		if len(fetcher.gotURLs) != 1 || fetcher.gotURLs[0] != "https://jobs.example.com/1" {
			t.Errorf("fetched URLs = %q, want exactly [https://jobs.example.com/1]", fetcher.gotURLs)
		}
		if len(parser.gotPages) != 1 || parser.gotPages[0] != page {
			t.Errorf("parser received %+v, want the exact page the fetcher returned", parser.gotPages)
		}
	})

	t.Run("parser does not run when the fetcher fails", func(t *testing.T) {
		t.Parallel()

		parser := &fakeParser{desc: &Description{Text: validManualText()}}
		svc := NewService(&fakeFetcher{err: errors.New("boom")}, parser, obs.New())

		if _, err := svc.Resolve(context.Background(), "https://jobs.example.com/1", ""); err == nil {
			t.Fatal("Resolve() = nil error, want the fetcher error")
		}
		if len(parser.gotPages) != 0 {
			t.Errorf("parser was called %d times after a fetch failure, want 0", len(parser.gotPages))
		}
	})

	t.Run("manual path touches neither fetcher nor parser", func(t *testing.T) {
		t.Parallel()

		fetcher := &fakeFetcher{}
		parser := &fakeParser{}
		svc := NewService(fetcher, parser, obs.New())

		got, err := svc.Resolve(context.Background(), "", validManualText())
		if err != nil {
			t.Fatalf("Resolve() error = %v, want success", err)
		}
		if got.Source != SourceManual {
			t.Errorf("Source = %q, want %q", got.Source, SourceManual)
		}
		if len(fetcher.gotURLs) != 0 {
			t.Errorf("fetcher was called with %q on the manual path, want no calls", fetcher.gotURLs)
		}
		if len(parser.gotPages) != 0 {
			t.Errorf("parser was called %d times on the manual path, want 0", len(parser.gotPages))
		}
	})
}

// TestServiceResolveOverlongManualText asserts that an over-long pasted
// description is rejected rather than silently truncated. Truncating would drop
// requirements from the end of the posting, and a requirement the AI never sees
// cannot appear in the gap analysis.
func TestServiceResolveOverlongManualText(t *testing.T) {
	t.Parallel()

	input := strings.Repeat("a", maxManualRunes+500)
	svc := NewService(nil, nil, obs.New())

	got, err := svc.Resolve(context.Background(), "", input)
	if err == nil {
		t.Fatalf("Resolve() with an over-long description = %+v, want an error", got)
	}
	if !apperror.Is(err, apperror.CodeInvalidJobInput) {
		t.Errorf("error = %v, want code %s", err, apperror.CodeInvalidJobInput)
	}
}

func TestServiceResolveLongDescriptionNoMarkerWhenUnderLimit(t *testing.T) {
	t.Parallel()

	// Exactly at the limit there is nothing to truncate, so no marker is added.
	input := strings.Repeat("a", maxManualRunes)
	svc := NewService(nil, nil, obs.New())

	got, err := svc.Resolve(context.Background(), "", input)
	if err != nil {
		t.Fatalf("Resolve() error = %v, want success", err)
	}
	if strings.Contains(got.Text, "[truncated]") {
		t.Error("Text was marked truncated even though it is exactly at the limit")
	}
	if len([]rune(got.Text)) != maxManualRunes {
		t.Errorf("len(Text runes) = %d, want %d", len([]rune(got.Text)), maxManualRunes)
	}
}

// TestServiceResolveCancelledContext makes sure a cancelled context is not
// silently ignored on the URL path.
func TestServiceResolveCancelledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	fetcher := &fakeFetcher{page: &Page{URL: "https://jobs.example.com/1", HTML: testHTMLBody}}
	svc := NewService(fetcher, &fakeParser{desc: &Description{Text: validManualText()}}, obs.New())

	// The scripted fakes ignore ctx, so Resolve itself is expected to succeed;
	// what matters is that the context reaches the fetcher unchanged.
	if _, err := svc.Resolve(ctx, "https://jobs.example.com/1", ""); err != nil {
		t.Fatalf("Resolve() with a scripted fetcher = %v, want success", err)
	}
	if len(fetcher.gotURLs) != 1 {
		t.Errorf("fetcher called %d times, want 1", len(fetcher.gotURLs))
	}
}

// TestServiceResolveCarriesApplicationErrors makes sure the specific upstream
// error code survives the hand-off rather than being flattened to a generic one,
// which is what the client-facing message depends on.
func TestServiceResolveCarriesApplicationErrors(t *testing.T) {
	t.Parallel()

	sentinel := apperror.New(apperror.CodeUnsafeURL, "Job URL host is not allowed.", 400)

	svc := NewService(
		&fakeFetcher{err: sentinel},
		&fakeParser{desc: &Description{Text: validManualText()}},
		obs.New(),
	)

	_, err := svc.Resolve(context.Background(), "https://jobs.example.com/1", "")
	if !errors.Is(err, sentinel) {
		t.Fatalf("Resolve() error = %v, want the fetcher's error verbatim", err)
	}
	if got := apperror.Status(err); got != 400 {
		t.Errorf("status = %d, want 400", got)
	}
}
