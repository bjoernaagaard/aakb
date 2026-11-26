package openalex

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"

	"github.com/bjoernaagaard/aakb/internal/httpx"
	"github.com/bjoernaagaard/aakb/internal/store"
)

// DownloadOptions selects one OpenAlex work and one of its copies. Artifact
// is "pdf" (default) or "tei_xml"; LocationID and Version ("published",
// "accepted" or "submitted") choose among the open copies of a PDF.
type DownloadOptions struct {
	DOI        string
	OpenAlexID string
	Title      string
	Format     string
	Artifact   string
	LocationID string
	Version    string
}

// DownloadResult identifies the stored file and the copy it came from.
type DownloadResult struct {
	Path       string
	Name       string
	Bytes      int64
	MD5        string
	SHA256     string
	MIMEType   string
	Route      string
	OpenAlexID string
	DOI        string
	LocationID string
	Version    string
	URL        string
	Reused     bool
}

// DownloadCandidate is one ranked copy of a work: the hosted artifact or one
// of up to two ranked open-access external PDFs.
type DownloadCandidate struct {
	URL      string
	Route    string
	Location string
	Version  string
	Hosted   bool
}

// NormalizeDownload trims and validates download options: exactly one of DOI
// or OpenAlex ID, DOI and OpenAlex ID canonicalization, artifact defaulting
// to pdf, format dot-stripping, and version normalization.
func NormalizeDownload(options DownloadOptions) (DownloadOptions, error) {
	options.DOI = strings.TrimSpace(options.DOI)
	options.OpenAlexID = strings.TrimSpace(options.OpenAlexID)
	count := 0
	for _, value := range []string{options.DOI, options.OpenAlexID} {
		if value != "" {
			count++
		}
	}
	if count != 1 {
		return options, downloadError(httpx.CodeInvalidArgument, "invalid_identifier", "supply exactly one of doi or openalex_id")
	}
	if options.DOI != "" {
		if IsWorkID(options.DOI) {
			options.OpenAlexID = options.DOI
			options.DOI = ""
		} else if normalized := NormalizeDOI(options.DOI); normalized != "" {
			options.DOI = normalized
		} else {
			return options, downloadError(httpx.CodeInvalidArgument, "invalid_identifier", "doi must be a DOI or DOI URL")
		}
	}
	if options.OpenAlexID != "" {
		if !IsWorkID(options.OpenAlexID) {
			return options, downloadError(httpx.CodeInvalidArgument, "invalid_identifier", "openalex_id must be a W ID or OpenAlex work URL")
		}
		if canonical, err := CanonicalID(options.OpenAlexID); err == nil {
			options.OpenAlexID = canonical
		}
	}
	options.Artifact = strings.ToLower(strings.TrimSpace(options.Artifact))
	if options.Artifact == "" {
		options.Artifact = "pdf"
	}
	if options.Artifact != "pdf" && options.Artifact != "tei_xml" {
		return options, downloadError(httpx.CodeInvalidArgument, "invalid_artifact", "artifact must be pdf or tei_xml")
	}
	options.Format = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(options.Format), "."))
	options.Version = normalizedVersion(options.Version)
	if options.Version == "invalid" {
		return options, downloadError(httpx.CodeInvalidArgument, "invalid_version", "version must be published, accepted or submitted")
	}
	if options.Artifact == "tei_xml" && (options.LocationID != "" || options.Version != "" ||
		(options.Format != "" && options.Format != "xml")) {
		return options, downloadError(httpx.CodeInvalidArgument, "incompatible_arguments", "TEI XML uses the hosted structured text without a location, version or non-XML extension")
	}
	if options.Artifact == "pdf" && options.Format != "" && options.Format != "pdf" {
		if options.LocationID != "" || options.Version != "" {
			return options, downloadError(httpx.CodeInvalidArgument, "invalid_format", "a selected OpenAlex location or version requires PDF format")
		}
		return options, downloadError(httpx.CodeInvalidArgument, "invalid_format", "a selected OpenAlex work can be downloaded as PDF or TEI XML")
	}
	return options, nil
}

