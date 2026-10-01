package job

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html/charset"

	"coverletter/internal/apperror"
	"coverletter/internal/obs"
)

// HTTPFetcher retrieves job postings over plain HTTP.
//
// It deliberately does *not* use a shared http.Client: the transport carries a
// guard-aware dialer, and reusing the default client would bypass the SSRF
// protections that are the whole point of this type.
type HTTPFetcher struct {
	client    *http.Client
	guard     *resolveGuard
	userAgent string
	maxBytes  int64
	metrics   *obs.Registry
	logger    *slog.Logger
}

// NewHTTPFetcher builds a fetcher with the SSRF guard wired into both the
// dialer and the redirect chain.
func NewHTTPFetcher(
	timeout time.Duration,
	maxRedirects int,
	maxBytes int64,
	userAgent string,
	allowPrivateNetworks bool,
	metrics *obs.Registry,
	logger *slog.Logger,
) *HTTPFetcher {
	guard := newResolveGuard(net.DefaultResolver, allowPrivateNetworks)

	transport := &http.Transport{
		DialContext:           guard.dialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: timeout,
		MaxIdleConns:          16,
		IdleConnTimeout:       30 * time.Second,
	}

	f := &HTTPFetcher{
		guard:     guard,
		userAgent: userAgent,
		maxBytes:  maxBytes,
		metrics:   metrics,
		logger:    logger,
	}

	f.client = &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return apperror.Validation(apperror.CodeJobExtraction, "Job URL redirected too many times.")
			}
			// Each hop is untrusted: a public host can redirect to 127.0.0.1, so
			// the same validation runs again for the new target.
			parsed, err := ValidateJobURL(req.URL.String())
			if err != nil {
				return err
			}
			return guard.checkHost(req.Context(), parsed.Hostname())
		},
	}
	return f
}

// Fetch implements Fetcher.
func (f *HTTPFetcher) Fetch(ctx context.Context, rawURL string) (*Page, error) {
	start := time.Now()
	defer func() { f.metrics.Observe("job.fetch.duration", time.Since(start)) }()

	page, err := f.fetch(ctx, rawURL)
	if err != nil {
		f.metrics.Inc("job.fetch.failed")
		return nil, err
	}
	f.metrics.Inc("job.fetch.succeeded")
	return page, nil
}

func (f *HTTPFetcher) fetch(ctx context.Context, rawURL string) (*Page, error) {
	parsed, err := ValidateJobURL(rawURL)
	if err != nil {
		return nil, err
	}
	if err := f.guard.checkHost(ctx, parsed.Hostname()); err != nil {
		f.metrics.Inc("job.fetch.blocked")
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, apperror.Wrap(err, apperror.CodeJobExtraction, "Could not build the job URL request.", 502)
	}
	req.Header.Set("User-Agent", f.userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("Accept-Language", "en")

	resp, err := f.client.Do(req)
	if err != nil {
		// An SSRF rejection or redirect error surfaces here wrapped in a
		// *url.Error; unwrap so the caller sees the specific reason.
		if appErr, ok := apperror.From(unwrapURLError(err)); ok {
			return nil, appErr
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, apperror.Wrap(err, apperror.CodeJobExtraction, "Timed out while loading the job URL.", 504)
		}
		return nil, apperror.Wrap(err, apperror.CodeJobExtraction, "Could not load the job URL.", 502)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, apperror.New(apperror.CodeJobExtraction,
			fmt.Sprintf("Job URL returned HTTP %d.", resp.StatusCode), 502)
	}

	contentType := resp.Header.Get("Content-Type")
	if err := checkContentType(contentType); err != nil {
		return nil, err
	}

	// Read one byte past the limit so an oversized page is rejected rather than
	// silently truncated into a half job description.
	body, err := io.ReadAll(io.LimitReader(resp.Body, f.maxBytes+1))
	if err != nil {
		return nil, apperror.Wrap(err, apperror.CodeJobExtraction, "Could not read the job URL response.", 502)
	}
	if int64(len(body)) > f.maxBytes {
		return nil, apperror.New(apperror.CodeJobExtraction, "The job page is too large to process.", 502)
	}

	decoded := decodeBody(body, contentType)

	f.logger.Debug("fetched job page",
		slog.String("url", parsed.Redacted()),
		slog.String("final_url", resp.Request.URL.Redacted()),
		slog.Int("bytes", len(decoded)),
	)

	return &Page{
		URL:      parsed.String(),
		FinalURL: resp.Request.URL.String(),
		HTML:     decoded,
	}, nil
}

// checkContentType rejects responses that are definitely not HTML, so a URL
// pointing at a PDF or an image fails with a clear message instead of producing
// garbage extraction downstream.
func checkContentType(contentType string) error {
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	switch mediaType {
	case "", "text/html", "application/xhtml+xml", "text/plain", "application/xml", "text/xml":
		return nil
	default:
		return apperror.New(apperror.CodeJobExtraction, "The job URL did not return an HTML page.", 502)
	}
}

// decodeBody honours the charset from Content-Type or the document itself. A
// mislabelled charset falls back to the raw bytes rather than failing the
// request, which is what most job pages need anyway.
func decodeBody(body []byte, contentType string) string {
	reader, err := charset.NewReader(bytes.NewReader(body), contentType)
	if err != nil {
		return string(body)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil {
		return string(body)
	}
	return string(decoded)
}

// unwrapURLError digs the application error out of the *url.Error that
// http.Client wraps around CheckRedirect and transport failures.
func unwrapURLError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return urlErr.Err
	}
	return err
}
