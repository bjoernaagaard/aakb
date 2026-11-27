package aakb

import (
	"context"
	"strings"
)

// UpstreamStatus reports the current reachability of the configured archive mirror.
type UpstreamStatus struct {
	Status     string `json:"status"`
	HTTPStatus int    `json:"http_status"`
}

type archiveSearchQuery struct {
	Kind               Kind
	Query              string
	Title              string
	Author             string
	Publisher          string
	Year               string
	Formats            []string
	Languages          []string
	Sources            []string
	Content            []string
	Sort               Sort
	Page               int
	SearchDescriptions bool
}

type archiveDownloadRequest struct {
	Hash          string
	DOI           string
	Title         string
	ExpectedTitle string
	Format        string
	Progress      func(done, total int64)
}

// Compile-time checks that the providers satisfy the seam interfaces.
var (
	_ archiveProvider   = (*remoteArchiveProvider)(nil)
	_ papersProvider    = (*openalexProvider)(nil)
	_ doiMirrorProvider = (*remoteDOIMirrorProvider)(nil)
)

type doiMirrorDownloadRequest struct {
	DOI      string
	Title    string
	Format   string
	Progress func(done, total int64)
}

type openAlexSearchRequest struct {
	Query            string
	Title            string
	Author           string
	Year             string
	WorkType         string
	Languages        []string
	Sort             Sort
	Page             int
	Limit            int
	DownloadableOnly bool
	Filter           OpenAlexFilter
}

type openAlexDownloadRequest struct {
	OpenAlexID string
	Title      string
	Format     string
	Artifact   string
	LocationID string
	Version    string
	Progress   func(done, total int64)
}

type archiveProvider interface {
	Search(ctx context.Context, query archiveSearchQuery) (*SearchResult, error)
	LookupDOI(ctx context.Context, doi string) (*Record, error)
	Details(ctx context.Context, id string) (*RecordDetails, error)
	FileStats(ctx context.Context, hashes []string) (map[string]Stats, error)
	DownloadHash(ctx context.Context, req archiveDownloadRequest) (*DownloadResult, error)
	DownloadDOI(ctx context.Context, req archiveDownloadRequest) (*DownloadResult, error)
	AccountStatus(ctx context.Context) (*Account, error)
	ProbeHealth(ctx context.Context) UpstreamStatus
}

type papersProvider interface {
	Search(ctx context.Context, req openAlexSearchRequest) (*SearchResult, error)
	Details(ctx context.Context, id string) (*RecordDetails, error)
	Read(ctx context.Context, req ReadRequest) (*Article, error)
	Download(ctx context.Context, req openAlexDownloadRequest) (*DownloadResult, error)
}

type doiMirrorProvider interface {
	DownloadDOI(ctx context.Context, req doiMirrorDownloadRequest) (*DownloadResult, error)
}

func isDOIQuery(query string) bool {
	query = strings.ToLower(unwrapDOI(query))
	return strings.HasPrefix(query, "10.") ||
		strings.HasPrefix(query, "doi:") ||
		strings.HasPrefix(query, "https://doi.org/") ||
		strings.HasPrefix(query, "http://doi.org/") ||
		strings.HasPrefix(query, "https://dx.doi.org/") ||
		strings.HasPrefix(query, "http://dx.doi.org/")
}

func unwrapDOI(doi string) string {
	doi = strings.TrimSpace(doi)
	for len(doi) >= 2 {
		first, last := doi[0], doi[len(doi)-1]
		if (first != '<' || last != '>') && (first != '(' || last != ')') &&
			(first != '[' || last != ']') && (first != '{' || last != '}') &&
			(first != '"' || last != '"') && (first != '\'' || last != '\'') {
			break
		}
		doi = strings.TrimSpace(doi[1 : len(doi)-1])
	}
	return doi
}
