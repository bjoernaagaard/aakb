package aakb

import (
	"cmp"
	"context"
	"slices"
	"strings"

	"github.com/bjoernaagaard/aakb/internal/openalex"
)

const (
	defaultLimit = 10
	maxLimit     = 50
	maxPartial   = 5
)

// Kind selects a search source and record type.
type Kind string

const (
	// KindAuto searches books or resolves a DOI to an article.
	KindAuto Kind = ""
	// KindBook searches books in Anna's Archive.
	KindBook Kind = "book"
	// KindArticle searches articles in Anna's Archive.
	KindArticle Kind = "article"
	// KindMetadata searches catalog records in Anna's Archive.
	KindMetadata Kind = "metadata"
	// KindOpenAlex searches scholarly works through OpenAlex.
	KindOpenAlex Kind = "openalex"
)

// Sort controls the order of search results.
type Sort string

const (
	// SortRelevance uses the provider's relevance order.
	SortRelevance Sort = ""
	// SortNewest puts recently published records first.
	SortNewest Sort = "newest"
	// SortOldest puts earliest publications first.
	SortOldest Sort = "oldest"
	// SortLargest puts the largest files first.
	SortLargest Sort = "largest"
	// SortSmallest puts the smallest files first.
	SortSmallest Sort = "smallest"
	// SortNewestAdded puts recently added records first.
	SortNewestAdded Sort = "newest_added"
	// SortOldestAdded puts the earliest additions first.
	SortOldestAdded Sort = "oldest_added"
	// SortRandom requests a random order.
	SortRandom Sort = "random"
	// SortMostCited puts the most cited OpenAlex works first.
	SortMostCited Sort = "most_cited"
)

// SearchRequest describes one search. Page defaults to 1 and Limit to 10
// (maximum 50). A leading minus sign excludes a filter value.
type SearchRequest struct {
	Kind                                                     Kind
	Query, Title, Author, Publisher, Year, WorkType          string
	OpenAlex                                                 OpenAlexFilter
	Formats, Languages, Sources, Content                     []string
	Sort                                                     Sort
	Page, Limit                                              int
	IncludeStats, Rank, DownloadableOnly, SearchDescriptions bool
}

// OpenAlexFilter narrows a KindOpenAlex search by OpenAlex identifiers.
// Relation is "citing", "references" or "related" and needs WorkID.
type OpenAlexFilter struct {
	AuthorID, InstitutionID, RepositoryID string
	Relation, WorkID                      string
}

func (f OpenAlexFilter) empty() bool {
	return strings.TrimSpace(f.AuthorID+f.InstitutionID+f.RepositoryID+f.Relation+f.WorkID) == ""
}

// SearchResult contains exact results and up to five looser matches.
// Both slices are non-nil. NextPage is set when MorePages is true.
type SearchResult struct {
	Kind              Kind     `json:"kind"`
	Results           []Record `json:"results"`
	PartialMatches    []Record `json:"partial_matches"`
	Total             int      `json:"total"`
	TotalIsLowerBound bool     `json:"total_is_lower_bound"`
	MorePages         bool     `json:"more_pages"`
	NextPage          int      `json:"next_page,omitempty"`
}

// Record is a search hit. OpenAlex hits have kind article and a W identifier.
type Record struct {
	Kind           string   `json:"kind"`
	Hash           string   `json:"hash,omitempty"`
	ID             string   `json:"id,omitempty"`
	Title          string   `json:"title"`
	Authors        string   `json:"authors,omitempty"`
	Publisher      string   `json:"publisher,omitempty"`
	Edition        string   `json:"edition,omitempty"`
	Year           string   `json:"year,omitempty"`
	Language       string   `json:"language,omitempty"`
	Format         string   `json:"format,omitempty"`
	Size           string   `json:"size,omitempty"`
	Content        string   `json:"content,omitempty"`
	Sources        []string `json:"sources,omitempty"`
	DOI            string   `json:"doi,omitempty"`
	URL            string   `json:"url,omitempty"`
	FastDownload   bool     `json:"fast_download"`
	PossibleIssues bool     `json:"possible_issues,omitempty"`
	Stats          *Stats   `json:"stats,omitempty"` // Nil when counts are unknown or were not requested.
	Score          int      `json:"score,omitempty"`
}

// Stats contains the archive's download and community counts.
type Stats struct {
	Downloads    int `json:"downloads"`
	GreatQuality int `json:"great_quality"`
	Reports      int `json:"reports"`
	Lists        int `json:"lists"`
	Comments     int `json:"comments"`
}

