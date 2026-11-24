// Package service is the shared core behind the MCP tools and the CLI.
// It wraps the anna client with unified inputs, dedupe and ranking, so
// both adapters return the same shapes. Use it only for material you
// are entitled to obtain.
package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/SokolskyNikita/annas-mcp/anna"

	"github.com/bjoernaagaard/aakb/internal/config"
)

const (
	defaultSearchTimeout   = 60 * time.Second
	defaultDetailsTimeout  = 60 * time.Second
	defaultDownloadTimeout = 30 * time.Minute
	maxLimit               = 50
)

// Service performs Anna's Archive operations for one configuration.
type Service struct {
	client *anna.Client
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
	return &Service{client: client}, nil
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
	Kind               string `json:"kind,omitempty" jsonschema:"book, article or metadata; empty auto-detects a DOI as an article"`
	Query              string `json:"query,omitempty" jsonschema:"title, author, topic, ISBN, DOI or MD5"`
	Title              string `json:"title,omitempty" jsonschema:"phrase the title must contain"`
	Author             string `json:"author,omitempty" jsonschema:"phrase the author must contain"`
	Publisher          string `json:"publisher,omitempty" jsonschema:"phrase the publisher must contain"`
	Year               string `json:"year,omitempty" jsonschema:"publication year, such as 2008"`
	Format             string `json:"format,omitempty" jsonschema:"comma-separated formats such as epub,pdf; prefix - to exclude"`
	Language           string `json:"language,omitempty" jsonschema:"comma-separated language codes such as en,de"`
	Source             string `json:"source,omitempty" jsonschema:"comma-separated source libraries or catalogs"`
	Content            string `json:"content,omitempty" jsonschema:"comma-separated content types such as book_nonfiction,magazine"`
	Sort               string `json:"sort,omitempty" jsonschema:"newest, oldest, largest, smallest, newest_added or oldest_added"`
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

// DownloadInput selects exactly one file, by hash or by DOI.
type DownloadInput struct {
	Hash           string `json:"hash,omitempty" jsonschema:"MD5 hash or Anna's Archive /md5/ URL"`
	DOI            string `json:"doi,omitempty" jsonschema:"paper DOI, resolved to a file first"`
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
	timeout := timeoutOr(in.TimeoutSeconds, defaultSearchTimeout)
	opts, err := searchOptions(in)
	if err != nil {
		return nil, err
	}
	if (kind == "" || kind == "article") && anna.IsDOIQuery(in.Query) {
		paper, err := s.client.LookupDOI(ctx, in.Query, timeout)
		if err != nil {
			return nil, wrap(err)
		}
		rec := paperRecord(paper)
		return &SearchOutput{Kind: "article", Results: []*Record{rec}, Total: 1}, nil
	}
	switch kind {
	case "", "book":
		page, err := s.client.FindBook(ctx, in.Query, opts, timeout)
		if err != nil {
			return nil, wrap(err)
		}
		out := finishPage("book", in, page.Results, page.Partial,
			page.Total, page.TotalIsLowerBound, page.MorePages,
			func(b *anna.Book) *Record { return bookRecord(b) })
		if in.IncludeStats {
			s.attachStats(ctx, out, in.Rank)
		}
		return out, nil
	case "article":
		page, err := s.client.FindArticle(ctx, in.Query, opts, timeout)
		if err != nil {
			return nil, wrap(err)
		}
		out := finishPage("article", in, page.Results, page.Partial,
			page.Total, page.TotalIsLowerBound, page.MorePages,
			func(p *anna.Paper) *Record { return paperRecord(p) })
		if in.IncludeStats {
			s.attachStats(ctx, out, in.Rank)
		}
		return out, nil
	case "metadata":
		page, err := s.client.FindMetadata(ctx, in.Query, opts, timeout)
		if err != nil {
			return nil, wrap(err)
		}
		return finishPage("metadata", in, page.Results, page.Partial,
			page.Total, page.TotalIsLowerBound, page.MorePages,
			func(m *anna.MetadataRecord) *Record { return metadataRecord(m) }), nil
	default:
		return nil, fmt.Errorf("[INVALID_ARGUMENT] kind %q: want book, article or metadata", in.Kind)
	}
}

// Details returns the full record for one id.
func (s *Service) Details(ctx context.Context, in DetailsInput) (*anna.Details, error) {
	d, err := s.client.Details(ctx, in.ID, timeoutOr(in.TimeoutSeconds, defaultDetailsTimeout))
	if err != nil {
		return nil, wrap(err)
	}
	return d, nil
}

// Account returns membership and remaining fast downloads.
func (s *Service) Account(ctx context.Context, in AccountInput) (*anna.AccountStatus, error) {
	st, err := s.client.AccountStatus(ctx, timeoutOr(in.TimeoutSeconds, defaultDetailsTimeout))
	if err != nil {
		return nil, wrap(err)
	}
	return st, nil
}

// Download fetches and verifies one file, reusing a matching local copy.
func (s *Service) Download(ctx context.Context, in DownloadInput) (*DownloadOutput, error) {
	timeout := timeoutOr(in.TimeoutSeconds, defaultDownloadTimeout)
	hasHash := strings.TrimSpace(in.Hash) != ""
	hasDOI := strings.TrimSpace(in.DOI) != ""
	if hasHash == hasDOI {
		return nil, fmt.Errorf("[INVALID_ARGUMENT] give exactly one of hash or doi")
	}
	if hasDOI {
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
		// An article hash is not on the book route; retry there once.
		art, aerr := s.client.DownloadArticle(ctx, anna.ArticleDownloadOptions{
			Hash: in.Hash, Title: in.Title, Format: in.Format,
		}, timeout, nil)
		if aerr == nil {
			return downloadOutput(art), nil
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
