package doimirror

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/bjoernaagaard/aakb/internal/httpx"
	"github.com/bjoernaagaard/aakb/internal/store"
)

const (
	maxViewerHTMLBytes = 4 << 20
	peekPrefixBytes    = 512
)

var validDOIPattern = regexp.MustCompile(`(?i)^10\.\d{4,9}/\S+$`)

// DownloadRequest describes a DOI PDF download from a configured DOI mirror.
type DownloadRequest struct {
	DOI      string
	Title    string
	Format   string
	Progress func(done, total int64)
}

// DownloadResult describes a stored PDF downloaded or reused via a DOI mirror.
type DownloadResult struct {
	Path   string
	Name   string
	Bytes  int64
	MD5    string
	SHA256 string
	Reused bool
}

// DownloadDOI resolves doi on a configured mirror and streams the verified PDF
// through the shared download store.
func (c *Client) DownloadDOI(ctx context.Context, req DownloadRequest) (*DownloadResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !c.Configured() {
		return nil, errNoMirrorConfigured
	}
	if c.store == nil {
		return nil, httpx.NewError(httpx.CodeConfig, "missing_store", "DownloadDir and CacheDir are required for downloads")
	}
	doi, ok := parseDOI(req.DOI)
	if !ok {
		return nil, httpx.NewError(httpx.CodeInvalidArgument, "invalid_doi", "doi must be a valid DOI or DOI URL")
	}
	format := strings.TrimSpace(req.Format)
	if format != "" {
		normalized, err := store.NormalizeFormat(format)
		if err != nil {
			return nil, err
		}
		if normalized != "pdf" {
			return nil, httpx.NewError(httpx.CodeInvalidArgument, "invalid_format", "DOI mirror downloads provide PDF files only")
		}
		format = normalized
	} else {
		format = "pdf"
	}

	key := "doimirror:" + strings.ToLower(doi)
	if reused, found := c.store.LookupKey(ctx, key); found {
		return &DownloadResult{
			Path:   reused.Path,
			Name:   reused.Name,
			Bytes:  reused.Bytes,
			MD5:    reused.MD5,
			SHA256: reused.SHA256,
			Reused: true,
		}, nil
	}

	var resolved ViewerPage
	err := c.onMirror(ctx, func(base string) error {
		page, resolveErr := c.resolveOn(ctx, base, doi)
		if resolveErr != nil {
			return resolveErr
		}
		resolved = page
		return nil
	})
	if err != nil {
		return nil, err
	}

	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = resolved.Title
	}
	if title == "" {
		title = doi
	}

	transfer, err := c.store.Download(ctx, store.TransferRequest{
		Key:         key,
		URL:         resolved.PDFURL,
		Title:       title,
		Format:      format,
		Validation:  store.ValidationPDF,
		AllowResume: true,
		Fetch:       c.fetchStream,
		Progress:    req.Progress,
	})
	if err != nil {
		if pe, isPE := errors.AsType[*httpx.ProviderError](err); isPE && pe != nil && pe.Code == httpx.CodeUpstreamBlocked {
			c.mirrorFailed(ctx, resolved.PDFURL, mirrorFailure{Reason: "returned HTML instead of a PDF", Blocked: true})
		}
		return nil, err
	}
	return &DownloadResult{
		Path:   transfer.Path,
		Name:   transfer.Name,
		Bytes:  transfer.Bytes,
		MD5:    transfer.MD5,
		SHA256: transfer.SHA256,
		Reused: transfer.Reused,
	}, nil
}

func (c *Client) resolveOn(ctx context.Context, base, doi string) (ViewerPage, error) {
	pageURL := buildDOIURL(base, doi)
	resp, err := c.doGet(ctx, pageURL, nil)
	if err != nil {
		return ViewerPage{}, err
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // closing mirror viewer response body

	finalURL := pageURL
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusGone:
		httpx.DrainAndClose(resp.Body)
		return ViewerPage{}, httpx.NewError(httpx.CodeNotFound, "doi_not_found", "DOI mirror has no record for this DOI")
	default:
		httpx.DrainAndClose(resp.Body)
		return ViewerPage{}, httpStatusError(resp, pageURL)
	}

	reader := bufio.NewReader(resp.Body)
	prefix, peekErr := reader.Peek(peekPrefixBytes)
	if peekErr != nil && !errors.Is(peekErr, io.EOF) && !errors.Is(peekErr, bufio.ErrBufferFull) {
		return ViewerPage{}, c.requestFailure(peekErr, pageURL)
	}
	if store.ValidatePDFPrefix(prefix) {
		return ViewerPage{PDFURL: finalURL}, nil
	}

	body, readErr := httpx.ReadBounded(reader, maxViewerHTMLBytes+1)
	if readErr != nil {
		return ViewerPage{}, c.requestFailure(readErr, pageURL)
	}
	if int64(len(body)) > maxViewerHTMLBytes {
		return ViewerPage{}, httpx.NewError(httpx.CodeUpstream, "html_too_large", "DOI mirror response exceeded the HTML size limit")
	}
	parsed, parseErr := ParseViewerPage(finalURL, body)
	if parseErr != nil {
		if pe, ok := errors.AsType[*httpx.ProviderError](parseErr); ok && pe != nil && pe.Code == httpx.CodeUpstreamBlocked {
			c.mirrorFailed(ctx, base, mirrorFailure{Reason: "answered with an anti-bot challenge", Blocked: true})
		}
		return ViewerPage{}, parseErr
	}
	return parsed, nil
}

