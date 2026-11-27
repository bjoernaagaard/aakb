// Package unpaywall resolves open-access PDF copies for DOIs through the
// Unpaywall REST API and streams verified PDFs into internal/store.
package unpaywall

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/bjoernaagaard/aakb/internal/httpx"
	"github.com/bjoernaagaard/aakb/internal/store"
)

const (
	defaultBaseURL   = "https://api.unpaywall.org"
	defaultEmail     = "aakb-client@users.noreply.github.com"
	lookupTimeout    = 8 * time.Second
	maxAPIBytes      = 2 << 20
	maxCandidateURLs = 4
)

var validDOIPattern = regexp.MustCompile(`(?i)^10\.\d{4,9}/\S+$`)

// Config configures an Unpaywall client. Empty Email uses a polite default;
// set Email to "off" or "disabled" to disable Unpaywall lookups.
type Config struct {
	Email      string
	BaseURL    string
	HTTPClient *http.Client
	UserAgent  string
	Store      *store.Store
}

// DownloadRequest describes a DOI PDF download via Unpaywall.
type DownloadRequest struct {
	DOI      string
	Title    string
	Format   string
	Progress func(done, total int64)
}

// DownloadResult describes a stored PDF downloaded or reused via Unpaywall.
type DownloadResult struct {
	Path   string
	Name   string
	Bytes  int64
	MD5    string
	SHA256 string
	Reused bool
}

// Client queries the Unpaywall v2 API and downloads verified open-access PDFs.
type Client struct {
	email string
	base  string
	http  *httpx.Client
	store *store.Store
}

// NewClient validates configuration and builds a Client without network access.
func NewClient(cfg Config) (*Client, error) {
	email := strings.TrimSpace(cfg.Email)
	switch strings.ToLower(email) {
	case "off", "disabled", "none", "false":
		return &Client{}, nil
	case "":
		email = defaultEmail
	}
	if !strings.Contains(email, "@") || strings.ContainsAny(email, " \t\r\n") {
		return nil, httpx.NewError(httpx.CodeConfig, "invalid_unpaywall_email", "UnpaywallEmail must be a valid email address or 'off'")
	}

	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		base = defaultBaseURL
	}
	parsed, err := url.Parse(base)
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.Host == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" {
		return nil, httpx.NewError(httpx.CodeConfig, "invalid_unpaywall_url", "Unpaywall base URL must be an HTTPS origin")
	}

	ua := strings.TrimSpace(cfg.UserAgent)
	if ua == "" {
		ua = "aakb/dev"
	}
	guarded := httpx.NewClient(httpx.ClientOptions{
		BaseClient: cfg.HTTPClient,
		UserAgent:  ua,
		PublicOnly: true,
	})
	return &Client{
		email: email,
		base:  base,
		http:  guarded,
		store: cfg.Store,
	}, nil
}

// Enabled reports whether Unpaywall lookups are enabled.
func (c *Client) Enabled() bool {
	return c != nil && c.email != "" && c.base != ""
}

type apiResponse struct {
	Title          string       `json:"title"`
	BestOALocation *oaLocation  `json:"best_oa_location"`
	OALocations    []oaLocation `json:"oa_locations"`
}

type oaLocation struct {
	URLForPDF string `json:"url_for_pdf"`
	Version   string `json:"version"`
	HostType  string `json:"host_type"`
}

// DownloadDOI looks up doi on Unpaywall and downloads the first working PDF
// candidate into the shared store.
func (c *Client) DownloadDOI(ctx context.Context, req DownloadRequest) (*DownloadResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !c.Enabled() {
		return nil, httpx.NewError(httpx.CodeConfig, "unpaywall_disabled", "Unpaywall is disabled")
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
			return nil, httpx.NewError(httpx.CodeInvalidArgument, "invalid_format", "Unpaywall downloads provide PDF files only")
		}
		format = normalized
	} else {
		format = "pdf"
	}

	key := "unpaywall:" + strings.ToLower(doi)
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

	record, err := c.lookup(ctx, doi)
	if err != nil {
		return nil, err
	}
	candidates := candidatePDFURLs(record)
	if len(candidates) == 0 {
		return nil, httpx.NewError(httpx.CodeNotFound, "no_oa_pdf", "Unpaywall found no open-access PDF for this DOI")
	}

	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = strings.TrimSpace(record.Title)
	}
	if title == "" {
		title = doi
	}

	var lastErr error
	for _, pdfURL := range candidates {
		res, dlErr := c.store.Download(ctx, store.TransferRequest{
			Key:         key,
			URL:         pdfURL,
			Title:       title,
			Format:      format,
			Validation:  store.ValidationPDF,
			AllowResume: true,
			Progress:    req.Progress,
		})
		if dlErr == nil {
			return &DownloadResult{
				Path:   res.Path,
				Name:   res.Name,
				Bytes:  res.Bytes,
				MD5:    res.MD5,
				SHA256: res.SHA256,
				Reused: res.Reused,
			}, nil
		}
		if terminalError(ctx, dlErr) {
			return nil, dlErr
		}
		lastErr = dlErr
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, httpx.NewError(httpx.CodeNotFound, "no_oa_pdf", "Unpaywall found no working open-access PDF for this DOI")
}

