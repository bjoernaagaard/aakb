package annas

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	xhtml "golang.org/x/net/html"

	"github.com/bjoernaagaard/aakb/internal/httpx"
)

// viewerPath is the same-origin PDF.js viewer SciDB embeds its PDF in.
const viewerPath = "/pdfjs/web/viewer.html"

// Why a SciDB page gives no file.
var (
	// ErrSciDBEmpty reports a missing SciDB document.
	ErrSciDBEmpty = errors.New("SciDB response was empty")
	// ErrSciDBPageURL reports a SciDB page URL that is not usable.
	ErrSciDBPageURL = errors.New("invalid SciDB page URL")
	// ErrSciDBNoFile reports a SciDB page without one usable viewer link.
	ErrSciDBNoFile = errors.New("SciDB did not provide one usable PDF viewer link")
	// ErrSciDBNoRecord reports a SciDB page without one archive hash.
	ErrSciDBNoRecord = errors.New("SciDB did not identify one archive file for checksum verification")
)

// buildSciDBURL is the SciDB page for doi in the form SciDB redirects to:
// lowercase with a trailing slash. Asking for it directly saves up to four
// redirects, each of which takes the archive seconds to answer.
func buildSciDBURL(base, doi string) string {
	u, err := url.Parse(buildPathURL(base, "/scidb"))
	if err != nil {
		return ""
	}
	u.Path += "/" + strings.ToLower(NormalizeDOI(doi)) + "/"
	u.RawPath, u.RawQuery, u.Fragment = "", "", ""
	return u.String()
}

func isSciDBDOIPage(page *url.URL, base, doi string) bool {
	want, err := url.Parse(buildSciDBURL(base, doi))
	return err == nil && page != nil && httpx.SameOrigin(page, want) &&
		strings.EqualFold(strings.TrimSuffix(page.Path, "/"), strings.TrimSuffix(want.Path, "/"))
}

// fetchSciDBPage reads a SciDB page without following a redirect off SciDB.
// For a DOI it does not have, SciDB redirects to a search for the DOI, which
// costs another slow request and is not the paper's page anyway.
func (c *Client) fetchSciDBPage(ctx context.Context, rawURL string) (*xhtml.Node, *url.URL, error) {
	stopOutside := c.http.WithOnRedirect(func(next *http.Request, _ []*http.Request) error {
		if !strings.HasPrefix(next.URL.Path, "/scidb/") {
			return http.ErrUseLastResponse
		}
		return nil
	})
	resp, err := c.doGetWithClient(ctx, rawURL, nil, stopOutside)
	if err != nil {
		return nil, nil, err
	}
	var finalURL *url.URL
	if resp.Request != nil {
		finalURL = resp.Request.URL
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		httpx.DrainAndClose(resp.Body)
		return nil, finalURL, httpx.NewError(httpx.CodeNotFound, "scidb_not_found", "SciDB has no page for this DOI")
	default:
		if resp.StatusCode >= http.StatusMultipleChoices && resp.StatusCode < http.StatusBadRequest {
			httpx.DrainAndClose(resp.Body)
			return nil, finalURL, httpx.NewError(httpx.CodeNotFound, "scidb_redirected", "SciDB redirected without providing the requested paper's PDF")
		}
		httpx.DrainAndClose(resp.Body)
		return nil, finalURL, httpResponseError(resp, rawURL)
	}
	body, readErr := httpx.ReadBounded(resp.Body, maxHTMLBytes+1)
	closeErr := resp.Body.Close()
	if readErr != nil {
		return nil, finalURL, c.requestFailure(readErr, rawURL)
	}
	if closeErr != nil {
		return nil, finalURL, c.requestFailure(closeErr, rawURL)
	}
	if int64(len(body)) > maxHTMLBytes {
		return nil, finalURL, httpx.NewError(httpx.CodeUpstream, "html_too_large", "SciDB response exceeded the HTML size limit")
	}
	root, err := parseHTML(body)
	if err != nil {
		return nil, finalURL, httpx.WrapError(httpx.CodeUpstream, "invalid_html", "SciDB returned a page this version cannot read", err)
	}
	return root, finalURL, nil
}

