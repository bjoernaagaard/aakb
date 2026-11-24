// Package service is the shared core behind the MCP tools and the CLI.
// It wraps the anna client with unified inputs, dedupe and ranking, so
// both adapters return the same shapes. Use it only for material you
// are entitled to obtain.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/SokolskyNikita/annas-mcp/anna"
	"github.com/SokolskyNikita/annas-mcp/papers"

	"github.com/bjoernaagaard/aakb/internal/config"
)

const (
	defaultSearchTimeout   = 60 * time.Second
	defaultDetailsTimeout  = 60 * time.Second
	defaultDownloadTimeout = 30 * time.Minute
	maxLimit               = 50
	maxPartial             = 5
	maxDescription         = 2000
)

// ErrInvalidArgument marks caller input the service rejects.
var ErrInvalidArgument = errors.New("invalid argument")

func invalid(format string, args ...any) error {
	return fmt.Errorf("[INVALID_ARGUMENT] %s: %w", fmt.Sprintf(format, args...), ErrInvalidArgument)
}

// Service performs Anna's Archive operations for one configuration.
type Service struct {
	client     *anna.Client
	papers     *papers.Client
	baseURL    string
	autoMirror bool
	// DownloadPath is the folder downloads land in.
	DownloadPath string
}

// New builds a Service and prepares the cache directory for the session file.
func New(cfg config.Config, logger *slog.Logger) (*Service, error) {
	if err := ensureDir(cfg.CacheDir); err != nil {
		return nil, err
	}
	client, err := anna.NewClient(anna.Config{
		BaseURL:       cfg.BaseURL,
		SecretKey:     cfg.SecretKey,
		AccountCookie: cfg.AccountCookie,
		DownloadPath:  cfg.DownloadPath,
		SessionFile:   cfg.SessionFile(),
		AutoBaseURL:   cfg.AutoBaseURL,
		Logger:        logger,
	})
	if err != nil {
		return nil, fmt.Errorf("anna client: %w", err)
	}
	svc := &Service{client: client, baseURL: cfg.BaseURL, autoMirror: cfg.AutoBaseURL, DownloadPath: cfg.DownloadPath}
	if cfg.OpenAlexKey != "" {
		pc, err := papers.NewClient(papers.Config{
			OpenAlexAPIKey: cfg.OpenAlexKey,
			DownloadPath:   cfg.DownloadPath,
			CacheDir:       cfg.CacheDir,
			Archive:        client,
		})
		if err != nil {
			return nil, fmt.Errorf("openalex client: %w", err)
		}
		svc.papers = pc
	}
	return svc, nil
}

// Record is the unified search result across books, papers and catalog entries.
type Record struct {
	Kind           string            `json:"kind"`
	Hash           string            `json:"hash,omitempty"`
	ID             string            `json:"id,omitempty"`
	Title          string            `json:"title"`
	Authors        string            `json:"authors,omitempty"`
	Publisher      string            `json:"publisher,omitempty"`
	Edition        string            `json:"edition,omitempty"`
	Year           string            `json:"year,omitempty"`
	Language       string            `json:"language,omitempty"`
	Format         string            `json:"format,omitempty"`
	Size           string            `json:"size,omitempty"`
	Content        string            `json:"content,omitempty"`
	Sources        []string          `json:"sources,omitempty"`
	DOI            string            `json:"doi,omitempty"`
	URL            string            `json:"url,omitempty"`
	FastDownload   bool              `json:"fast_download"`
	PossibleIssues bool              `json:"possible_issues,omitempty"`
	Stats          *anna.RecordStats `json:"stats,omitempty"`
	Score          int               `json:"score,omitempty"`
}