func (c *Client) fetchStream(ctx context.Context, rawURL string, extraHeaders http.Header) (*http.Response, error) {
	return c.doGet(ctx, rawURL, extraHeaders)
}

func (c *Client) doGet(ctx context.Context, rawURL string, extraHeaders http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, c.http.Redactor().WrapError(httpx.CodeInvalidArgument, "invalid_url", "invalid DOI mirror URL", err)
	}
	for key, values := range extraHeaders {
		req.Header[key] = slices.Clone(values)
	}
	if c.cookie != "" {
		if _, ok := c.mirrorHost(req.URL); ok {
			req.Header.Set("Cookie", c.cookie)
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			c.mirrorFailed(ctx, rawURL, unreachableFailure(err))
		}
		return nil, c.requestFailure(err, rawURL)
	}
	if failure := responseFailure(resp); failure != nil {
		c.mirrorFailed(ctx, rawURL, *failure)
	}
	return resp, nil
}

func buildDOIURL(base, doi string) string {
	u, err := url.Parse(strings.TrimSuffix(strings.TrimSpace(base), "/"))
	if err != nil || u.Host == "" {
		return strings.TrimSuffix(base, "/") + "/" + doi
	}
	u.Path = "/" + doi
	u.RawPath, u.RawQuery, u.Fragment = "", "", ""
	return u.String()
}

func parseDOI(raw string) (string, bool) {
	doi := normalizeDOI(raw)
	return doi, validDOIPattern.MatchString(doi)
}

func normalizeDOI(doi string) string {
	for {
		next := normalizeDOIOnce(doi)
		if next == doi {
			return next
		}
		doi = next
	}
}

func normalizeDOIOnce(doi string) string {
	doi = unwrapDOI(doi)
	if strings.HasPrefix(strings.ToLower(doi), "doi:") {
		doi = unwrapDOI(doi[len("doi:"):])
	}
	lower := strings.ToLower(doi)
	for _, prefix := range []string{"https://doi.org/", "http://doi.org/", "https://dx.doi.org/", "http://dx.doi.org/"} {
		if strings.HasPrefix(lower, prefix) {
			if parsed, err := url.Parse(doi); err == nil && parsed.Host != "" {
				doi = strings.TrimPrefix(parsed.Path, "/")
			} else {
				doi = doi[len(prefix):]
			}
			break
		}
	}
	return strings.TrimSpace(doi)
}

func unwrapDOI(doi string) string {
	doi = strings.TrimSpace(doi)
	for len(doi) >= 2 {
		first, last := doi[0], doi[len(doi)-1]
		wrapped := first == '<' && last == '>' || first == '(' && last == ')' ||
			first == '[' && last == ']' || first == '{' && last == '}' ||
			first == '"' && last == '"' || first == '\'' && last == '\''
		if !wrapped {
			break
		}
		doi = strings.TrimSpace(doi[1 : len(doi)-1])
	}
	return doi
}

func httpStatusError(resp *http.Response, rawURL string) *httpx.ProviderError {
	status := resp.StatusCode
	host := responseHost(resp, rawURL)
	message := strings.TrimSpace(fmt.Sprintf("HTTP status %d %s", status, http.StatusText(status))) + " from " + host
	code := httpx.CodeUpstream
	reason := "http_status"
	switch status {
	case http.StatusNotFound, http.StatusGone:
		code = httpx.CodeNotFound
		reason = "not_found"
		message += ": requested resource is unavailable on this server"
	case http.StatusUnauthorized, http.StatusForbidden:
		code = httpx.CodeUpstreamBlocked
		reason = "access_denied"
		message += ": access denied"
	case http.StatusTooManyRequests:
		code = httpx.CodeUpstream
		reason = "rate_limited"
		message += ": rate limited; try again later"
	default:
		if status >= http.StatusInternalServerError {
			message += ": server error; try again later"
		}
	}
	return &httpx.ProviderError{
		Code:        code,
		Reason:      reason,
		SafeMessage: message,
		StatusCode:  status,
	}
}

func (c *Client) requestFailure(err error, rawURL string) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if pe, ok := errors.AsType[*httpx.ProviderError](err); ok && pe != nil {
		return pe
	}
	message := err.Error()
	if urlErr, ok := errors.AsType[*url.Error](err); ok && urlErr != nil {
		message = urlErr.Err.Error()
		if urlErr.URL != "" {
			rawURL = urlErr.URL
		}
	}
	message = diagnosticURLPattern.ReplaceAllStringFunc(message, requestHost)
	safe := "request to " + requestHost(rawURL) + " failed: " + message
	return c.http.Redactor().WrapError(httpx.CodeUpstream, "request_failed", safe, err)
}