func (c *Client) lookup(ctx context.Context, doi string) (*apiResponse, error) {
	lookupCtx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()

	endpoint := c.base + "/v2/" + url.PathEscape(doi) + "?email=" + url.QueryEscape(c.email)
	req, err := http.NewRequestWithContext(lookupCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, httpx.WrapError(httpx.CodeUpstream, "request_failed", "could not construct Unpaywall request", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, httpx.WrapError(httpx.CodeUpstream, "request_failed", "Unpaywall lookup failed", err)
	}
	body, readErr := httpx.ReadBounded(resp.Body, maxAPIBytes)
	closeErr := resp.Body.Close()
	if readErr != nil {
		return nil, httpx.WrapError(httpx.CodeUpstream, "read_failed", "could not read Unpaywall response", readErr)
	}
	if closeErr != nil {
		return nil, httpx.WrapError(httpx.CodeUpstream, "read_failed", "could not close Unpaywall response", closeErr)
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, httpx.NewError(httpx.CodeNotFound, "doi_not_found", "Unpaywall has no record for this DOI")
	case http.StatusTooManyRequests:
		return nil, httpx.NewError(httpx.CodeUpstream, "rate_limited", "Unpaywall rate limit reached")
	default:
		return nil, httpx.NewError(httpx.CodeUpstream, "http_status", "Unpaywall returned an unexpected status")
	}
	var parsed apiResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, httpx.WrapError(httpx.CodeUpstream, "invalid_json", "Unpaywall returned invalid JSON", err)
	}
	return &parsed, nil
}

func candidatePDFURLs(record *apiResponse) []string {
	if record == nil {
		return nil
	}
	seen := make(map[string]bool)
	var out []string
	add := func(raw string) {
		if len(out) >= maxCandidateURLs {
			return
		}
		normalized, ok := normalizePublicHTTPS(raw)
		if !ok || seen[normalized] {
			return
		}
		seen[normalized] = true
		out = append(out, normalized)
	}
	if record.BestOALocation != nil {
		add(record.BestOALocation.URLForPDF)
	}
	locs := append([]oaLocation(nil), record.OALocations...)
	sort.SliceStable(locs, func(i, j int) bool {
		return versionRank(locs[i].Version) < versionRank(locs[j].Version)
	})
	for _, loc := range locs {
		add(loc.URLForPDF)
	}
	return out
}

func normalizePublicHTTPS(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	if strings.EqualFold(parsed.Scheme, "http") {
		parsed.Scheme = "https"
	}
	parsed.Fragment = ""
	if err := httpx.ValidatePublicHTTPSURL(parsed); err != nil {
		return "", false
	}
	return parsed.String(), true
}

func versionRank(version string) int {
	switch strings.ToLower(strings.TrimSpace(version)) {
	case "publishedversion":
		return 0
	case "acceptedversion":
		return 1
	case "submittedversion":
		return 2
	default:
		return 3
	}
}

func terminalError(ctx context.Context, err error) bool {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if pathErr, ok := errors.AsType[*os.PathError](err); ok && pathErr != nil {
		return true
	}
	if pe, ok := errors.AsType[*httpx.ProviderError](err); ok && pe != nil {
		return pe.Code == httpx.CodeConfig || pe.Reason == "local_io"
	}
	return false
}

func parseDOI(raw string) (string, bool) {
	doi := strings.TrimSpace(raw)
	for {
		lower := strings.ToLower(doi)
		switch {
		case strings.HasPrefix(lower, "doi:"):
			doi = strings.TrimSpace(doi[4:])
		case strings.HasPrefix(lower, "https://doi.org/"):
			doi = strings.TrimSpace(doi[len("https://doi.org/"):])
		case strings.HasPrefix(lower, "http://doi.org/"):
			doi = strings.TrimSpace(doi[len("http://doi.org/"):])
		case strings.HasPrefix(lower, "https://dx.doi.org/"):
			doi = strings.TrimSpace(doi[len("https://dx.doi.org/"):])
		case strings.HasPrefix(lower, "http://dx.doi.org/"):
			doi = strings.TrimSpace(doi[len("http://dx.doi.org/"):])
		default:
			return doi, validDOIPattern.MatchString(doi)
		}
	}
}