// SearchInput is the single search entry point for every kind.
type SearchInput struct {
	Kind               string `json:"kind,omitempty" jsonschema:"book, article, metadata or openalex; empty auto-detects a DOI as an article"`
	Provider           string `json:"provider,omitempty" jsonschema:"openalex routes article search to OpenAlex; needs OPENALEX_API_KEY"`
	WorkType           string `json:"work_type,omitempty" jsonschema:"OpenAlex work type such as dissertation (openalex only)"`
	Query              string `json:"query,omitempty" jsonschema:"title, author, topic, ISBN, DOI or MD5"`
	Title              string `json:"title,omitempty" jsonschema:"phrase the title must contain"`
	Author             string `json:"author,omitempty" jsonschema:"phrase the author must contain"`
	Publisher          string `json:"publisher,omitempty" jsonschema:"phrase the publisher must contain"`
	Year               string `json:"year,omitempty" jsonschema:"publication year, such as 2008"`
	Format             string `json:"format,omitempty" jsonschema:"comma-separated formats such as epub,pdf; prefix - to exclude"`
	Language           string `json:"language,omitempty" jsonschema:"comma-separated language codes such as en,de"`
	Source             string `json:"source,omitempty" jsonschema:"comma-separated source libraries or catalogs"`
	Content            string `json:"content,omitempty" jsonschema:"comma-separated content types such as book_nonfiction,magazine"`
	Sort               string `json:"sort,omitempty" jsonschema:"newest, oldest, largest, smallest, newest_added, oldest_added or random"`
	Page               int    `json:"page,omitempty" jsonschema:"result page, starting at 1"`
	Limit              int    `json:"limit,omitempty" jsonschema:"max results to return, 1 to 50"`
	IncludeStats       bool   `json:"include_stats,omitempty" jsonschema:"add download and quality counts; costs extra requests"`
	DownloadableOnly   bool   `json:"downloadable_only,omitempty" jsonschema:"only results the download tool can fetch"`
	SearchDescriptions bool   `json:"search_descriptions,omitempty" jsonschema:"also match query against descriptions"`
	Rank               bool   `json:"rank,omitempty" jsonschema:"rank exact results by availability, quality and popularity"`
	TimeoutSeconds     int    `json:"timeout_seconds,omitempty" jsonschema:"whole-operation timeout; 0 uses the default"`
}

// SearchOutput separates exact matches from looser upstream suggestions.
type SearchOutput struct {
	Kind              string    `json:"kind"`
	Results           []*Record `json:"results"`
	PartialMatches    []*Record `json:"partial_matches,omitempty"`
	Total             int       `json:"total"`
	TotalIsLowerBound bool      `json:"total_is_lower_bound,omitempty"`
	MorePages         bool      `json:"more_pages"`
	NextPage          int       `json:"next_page,omitempty"`
}

// DetailsInput identifies one record by hash, DOI or catalog id.
type DetailsInput struct {
	ID             string `json:"id" jsonschema:"MD5 hash, Anna's Archive /md5/ URL, DOI or catalog id such as isbndb:9780521879286"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"whole-operation timeout; 0 uses the default"`
}

// DetailsOutput is the trimmed, stable shape get_details returns.
type DetailsOutput struct {
	ID           string              `json:"id"`
	Hash         string              `json:"hash,omitempty"`
	URL          string              `json:"url"`
	Title        string              `json:"title"`
	Authors      string              `json:"authors,omitempty"`
	Publisher    string              `json:"publisher,omitempty"`
	Edition      string              `json:"edition,omitempty"`
	Year         string              `json:"year,omitempty"`
	Languages    []string            `json:"languages,omitempty"`
	Format       string              `json:"format,omitempty"`
	SizeBytes    int64               `json:"size_bytes,omitempty"`
	Description  string              `json:"description,omitempty"`
	Identifiers  map[string][]string `json:"identifiers,omitempty"`
	Subjects     []string            `json:"subjects,omitempty"`
	Sources      []string            `json:"sources,omitempty"`
	FastDownload bool                `json:"fast_download"`
	Problems     []anna.Problem      `json:"problems,omitempty"`
	Stats        *anna.RecordStats   `json:"stats,omitempty"`
}

// ReadInput requests one bounded OpenAlex full-text passage.
type ReadInput struct {
	ID             string `json:"id" jsonschema:"OpenAlex work id, DOI or work URL"`
	Continuation   string `json:"continuation,omitempty" jsonschema:"continuation token from a previous article_read result"`
	Limit          int    `json:"limit,omitempty" jsonschema:"max passages to return"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"whole-operation timeout; 0 uses the default"`
}

// DownloadInput selects exactly one file, by hash or by DOI.
type DownloadInput struct {
	Hash           string `json:"hash,omitempty" jsonschema:"MD5 hash or Anna's Archive /md5/ URL; give exactly one of hash, doi or openalex_id"`
	DOI            string `json:"doi,omitempty" jsonschema:"paper DOI, resolved to a file first"`
	OpenAlexID     string `json:"openalex_id,omitempty" jsonschema:"OpenAlex work id; downloads through OpenAlex first when configured"`
	Title          string `json:"title,omitempty" jsonschema:"filename title for a fresh download"`
	Format         string `json:"format,omitempty" jsonschema:"file extension such as epub or pdf; does not convert"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"whole-operation timeout; 0 uses the default"`
}

// DownloadOutput reports where the verified file landed.
type DownloadOutput struct {
	Path          string `json:"path"`
	Bytes         int64  `json:"bytes"`
	MD5           string `json:"md5,omitempty"`
	Reused        bool   `json:"reused,omitempty"`
	DownloadsLeft *int   `json:"downloads_left,omitempty"`
}

