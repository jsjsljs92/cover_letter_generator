package job

import (
	"context"
	"encoding/json"
	"strings"

	"golang.org/x/net/html"

	"coverletter/internal/apperror"
)

// minDescriptionRunes is the shortest body we accept as a usable job
// description. Below this the extraction almost certainly grabbed navigation
// chrome or an error page, and feeding it to the model would produce a
// confidently wrong cover letter.
const minDescriptionRunes = 200

// HTMLParser turns a fetched page into a clean Description.
//
// It runs in two passes. First it looks for schema.org JSON-LD JobPosting data,
// which job boards publish and which is by far the most reliable signal. Failing
// that it falls back to heuristic DOM extraction: strip chrome, then pick the
// most plausible content container.
type HTMLParser struct {
	// MaxRunes truncates the extracted description. 0 disables truncation.
	MaxRunes int
}

// NewHTMLParser returns a parser that truncates descriptions to maxRunes.
func NewHTMLParser(maxRunes int) *HTMLParser {
	return &HTMLParser{MaxRunes: maxRunes}
}

// Parse implements Parser.
func (p *HTMLParser) Parse(ctx context.Context, page *Page) (*Description, error) {
	if page == nil || strings.TrimSpace(page.HTML) == "" {
		return nil, apperror.New(apperror.CodeJobExtraction, "The job page was empty.", 502)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	doc, err := html.Parse(strings.NewReader(page.HTML))
	if err != nil {
		return nil, apperror.Wrap(err, apperror.CodeJobExtraction, "Could not parse the job page HTML.", 502)
	}

	desc := &Description{
		Source: SourceURL,
		URL:    page.FinalURL,
	}
	if desc.URL == "" {
		desc.URL = page.URL
	}

	if posting := findJobPosting(doc); posting != nil {
		desc.Title = firstNonEmpty(stringField(posting, "title"), documentTitle(doc))
		desc.Company = hiringOrganizationName(posting)
		desc.Text = cleanText(stringField(posting, "description"))
	}

	// JSON-LD is often partial or absent, and the description element is
	// usually the highest-signal part of the page, so a DOM pass fills any
	// field JSON-LD did not provide.
	if len(desc.Text) < minDescriptionRunes {
		domTitle, domCompany, domText := extractFromDOM(doc)
		desc.Title = firstNonEmpty(desc.Title, domTitle)
		desc.Company = firstNonEmpty(desc.Company, domCompany)
		desc.Text = firstNonEmpty(desc.Text, domText)
	}

	if len([]rune(desc.Text)) < minDescriptionRunes {
		return nil, apperror.New(apperror.CodeJobExtraction,
			"Could not find a job description on that page.", 422)
	}

	if p.MaxRunes > 0 {
		desc.Text = truncateRunes(desc.Text, p.MaxRunes)
	}
	return desc, nil
}

// extractFromDOM returns the title, company and body text found by heuristic
// DOM inspection.
func extractFromDOM(doc *html.Node) (title, company, text string) {
	stripChrome(doc)

	body := findBody(doc)
	if body == nil {
		return "", "", ""
	}

	title = firstNonEmpty(
		firstTextOfBody(findFirst(body, hasTagName("h1"))),
		trimSiteSuffix(documentTitle(doc)),
	)
	company = firstNonEmpty(
		firstTextOfBody(findFirst(body, matchesToken("company", "employer", "organization"))),
		"",
	)

	container := findDescriptionContainer(body)
	if container == nil {
		container = body
	}
	return title, company, cleanText(textOf(container))
}

// descriptionSelectors are tried in order of decreasing confidence. Job boards
// overwhelmingly mark the posting body with one of these signals.
var descriptionSelectors = []func(*html.Node) bool{
	hasAttrValue("itemprop", "description"),
	hasTagName("article"),
	hasTagName("main"),
	matchesToken("jobdescription", "job-description", "jobposting", "job-posting", "jobcontent", "description"),
	matchesToken("content", "body"),
}

func findDescriptionContainer(body *html.Node) *html.Node {
	for _, match := range descriptionSelectors {
		if node := findFirst(body, match); node != nil {
			if len([]rune(cleanText(textOf(node)))) >= minDescriptionRunes {
				return node
			}
		}
	}
	return nil
}

// chromeTags never contain job description content.
var chromeTags = map[string]bool{
	"script": true, "style": true, "noscript": true, "svg": true,
	"nav": true, "header": true, "footer": true, "aside": true,
	"form": true, "iframe": true, "template": true, "canvas": true,
}

func stripChrome(n *html.Node) {
	var visit func(*html.Node)
	visit = func(node *html.Node) {
		for child := node.FirstChild; child != nil; {
			next := child.NextSibling
			if child.Type == html.ElementNode && chromeTags[strings.ToLower(child.Data)] {
				node.RemoveChild(child)
				// Advance before continuing. RemoveChild clears Parent but
				// leaves the pointer valid, so looping without this would visit
				// the detached node again and panic on the second removal.
				child = next
				continue
			}
			visit(child)
			child = next
		}
	}
	visit(n)
}

// blockTags force a line break in the extracted text.
var blockTags = map[string]bool{
	"p": true, "div": true, "section": true, "article": true, "main": true,
	"li": true, "ul": true, "ol": true, "br": true, "tr": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"blockquote": true, "pre": true, "table": true, "dd": true, "dt": true,
}

// textOf renders the visible text of a subtree, inserting line breaks around
// block-level elements so bullet lists and paragraphs survive extraction.
func textOf(n *html.Node) string {
	var sb strings.Builder
	var visit func(*html.Node)
	visit = func(node *html.Node) {
		switch node.Type {
		case html.TextNode:
			sb.WriteString(node.Data)
		case html.ElementNode:
			tag := strings.ToLower(node.Data)
			isBlock := blockTags[tag]
			if isBlock {
				sb.WriteByte('\n')
			}
			for child := node.FirstChild; child != nil; child = child.NextSibling {
				visit(child)
			}
			if isBlock {
				sb.WriteByte('\n')
			}
			return
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
	}
	visit(n)
	return sb.String()
}

// cleanText normalises whitespace and strips control characters so the model
// sees a compact, readable posting.
func cleanText(raw string) string {
	if raw == "" {
		return ""
	}
	var sb strings.Builder
	sb.Grow(len(raw))

	spaceRun := 0
	newlineRun := 0
	// A CRLF pair is one line break, not two, so the carriage return is
	// swallowed rather than counted.
	pendingCR := false
	for _, r := range raw {
		if pendingCR {
			pendingCR = false
			if r == '\n' {
				continue
			}
		}
		switch {
		case r == '\r':
			pendingCR = true
			spaceRun = 0
			newlineRun++
			if newlineRun <= 2 {
				sb.WriteByte('\n')
			}
		case r == '\n':
			spaceRun = 0
			newlineRun++
			if newlineRun <= 2 {
				sb.WriteByte('\n')
			}
		case r == ' ' || r == '\t' || r == '\v' || r == '\f' || r == 0xA0:
			newlineRun = 0
			spaceRun++
			if spaceRun <= 1 {
				sb.WriteByte(' ')
			}
		case r < 0x20 || r == 0x7F:
			// Drop other control characters entirely.
		default:
			newlineRun = 0
			spaceRun = 0
			sb.WriteRune(r)
		}
	}
	return strings.TrimSpace(sb.String())
}

// findJobPosting looks for a schema.org JobPosting inside any JSON-LD block.
func findJobPosting(doc *html.Node) map[string]any {
	var found map[string]any

	var visit func(*html.Node)
	visit = func(node *html.Node) {
		if found != nil {
			return
		}
		if node.Type == html.ElementNode && strings.EqualFold(node.Data, "script") {
			if strings.EqualFold(attr(node, "type"), "application/ld+json") {
				var decoded any
				if err := json.Unmarshal([]byte(scriptContent(node)), &decoded); err == nil {
					if posting := findPostingNode(decoded); posting != nil {
						found = posting
						return
					}
				}
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
	}
	visit(doc)
	return found
}

// findPostingNode walks a decoded JSON-LD value, which may be an object, an
// array, or a @graph container, looking for a JobPosting.
func findPostingNode(value any) map[string]any {
	switch v := value.(type) {
	case map[string]any:
		if typeMatchesJobPosting(v) {
			return v
		}
		// @graph is the usual wrapper for multi-entity schemas.
		if graph, ok := v["@graph"]; ok {
			if posting := findPostingNode(graph); posting != nil {
				return posting
			}
		}
		for _, key := range []string{"mainEntity", "itemListElement", "item"} {
			if nested, ok := v[key]; ok {
				if posting := findPostingNode(nested); posting != nil {
					return posting
				}
			}
		}
	case []any:
		for _, item := range v {
			if posting := findPostingNode(item); posting != nil {
				return posting
			}
		}
	}
	return nil
}

func typeMatchesJobPosting(obj map[string]any) bool {
	switch t := obj["@type"].(type) {
	case string:
		return strings.EqualFold(t, "JobPosting")
	case []any:
		for _, item := range t {
			if s, ok := item.(string); ok && strings.EqualFold(s, "JobPosting") {
				return true
			}
		}
	}
	return false
}

func hiringOrganizationName(posting map[string]any) string {
	org, ok := posting["hiringOrganization"].(map[string]any)
	if !ok {
		return ""
	}
	return firstNonEmpty(stringField(org, "name"), stringField(org, "legalName"))
}

func stringField(obj map[string]any, key string) string {
	switch v := obj[key].(type) {
	case string:
		return strings.TrimSpace(v)
	case []any:
		// Some producers emit a one-element array.
		if len(v) > 0 {
			if s, ok := v[0].(string); ok {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

func documentTitle(doc *html.Node) string {
	if node := findFirst(doc, hasTagName("title")); node != nil {
		return firstTextOfBody(node)
	}
	return ""
}

// trimSiteSuffix strips the trailing site name from a <title>, e.g.
// "Senior Engineer - Acme Careers" becomes "Senior Engineer".
func trimSiteSuffix(title string) string {
	for _, sep := range []string{" | ", " - ", " — ", " – ", " :: ", " » "} {
		if idx := strings.Index(title, sep); idx > 0 {
			return strings.TrimSpace(title[:idx])
		}
	}
	return title
}

func firstTextOfBody(node *html.Node) string {
	if node == nil {
		return ""
	}
	return strings.TrimSpace(cleanText(textOf(node)))
}

func findBody(doc *html.Node) *html.Node {
	return findFirst(doc, hasTagName("body"))
}

// findFirst performs a depth-first search for the first matching element.
func findFirst(root *html.Node, match func(*html.Node) bool) *html.Node {
	if root == nil {
		return nil
	}
	if root.Type == html.ElementNode && match(root) {
		return root
	}
	for child := root.FirstChild; child != nil; child = child.NextSibling {
		if found := findFirst(child, match); found != nil {
			return found
		}
	}
	return nil
}

func hasTagName(tag string) func(*html.Node) bool {
	return func(n *html.Node) bool {
		return n.Type == html.ElementNode && strings.EqualFold(n.Data, tag)
	}
}

func hasAttrValue(attrName, value string) func(*html.Node) bool {
	return func(n *html.Node) bool {
		return n.Type == html.ElementNode && strings.EqualFold(attr(n, attrName), value)
	}
}

// matchesToken matches an id or class attribute against any of the given
// tokens, allowing both "job-description" and "jobDescription" spellings.
func matchesToken(tokens ...string) func(*html.Node) bool {
	return func(n *html.Node) bool {
		if n.Type != html.ElementNode {
			return false
		}
		for _, attrName := range []string{"id", "class"} {
			value := strings.ToLower(attr(n, attrName))
			if value == "" {
				continue
			}
			compact := compactToken(value)
			for _, token := range tokens {
				if strings.Contains(compact, token) {
					return true
				}
			}
		}
		return false
	}
}

// compactToken strips every non-alphanumeric character so that
// "job-description", "job_description" and "jobDescription" all collapse to
// the same needle.
func compactToken(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// scriptContent returns the text of a <script> element, which is stored as its
// text child rather than as attributes.
func scriptContent(n *html.Node) string {
	var sb strings.Builder
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == html.TextNode {
			sb.WriteString(child.Data)
		}
	}
	return strings.TrimSpace(sb.String())
}

func attr(n *html.Node, name string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, name) {
			return a.Val
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return strings.TrimRight(string(runes[:max]), " \n\t,;:-") + "\n[truncated]"
}