// Search returns one page. A DOI in auto or article mode resolves directly.
// If ctx has no deadline, Search applies a 60-second deadline.
func (c *Client) Search(ctx context.Context, req SearchRequest) (*SearchResult, error) {
	const op = "search"
	ctx, cancel := withDefaultTimeout(ctx, defaultTimeout)
	defer cancel()
	if err := classify(ctx, op, nil); err != nil {
		return nil, err
	}
	if req.Limit < 0 || req.Limit > maxLimit {
		return nil, newError(op, CodeInvalidArgument, "limit must be between 1 and 50, or zero for the default")
	}
	if req.Page < 0 {
		return nil, newError(op, CodeInvalidArgument, "page must be at least 1, or zero for the default")
	}
	req.Kind = Kind(strings.ToLower(strings.TrimSpace(string(req.Kind))))
	if strings.TrimSpace(req.Query) == "" && strings.TrimSpace(req.Title) == "" &&
		strings.TrimSpace(req.Author) == "" && strings.TrimSpace(req.Publisher) == "" &&
		strings.TrimSpace(req.Year) == "" && (req.Kind != KindOpenAlex || (strings.TrimSpace(req.WorkType) == "" && req.OpenAlex.empty())) {
		return nil, newError(op, CodeInvalidArgument, "provide a query or at least one of title, author, publisher or year")
	}
	req.Sort = Sort(strings.ToLower(strings.TrimSpace(string(req.Sort))))
	if req.Sort == "relevance" {
		req.Sort = SortRelevance
	}
	req.Page = max(1, req.Page)
	if req.Limit == 0 {
		req.Limit = defaultLimit
	}
	if req.Kind == KindOpenAlex {
		return c.searchOpenAlex(ctx, req)
	}
	if !req.OpenAlex.empty() {
		return nil, newError(op, CodeInvalidArgument, "author_id, institution_id, repository_id, relation and work_id need kind openalex")
	}
	if (req.Kind == KindAuto || req.Kind == KindArticle) && isDOIQuery(req.Query) {
		rec, err := c.archive.LookupDOI(ctx, req.Query)
		if err := classify(ctx, op, err); err != nil {
			return nil, err
		}
		return c.finishSearch(ctx, req, &SearchResult{
			Kind: KindArticle, Results: []Record{*rec}, Total: 1,
		})
	}

	switch req.Kind {
	case KindAuto, KindBook, KindArticle, KindMetadata:
	case KindOpenAlex:
		return c.searchOpenAlex(ctx, req)
	default:
		return nil, newError(op, CodeInvalidArgument, "kind must be book, article, metadata or openalex")
	}

	out, err := c.archive.Search(ctx, archiveSearchQuery{
		Kind:               req.Kind,
		Query:              req.Query,
		Title:              req.Title,
		Author:             req.Author,
		Publisher:          req.Publisher,
		Year:               req.Year,
		Formats:            req.Formats,
		Languages:          req.Languages,
		Sources:            req.Sources,
		Content:            req.Content,
		Sort:               req.Sort,
		Page:               req.Page,
		SearchDescriptions: req.SearchDescriptions,
	})
	if err := classify(ctx, op, err); err != nil {
		return nil, err
	}
	return c.finishSearch(ctx, req, out)
}

func (c *Client) searchOpenAlex(ctx context.Context, req SearchRequest) (*SearchResult, error) {
	const op = "search"
	if c.papers == nil {
		return nil, newError(op, CodeConfig, "OpenAlexKey is required for OpenAlex search")
	}
	if openalex.IsWorkID(req.Query) || openalex.NormalizeDOI(req.Query) != "" {
		if strings.TrimSpace(req.Title+req.Author+req.Publisher+req.Year+req.WorkType+
			req.OpenAlex.AuthorID+req.OpenAlex.InstitutionID+req.OpenAlex.RepositoryID+
			req.OpenAlex.Relation+req.OpenAlex.WorkID) != "" || len(req.Languages) > 0 || req.Page > 1 {
			return nil, newError(op, CodeInvalidArgument, "exact identifier lookup cannot combine discovery filters; use article_details or a keyword query")
		}
	}
	aaOnly := strings.TrimSpace(req.Publisher) != "" || len(req.Formats) > 0 || len(req.Sources) > 0 ||
		len(req.Content) > 0 || req.SearchDescriptions || req.IncludeStats || archiveOnlySort(req.Sort)
	oaOnly := strings.TrimSpace(req.WorkType) != "" || !req.OpenAlex.empty() ||
		openalex.IsWorkID(req.Query) || strings.Contains(req.Year, "-") || req.Sort == SortMostCited
	if aaOnly && oaOnly {
		return nil, newError(op, CodeInvalidArgument, "OpenAlex-only and Anna-only search filters cannot be combined")
	}
	if aaOnly {
		kind := req.Kind
		if kind != KindBook && kind != KindArticle && kind != KindMetadata {
			kind = KindArticle
		}
		out, err := c.archive.Search(ctx, archiveSearchQuery{
			Kind:               kind,
			Query:              req.Query,
			Title:              req.Title,
			Author:             req.Author,
			Publisher:          req.Publisher,
			Year:               req.Year,
			Formats:            req.Formats,
			Languages:          req.Languages,
			Sources:            req.Sources,
			Content:            req.Content,
			Sort:               req.Sort,
			Page:               req.Page,
			SearchDescriptions: req.SearchDescriptions,
		})
		if err := classify(ctx, op, err); err != nil {
			return nil, err
		}
		page, err := c.finishSearch(ctx, req, out)
		if err != nil {
			return nil, err
		}
		page.Kind = KindOpenAlex
		return page, nil
	}
	out, err := c.papers.Search(ctx, openAlexSearchRequest{
		Query:            req.Query,
		Title:            req.Title,
		Author:           req.Author,
		Year:             req.Year,
		WorkType:         req.WorkType,
		Languages:        req.Languages,
		Sort:             req.Sort,
		Page:             req.Page,
		Limit:            req.Limit,
		DownloadableOnly: req.DownloadableOnly,
		Filter:           req.OpenAlex,
	})
	if err := classify(ctx, op, err); err != nil {
		return nil, err
	}
	out.Results = dedupe(out.Results)
	normalizeSearch(req, out)
	return out, nil
}