// AccountInput carries only the optional timeout.
type AccountInput struct {
	TimeoutSeconds int `json:"timeout_seconds,omitempty" jsonschema:"whole-operation timeout; 0 uses the default"`
}

// Search runs one unified search. A DOI with kind article or auto is
// resolved to its single paper instead of a keyword page.
func (s *Service) Search(ctx context.Context, in SearchInput) (*SearchOutput, error) {
	kind := strings.ToLower(strings.TrimSpace(in.Kind))
	if err := validateSearch(in); err != nil {
		return nil, err
	}
	timeout := timeoutOr(in.TimeoutSeconds, defaultSearchTimeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if strings.EqualFold(in.Provider, "openalex") || kind == "openalex" {
		return s.searchOpenAlex(ctx, in)
	}
	opts, err := searchOptions(in)
	if err != nil {
		return nil, err
	}
	if (kind == "" || kind == "article") && anna.IsDOIQuery(in.Query) {
		paper, err := s.client.LookupDOI(ctx, in.Query, timeout)
		if err != nil {
			return nil, wrap(err)
		}
		out := &SearchOutput{Kind: "article", Results: []*Record{paperRecord(paper)}, Total: 1}
		if in.IncludeStats {
			s.attachStats(ctx, out, in.Rank)
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	switch kind {
	case "", "book":
		page, err := s.client.FindBook(ctx, in.Query, opts, timeout)
		if err != nil {
			return nil, wrap(err)
		}
		out := finishPage("book", in, page.Results, page.Partial,
			page.Total, page.TotalIsLowerBound, page.MorePages,
			bookRecord)
		return s.finishSearch(ctx, in, out)
	case "article":
		page, err := s.client.FindArticle(ctx, in.Query, opts, timeout)
		if err != nil {
			return nil, wrap(err)
		}
		out := finishPage("article", in, page.Results, page.Partial,
			page.Total, page.TotalIsLowerBound, page.MorePages,
			paperRecord)
		return s.finishSearch(ctx, in, out)
	case "metadata":
		page, err := s.client.FindMetadata(ctx, in.Query, opts, timeout)
		if err != nil {
			return nil, wrap(err)
		}
		return finishPage("metadata", in, page.Results, page.Partial,
			page.Total, page.TotalIsLowerBound, page.MorePages,
			metadataRecord), nil
	default:
		return nil, invalid("kind %q: want book, article, metadata or openalex", in.Kind)
	}
}

func (s *Service) finishSearch(ctx context.Context, in SearchInput, out *SearchOutput) (*SearchOutput, error) {
	if in.IncludeStats {
		s.attachStats(ctx, out, in.Rank)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if in.Rank {
			rank(out.Results)
		}
	}
	applyLimit(in, out)
	return out, nil
}

func (s *Service) searchOpenAlex(ctx context.Context, in SearchInput) (*SearchOutput, error) {
	if s.papers == nil {
		return nil, fmt.Errorf("[CONFIG] OPENALEX_API_KEY is required for OpenAlex search")
	}
	res, err := s.papers.Search(ctx, papers.SearchOptions{
		Query: in.Query, Title: in.Title, Author: in.Author, Year: in.Year,
		Language: in.Language, WorkType: in.WorkType, Sort: in.Sort,
		Page: in.Page, Limit: limitOr(in.Limit, 10),
	})
	if err != nil {
		return nil, wrap(err)
	}
	out := &SearchOutput{Kind: "openalex", Total: res.Total, TotalIsLowerBound: res.TotalIsLowerBound, MorePages: res.MorePages}
	for _, w := range res.Results {
		out.Results = append(out.Results, workRecord(w))
	}
	for _, w := range res.PartialMatches {
		out.PartialMatches = append(out.PartialMatches, workRecord(w))
	}
	if len(out.PartialMatches) > maxPartial {
		out.PartialMatches = out.PartialMatches[:maxPartial]
	}
	out.Results = dedupe(out.Results)
	applyLimit(in, out)
	return out, nil
}

// Details returns the trimmed record for one id.
func (s *Service) Details(ctx context.Context, in DetailsInput) (*DetailsOutput, error) {
	timeout := timeoutOr(in.TimeoutSeconds, defaultDetailsTimeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	d, err := s.client.Details(ctx, in.ID, timeout)
	if err != nil {
		return nil, wrap(err)
	}
	desc := d.Description
	if len(desc) > maxDescription {
		desc = desc[:maxDescription] + "..."
	}
	return &DetailsOutput{
		ID: d.ID, Hash: d.Hash, URL: d.URL, Title: d.Title, Authors: d.Authors,
		Publisher: d.Publisher, Edition: d.Edition, Year: d.Year, Languages: d.Languages,
		Format: d.Format, SizeBytes: d.SizeBytes, Description: desc,
		Identifiers: d.Identifiers, Subjects: d.Subjects, Sources: d.Sources,
		FastDownload: d.FastDownload, Problems: d.Problems, Stats: d.Stats,
	}, nil
}

// Account returns membership and remaining fast downloads.
func (s *Service) Account(ctx context.Context, in AccountInput) (*anna.AccountStatus, error) {
	timeout := timeoutOr(in.TimeoutSeconds, defaultDetailsTimeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	st, err := s.client.AccountStatus(ctx, timeout)
	if err != nil {
		return nil, wrap(err)
	}
	return st, nil
}

// Read returns one bounded OpenAlex full-text passage.
func (s *Service) Read(ctx context.Context, in ReadInput) (*papers.ReadResult, error) {
	if s.papers == nil {
		return nil, fmt.Errorf("[CONFIG] OPENALEX_API_KEY is required for article_read")
	}
	timeout := timeoutOr(in.TimeoutSeconds, defaultSearchTimeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := s.papers.Read(ctx, papers.ReadOptions{ID: in.ID, Continuation: in.Continuation, Limit: in.Limit})
	if err != nil {
		return nil, wrap(err)
	}
	return res, nil
}

// Download fetches and verifies one file, reusing a matching local copy.
// One deadline bounds every sub-call in the operation.
func (s *Service) Download(ctx context.Context, in DownloadInput) (*DownloadOutput, error) {
	timeout := timeoutOr(in.TimeoutSeconds, defaultDownloadTimeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	given := 0
	for _, v := range []string{in.Hash, in.DOI, in.OpenAlexID} {
		if strings.TrimSpace(v) != "" {
			given++
		}
	}
	if given != 1 {
		return nil, invalid("give exactly one of hash, doi or openalex_id")
	}
	if in.OpenAlexID != "" {
		if s.papers == nil {
			return nil, fmt.Errorf("[CONFIG] OPENALEX_API_KEY is required for an OpenAlex download")
		}
		res, err := s.papers.Download(ctx, papers.DownloadOptions{OpenAlexID: in.OpenAlexID, Title: in.Title, Format: in.Format}, nil)
		if err != nil {
			return nil, wrap(err)
		}
		return &DownloadOutput{Path: res.Path, Bytes: res.Bytes}, nil
	}
	if in.Title == "" {
		// Name the file after its record instead of "untitled".
		if in.DOI != "" {
			if paper, err := s.client.LookupDOI(ctx, in.DOI, timeout); err == nil {
				in.Title = paper.Title
			}
		} else if d, err := s.client.Details(ctx, in.Hash, timeout); err == nil {
			in.Title = d.Title
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	if in.DOI != "" {
		res, err := s.client.DownloadArticle(ctx, anna.ArticleDownloadOptions{
			DOI: in.DOI, Title: in.Title, Format: in.Format,
		}, timeout, nil)
		if err != nil {
			return nil, wrap(err)
		}
		return downloadOutput(res), nil
	}
	res, err := s.client.DownloadBook(ctx, &anna.Book{
		Hash: in.Hash, Title: in.Title, Format: in.Format,
	}, timeout, nil)
	if err != nil {
		// Fall back to the article route only for a genuine not-found.
		if ctx.Err() == nil && (anna.CodeOf(err) == anna.NotFound || errors.Is(err, anna.ErrNotOnFastServers)) {
			art, aerr := s.client.DownloadArticle(ctx, anna.ArticleDownloadOptions{
				Hash: in.Hash, Title: in.Title, Format: in.Format,
			}, timeout, nil)
			if aerr == nil {
				return downloadOutput(art), nil
			}
		}
		return nil, wrap(err)
	}
	return downloadOutput(res), nil
}

func downloadOutput(r anna.DownloadResult) *DownloadOutput {
	return &DownloadOutput{Path: r.Path, Bytes: r.Bytes, MD5: r.MD5, Reused: r.Reused, DownloadsLeft: r.DownloadsLeft}
}

func wrap(err error) error {
	if code := anna.CodeOf(err); code != "" {
		return fmt.Errorf("[%s] %w", code, err)
	}
	return err
}

func timeoutOr(seconds int, fallback time.Duration) time.Duration {
	if seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return fallback
}

// UpstreamReachable probes the configured mirror with one HEAD request.
func (s *Service) UpstreamReachable(ctx context.Context) (string, int) {
	base := s.baseURL
	if base == "" {
		base = "https://annas-archive.gl"
	}
	if !strings.HasPrefix(base, "http") {
		base = "https://" + base
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, base, nil)
	if err != nil {
		return "unreachable", 0
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "unreachable", 0
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // HEAD probe body is empty
	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		return "ok", resp.StatusCode
	}
	return "challenged", resp.StatusCode
}