// DownloadCandidates ranks the compatible copies: the hosted PDF first, then
// up to two open-access external PDFs ordered by version
// (published > accepted > submitted), with dissertations preferring
// repositories. A selected location returns exactly that copy.
func DownloadCandidates(work *Work, options DownloadOptions) ([]DownloadCandidate, error) {
	if options.LocationID != "" {
		for _, location := range work.Locations {
			if location.ID != options.LocationID {
				continue
			}
			if options.Version != "" && normalizedVersion(location.Version) != options.Version {
				return nil, downloadError(httpx.CodeNotFound, "version_mismatch", "the selected location does not establish the requested version")
			}
			if !eligibleLocation(location) {
				return nil, downloadError(httpx.CodeNotFound, "no_content", "the selected location has no direct open PDF")
			}
			return []DownloadCandidate{{URL: locationURL(location), Route: "external_pdf", Location: location.ID, Version: location.Version}}, nil
		}
		return nil, downloadError(httpx.CodeInvalidArgument, "unknown_location", "location_id does not belong to the selected work")
	}
	if options.Artifact == "tei_xml" {
		if work.HasContent.GrobidXML && work.ContentURLs.GrobidXML != "" {
			return []DownloadCandidate{{URL: work.ContentURLs.GrobidXML, Route: "hosted_tei_xml", Hosted: true}}, nil
		}
		return nil, nil
	}
	var candidates []DownloadCandidate
	alternateDOI := options.DOI != "" && NormalizeDOI(work.DOI) != options.DOI
	if work.HasContent.PDF && work.ContentURLs.PDF != "" && options.Version == "" && !alternateDOI {
		candidates = append(candidates, DownloadCandidate{URL: work.ContentURLs.PDF, Route: "hosted_pdf", Hosted: true})
	}
	locations := append([]Location(nil), work.Locations...)
	sourceName := func(location Location) string {
		if location.Source == nil {
			return ""
		}
		return location.Source.DisplayName
	}
	sourceType := func(location Location) string {
		if location.Source == nil {
			return ""
		}
		return location.Source.Type
	}
	sort.SliceStable(locations, func(i, j int) bool {
		if work.Type == "dissertation" && (sourceType(locations[i]) == "repository") != (sourceType(locations[j]) == "repository") {
			return sourceType(locations[i]) == "repository"
		}
		rankI, rankJ := versionRank(locations[i].Version), versionRank(locations[j].Version)
		if rankI != rankJ {
			return rankI < rankJ
		}
		if sourceName(locations[i]) != sourceName(locations[j]) {
			return sourceName(locations[i]) < sourceName(locations[j])
		}
		return locations[i].ID < locations[j].ID
	})
	seen := map[string]bool{}
	externalCount := 0
	for _, location := range locations {
		if externalCount == 2 {
			break
		}
		if !eligibleLocation(location) || seen[locationURL(location)] {
			continue
		}
		if options.Version != "" && normalizedVersion(location.Version) != options.Version {
			continue
		}
		if alternateDOI && NormalizeDOI(location.DOI) != options.DOI {
			continue
		}
		candidates = append(candidates, DownloadCandidate{URL: locationURL(location), Route: "external_pdf", Location: location.ID, Version: location.Version})
		seen[locationURL(location)] = true
		externalCount++
	}
	return candidates, nil
}

// DownloadArtifact stores one ranked copy through the shared download store.
// Hosted artifacts authenticate with the OpenAlex key on a public-only client;
// external copies use a nil fetch so the store's own public-only client runs
// the transfer. The manifest key binds the work, route, location, version,
// and artifact.
func (c *Client) DownloadArtifact(ctx context.Context, st *store.Store, work *Work, candidate DownloadCandidate, title, format string, progress func(done, total int64)) (DownloadResult, error) {
	if err := validateArtifactURL(candidate.URL, candidate.Hosted); err != nil {
		return DownloadResult{}, err
	}
	if candidate.Hosted && c.key == "" {
		return DownloadResult{}, httpx.NewError(httpx.CodeConfig, "missing_key", "set OPENALEX_API_KEY to use OpenAlex")
	}
	if title == "" {
		title = work.DisplayName
	}
	extension := "pdf"
	validation := store.ValidationPDF
	if candidate.Route == "hosted_tei_xml" {
		extension = "xml"
		validation = store.ValidationTEIXML
	}
	if format == "" {
		format = extension
	}
	key := strings.Join([]string{"openalex:" + work.ShortID(), candidate.Route, candidate.Location, candidate.Version, format}, "|")
	var fetch store.FetchFunc
	if candidate.Hosted {
		fetch = func(ctx context.Context, rawURL string, extra http.Header) (*http.Response, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
			if err != nil {
				return nil, httpx.NewError(httpx.CodeInvalidArgument, "invalid_url", "invalid download URL")
			}
			req.Header.Set("Authorization", "Bearer "+c.key)
			for name, values := range extra {
				for _, value := range values {
					req.Header.Add(name, value)
				}
			}
			return c.fetch.Do(req)
		}
	}
	result, err := st.Download(ctx, store.TransferRequest{
		Key:        key,
		URL:        candidate.URL,
		Title:      title,
		Format:     format,
		Validation: validation,
		Fetch:      fetch,
		Progress:   progress,
	})
	if err != nil {
		return DownloadResult{}, err
	}
	return DownloadResult{
		Path: result.Path, Name: result.Name, Bytes: result.Bytes,
		MD5: result.MD5, SHA256: result.SHA256, MIMEType: result.MIMEType,
		Route: candidate.Route, OpenAlexID: work.ShortID(), DOI: work.DOI,
		LocationID: candidate.Location, Version: candidate.Version,
		URL: work.LandingURL(), Reused: result.Reused,
	}, nil
}