// archiveOnlySort reports sorts that only Anna's Archive supports.
func archiveOnlySort(sort Sort) bool {
	switch sort {
	case SortLargest, SortSmallest, SortNewestAdded, SortOldestAdded, SortRandom:
		return true
	default:
		return false
	}
}

func (c *Client) finishSearch(ctx context.Context, req SearchRequest, out *SearchResult) (*SearchResult, error) {
	if req.DownloadableOnly {
		out.Results = filterDownloadable(out.Results)
		out.PartialMatches = filterDownloadable(out.PartialMatches)
	}
	out.Results = dedupe(out.Results)
	if req.Rank && req.Sort == SortRelevance {
		rank(out.Results)
	}
	if req.IncludeStats {
		c.attachStats(ctx, out)
		if err := classify(ctx, "search", nil); err != nil {
			return nil, err
		}
		if req.Rank && req.Sort == SortRelevance {
			rank(out.Results)
		}
	}
	normalizeSearch(req, out)
	return out, nil
}

func normalizeSearch(req SearchRequest, out *SearchResult) {
	out.Results = out.Results[:min(len(out.Results), req.Limit)]
	if out.PartialMatches == nil {
		out.PartialMatches = []Record{}
	}
	out.PartialMatches = out.PartialMatches[:min(len(out.PartialMatches), maxPartial)]
	if out.MorePages {
		out.NextPage = req.Page + 1
	}
}

func filterDownloadable(in []Record) []Record {
	out := make([]Record, 0, len(in))
	for _, r := range in {
		if r.FastDownload || (r.Kind == "article" && r.DOI != "") {
			out = append(out, r)
		}
	}
	return out
}

// dedupe drops repeat copies of the same file, then repeat editions with
// the same title, author, format and size, keeping the first seen.
func dedupe(in []Record) []Record {
	seen := make(map[string]bool, len(in))
	out := make([]Record, 0, len(in))
	for _, r := range in {
		keys := []string{
			strings.Join([]string{
				strings.ToLower(r.Title), strings.ToLower(r.Authors),
				strings.ToLower(r.Format), r.Size,
			}, "|"),
		}
		if r.Hash != "" {
			keys = append(keys, "hash:"+r.Hash)
		}
		dup := false
		for _, k := range keys {
			if seen[k] {
				dup = true
			}
		}
		if dup {
			continue
		}
		for _, k := range keys {
			seen[k] = true
		}
		out = append(out, r)
	}
	return out
}

// rank scores availability and reported quality. Popularity counts only
// when stats were requested, so unranked fields keep upstream order.
func rank(in []Record) {
	for i, r := range in {
		score := 0
		if r.FastDownload {
			score += 100
		}
		if r.PossibleIssues {
			score -= 50
		}
		if r.Stats != nil {
			score += min(r.Stats.Downloads, 40)
			score += min(r.Stats.GreatQuality, 10)
			score -= min(r.Stats.Reports, 20)
		}
		in[i].Score = score
	}
	slices.SortStableFunc(in, func(a, b Record) int {
		return cmp.Compare(b.Score, a.Score)
	})
}

// attachStats fills Stats for exact results only, from one bounded
// lookup. A failed lookup leaves Stats nil, which means unknown,
// not zero. The caller checks ctx afterwards.
func (c *Client) attachStats(ctx context.Context, out *SearchResult) {
	limit := 10
	var hashes []string
	for _, r := range out.Results {
		if len(hashes) >= limit {
			break
		}
		if r.Hash != "" {
			hashes = append(hashes, r.Hash)
		}
	}
	if len(hashes) == 0 {
		return
	}
	stats, err := c.archive.FileStats(ctx, hashes)
	if err != nil {
		return
	}
	for i, r := range out.Results {
		if st, ok := stats[r.Hash]; ok {
			out.Results[i].Stats = new(st)
		}
	}
}