// SciDBFile reads the PDF a SciDB page embeds and the hash of its archive
// record, which the download is checked against. Only the same-origin
// viewer's file parameter is read, never arbitrary page links.
func SciDBFile(root *xhtml.Node, pageURL string) (downloadURL, hash string, err error) {
	if root == nil {
		return "", "", ErrSciDBEmpty
	}
	page, parseErr := url.Parse(pageURL)
	if parseErr != nil || page.Scheme != "https" || page.Host == "" || page.User != nil {
		return "", "", ErrSciDBPageURL
	}
	files := make(map[string]struct{})
	for _, frame := range findNodes(root, func(node *xhtml.Node) bool { return isElement(node, "iframe") }) {
		src := nodeAttr(frame, "src")
		if strings.TrimSpace(src) == "" {
			continue
		}
		ref, parseErr := url.Parse(src)
		if parseErr != nil {
			continue
		}
		viewer := page.ResolveReference(ref)
		if !httpx.SameOrigin(page, viewer) || viewer.User != nil || viewer.Path != viewerPath {
			continue
		}
		if file, ok := resolveViewerFile(page, viewer.Query().Get("file")); ok {
			files[file] = struct{}{}
		}
	}
	if len(files) != 1 {
		return "", "", ErrSciDBNoFile
	}
	hashes := make(map[string]struct{})
	for _, link := range findNodes(root, func(node *xhtml.Node) bool {
		return isElement(node, "a") && strings.HasPrefix(nodeAttr(node, "href"), "/md5/")
	}) {
		href := nodeAttr(link, "href")
		if md5, ok := md5Hex(strings.TrimPrefix(href, "/md5/")); ok {
			hashes[md5] = struct{}{}
		}
	}
	if len(hashes) != 1 {
		return "", "", ErrSciDBNoRecord
	}
	for file := range files {
		downloadURL = file
	}
	for md5 := range hashes {
		hash = md5
	}
	return downloadURL, hash, nil
}

// resolveViewerFile resolves a viewer file link on page. It must use HTTPS
// without credentials; a relative link must stay on the page's origin.
func resolveViewerFile(page *url.URL, raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	ref, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	resolved := ref
	if !ref.IsAbs() {
		if resolved = page.ResolveReference(ref); !httpx.SameOrigin(page, resolved) {
			return "", false
		}
	}
	if resolved.Scheme != "https" || resolved.Host == "" || resolved.User != nil {
		return "", false
	}
	return resolved.String(), true
}

// md5Hex reads a 32-character hexadecimal MD5, in lower case.
func md5Hex(value string) (string, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) != 32 {
		return "", false
	}
	if _, err := hex.DecodeString(value); err != nil {
		return "", false
	}
	return value, true
}

// paperFromSciDBPage reads the SciDB page for doi. A canonical SciDB record
// path is an authoritative DOI-to-file mapping. Mirrors sometimes redirect
// this route to a generic search page, so its contents only count when the
// final URL is still the exact requested path.
func (c *Client) paperFromSciDBPage(ctx context.Context, base, doi string) (*Paper, error) {
	root, finalURL, err := c.fetchSciDBPage(ctx, buildSciDBURL(base, doi))
	if err != nil {
		return nil, err
	}
	if !isSciDBDOIPage(finalURL, base, doi) {
		return nil, httpx.NewError(httpx.CodeNotFound, "scidb_no_page", "SciDB has no page for this DOI")
	}
	return c.paperFromSciDB(ctx, root, doi, finalURL.String(), base)
}

func (c *Client) paperFromSciDB(ctx context.Context, root *xhtml.Node, doi, scidbURL, base string) (*Paper, error) {
	downloadURL, hash, err := sciDBFile(root, scidbURL)
	if err != nil {
		return nil, err
	}
	paper := &Paper{DOI: doi, PageURL: scidbURL, Hash: hash, DownloadURL: downloadURL, Format: "PDF"}
	details, err := c.detailsOn(ctx, base, "md5", hash)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return paper, nil
	}
	addSciDBDetails(paper, details)
	return paper, nil
}

// addSciDBDetails fills in a SciDB paper's details from its record's JSON
// metadata: the title, authors, and journal from its publisher line.
func addSciDBDetails(paper *Paper, details *Details) {
	paper.Title, paper.Authors = details.Title, details.Authors
	paper.Journal = joinNonEmpty(", ", details.Publisher, details.Edition)
	if details.Year != "" {
		paper.Year = details.Year
	}
	if details.SizeBytes > 0 {
		paper.Size = formatBytes(details.SizeBytes)
	}
	paper.Sources, paper.FastDownload = details.Sources, details.FastDownload
}

// sciDBFile reads a SciDB page's file and record hash. A page without them
// is NOT_FOUND; a page URL that is not HTTPS is the upstream's fault.
func sciDBFile(root *xhtml.Node, pageURL string) (downloadURL, hash string, err error) {
	downloadURL, hash, err = SciDBFile(root, pageURL)
	switch {
	case errors.Is(err, ErrSciDBPageURL):
		return "", "", httpx.NewError(httpx.CodeUpstream, "scidb_page_url", err.Error())
	case err != nil:
		return "", "", httpx.NewError(httpx.CodeNotFound, "scidb_no_file", err.Error())
	}
	return downloadURL, hash, nil
}

// formatBytes renders a byte count the way Anna's metadata lines show file
// sizes, such as "3.8MB" or "512KB".
func formatBytes(size int64) string {
	const unit = 1024
	if size < unit {
		return strconv.FormatInt(size, 10) + "B"
	}
	value := float64(size)
	suffix := "B"
	for _, next := range []string{"KB", "MB", "GB", "TB"} {
		value /= unit
		suffix = next
		if value < unit {
			break
		}
	}
	text := strconv.FormatFloat(value, 'f', 1, 64)
	return strings.TrimRight(strings.TrimRight(text, "0"), ".") + suffix
}