// ArchiveFallbackDOI reports the DOI the caller may retry through Anna's
// Archive when the OpenAlex copies are unavailable: provider auto, a plain
// PDF request, and a DOI on the options or the resolved work.
func ArchiveFallbackDOI(work *Work, options DownloadOptions) (string, bool) {
	if options.LocationID != "" || options.Version != "" || options.Artifact != "pdf" {
		return "", false
	}
	if options.DOI != "" {
		return options.DOI, true
	}
	if work != nil && NormalizeDOI(work.DOI) != "" {
		return NormalizeDOI(work.DOI), true
	}
	return "", false
}

// UnavailableDownload wraps the last OpenAlex failure when no configured
// source supplied the selected file, keeping the underlying code and message.
func UnavailableDownload(cause error) error {
	out := &httpx.ProviderError{
		Code:        httpx.CodeNotFound,
		Reason:      "no_content",
		SafeMessage: "no configured source supplied the selected file",
		Cause:       cause,
	}
	if cause == nil {
		return out
	}
	var detail *httpx.ProviderError
	if errors.As(cause, &detail) && detail != nil {
		out.Code = detail.Code
		if detail.Reason != "" {
			out.Reason = detail.Reason
		}
		message := detail.SafeMessage
		if message == "" {
			message = detail.Reason
		}
		out.SafeMessage = fmt.Sprintf("no configured source supplied the selected file: %s", message)
	}
	return out
}

func downloadError(code httpx.Code, reason, message string) error {
	return httpx.NewError(code, reason, message)
}

func locationURL(location Location) string {
	return location.PDFURL
}

// HasDownload reports whether OpenAlex advertises a directly fetchable PDF
// for work: hosted content or an open-access external PDF. Structured-text
// copies alone do not count because the default download target is PDF.
func HasDownload(work *Work) bool {
	if work == nil {
		return false
	}
	if work.HasContent.PDF && work.ContentURLs.PDF != "" {
		return true
	}
	return slices.ContainsFunc(work.Locations, eligibleLocation)
}

// eligibleLocation admits only direct open-access PDFs over public HTTPS
// without credentials.
func eligibleLocation(location Location) bool {
	parsed, err := url.Parse(locationURL(location))
	return location.IsOA && err == nil && parsed.Scheme == "https" && parsed.Hostname() != "" && parsed.User == nil
}

// validateArtifactURL confines hosted artifacts to the official OpenAlex
// content host and requires public credential-free HTTPS everywhere. Loopback,
// private, and reserved hosts never pass, even before DNS pinning at dial time.
func validateArtifactURL(raw string, hosted bool) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return httpx.NewError(httpx.CodeInvalidArgument, "invalid_artifact_url", "artifact URL must be public HTTPS without credentials")
	}
	if err := httpx.ValidatePublicHTTPSURL(parsed); err != nil {
		return httpx.NewError(httpx.CodeInvalidArgument, "invalid_artifact_url", "artifact URL must be public HTTPS without credentials")
	}
	if parsed.Fragment != "" {
		return httpx.NewError(httpx.CodeInvalidArgument, "invalid_artifact_url", "artifact URL must be public HTTPS without credentials")
	}
	if hosted && (parsed.Host != "content.openalex.org" || !strings.HasPrefix(parsed.Path, "/works/")) {
		return httpx.NewError(httpx.CodeInvalidArgument, "invalid_artifact_url", "hosted content URL must use the official OpenAlex content host")
	}
	return nil
}

func normalizedVersion(version string) string {
	switch strings.ToLower(strings.TrimSpace(version)) {
	case "", "any":
		return ""
	case "published", "publishedversion":
		return "publishedVersion"
	case "accepted", "acceptedversion":
		return "acceptedVersion"
	case "submitted", "submittedversion":
		return "submittedVersion"
	default:
		return "invalid"
	}
}

func versionRank(version string) int {
	switch normalizedVersion(version) {
	case "publishedVersion":
		return 0
	case "acceptedVersion":
		return 1
	case "submittedVersion":
		return 2
	default:
		return 3
	}
}
