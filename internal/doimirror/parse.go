package doimirror

import (
	"bytes"
	"net/url"
	"regexp"
	"strings"

	xhtml "golang.org/x/net/html"

	"github.com/bjoernaagaard/aakb/internal/httpx"
)

// ViewerPage holds the PDF target and optional title extracted from one DOI
// mirror viewer page.
type ViewerPage struct {
	PDFURL string
	Title  string
}

var locationHrefPattern = regexp.MustCompile(`location\.href\s*=\s*['"]([^'"]+)['"]`)

// IsChallengeHTML reports whether body looks like an anti-bot interstitial,
// JavaScript challenge, or interactive CAPTCHA page.
func IsChallengeHTML(body []byte) bool {
	lower := strings.ToLower(string(body))
	for _, marker := range []string{
		"_cf_chl_opt",
		"/cdn-cgi/challenge-platform",
		"<title>just a moment",
		"check.ddos-guard.net",
		"ddos-guard/js-challenge",
		"are you are robot",
		"are you a robot",
		"проверка на робота",
		`id="captcha"`,
		`name="captcha"`,
		"g-recaptcha",
		"h-captcha",
		"cf-turnstile",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func looksLikeMirrorLanding(body []byte) bool {
	root, err := xhtml.Parse(bytes.NewReader(body))
	if err != nil {
		return false
	}
	hasForm := false
	var walk func(*xhtml.Node)
	walk = func(n *xhtml.Node) {
		if hasForm || n == nil {
			return
		}
		if n.Type == xhtml.ElementNode && (n.Data == "form" || n.Data == "input") {
			hasForm = true
			return
		}
		for child := range n.ChildNodes() {
			walk(child)
		}
	}
	walk(root)
	return hasForm
}

// ParseViewerPage parses one DOI mirror HTML page, detecting anti-bot
// challenges before extracting the highest-priority public HTTPS PDF URL.
func ParseViewerPage(pageURL string, body []byte) (ViewerPage, error) {
	page, err := url.Parse(strings.TrimSpace(pageURL))
	if err != nil || httpx.ValidatePublicHTTPSURL(page) != nil {
		return ViewerPage{}, httpx.NewError(httpx.CodeUpstream, "invalid_mirror_page_url", "DOI mirror returned an invalid page URL")
	}
	if IsChallengeHTML(body) {
		return ViewerPage{}, httpx.NewError(httpx.CodeUpstreamBlocked, "mirror_challenged", "DOI mirror returned an anti-bot or CAPTCHA challenge")
	}
	root, err := xhtml.Parse(bytes.NewReader(body))
	if err != nil {
		return ViewerPage{}, httpx.WrapError(httpx.CodeUpstream, "invalid_html", "DOI mirror returned a page this version cannot read", err)
	}

	var priority [4][]string
	var titleText, citationText string

	var walk func(*xhtml.Node)
	walk = func(n *xhtml.Node) {
		if n == nil {
			return
		}
		if n.Type == xhtml.ElementNode {
			switch n.Data {
			case "title":
				if titleText == "" {
					titleText = cleanText(nodeText(n))
				}
			case "div":
				if citationText == "" && strings.EqualFold(nodeAttr(n, "id"), "citation") {
					citationText = cleanText(nodeText(n))
				}
			case "embed":
				src := strings.TrimSpace(nodeAttr(n, "src"))
				if src != "" && (strings.EqualFold(nodeAttr(n, "type"), "application/pdf") || strings.EqualFold(nodeAttr(n, "id"), "pdf") || looksLikePDFRef(src)) {
					priority[0] = append(priority[0], src)
				}
			case "iframe":
				src := strings.TrimSpace(nodeAttr(n, "src"))
				if src != "" && (strings.EqualFold(nodeAttr(n, "id"), "pdf") || looksLikePDFRef(src)) {
					priority[1] = append(priority[1], src)
				}
			case "a":
				href := strings.TrimSpace(nodeAttr(n, "href"))
				if href != "" && (hasAttr(n, "download") || looksLikePDFRef(href)) {
					priority[2] = append(priority[2], href)
				}
				if onclick := nodeAttr(n, "onclick"); onclick != "" {
					if target := extractOnclickTarget(onclick); target != "" && looksLikePDFRef(target) {
						priority[3] = append(priority[3], target)
					}
				}
			case "button":
				if onclick := nodeAttr(n, "onclick"); onclick != "" {
					if target := extractOnclickTarget(onclick); target != "" && looksLikePDFRef(target) {
						priority[3] = append(priority[3], target)
					}
				}
			}
		}
		for child := range n.ChildNodes() {
			walk(child)
		}
	}
	walk(root)

	for _, group := range priority {
		for _, candidate := range group {
			if resolved, ok := resolvePDFURL(page, candidate); ok {
				return ViewerPage{
					PDFURL: resolved,
					Title:  pickViewerTitle(citationText, titleText),
				}, nil
			}
		}
	}
	return ViewerPage{}, httpx.NewError(httpx.CodeNotFound, "doi_not_found", "DOI mirror did not provide a PDF for this DOI")
}

func resolvePDFURL(page *url.URL, raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	ref, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	resolved := page.ResolveReference(ref)
	resolved.Fragment = ""
	if err := httpx.ValidatePublicHTTPSURL(resolved); err != nil {
		return "", false
	}
	return resolved.String(), true
}

func looksLikePDFRef(raw string) bool {
	lower := strings.ToLower(strings.TrimSpace(raw))
	if lower == "" || strings.HasPrefix(lower, "javascript:") || strings.HasPrefix(lower, "mailto:") {
		return false
	}
	return strings.Contains(lower, ".pdf") ||
		strings.Contains(lower, "download=true") ||
		strings.Contains(lower, "/downloads/") ||
		strings.Contains(lower, "/tree/") ||
		strings.Contains(lower, "/pdf/")
}

func extractOnclickTarget(onclick string) string {
	match := locationHrefPattern.FindStringSubmatch(onclick)
	if len(match) < 2 {
		return ""
	}
	return strings.TrimSpace(match[1])
}

func pickViewerTitle(citation, pageTitle string) string {
	if citation != "" {
		return citation
	}
	return pageTitle
}

func nodeAttr(node *xhtml.Node, key string) string {
	for _, attr := range node.Attr {
		if strings.EqualFold(attr.Key, key) {
			return attr.Val
		}
	}
	return ""
}

func hasAttr(node *xhtml.Node, key string) bool {
	for _, attr := range node.Attr {
		if strings.EqualFold(attr.Key, key) {
			return true
		}
	}
	return false
}

func nodeText(node *xhtml.Node) string {
	if node == nil {
		return ""
	}
	var out strings.Builder
	var walk func(*xhtml.Node)
	walk = func(current *xhtml.Node) {
		if current.Type == xhtml.TextNode {
			out.WriteString(current.Data)
		}
		for child := range current.ChildNodes() {
			walk(child)
		}
	}
	walk(node)
	return out.String()
}

func cleanText(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
