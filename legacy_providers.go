package aakb

import (
	"context"
	"maps"
	"net/http"
	"path/filepath"
	"slices"
	"strings"

	"github.com/SokolskyNikita/annas-mcp/anna"

	"github.com/bjoernaagaard/aakb/internal/httpx"
)

type legacyArchiveProvider struct {
	client     *anna.Client
	baseURL    string
	healthHTTP *httpx.Client
}

func newLegacyArchiveProvider(client *anna.Client, baseURL string, healthHTTP *httpx.Client) *legacyArchiveProvider {
	return &legacyArchiveProvider{
		client:     client,
		baseURL:    baseURL,
		healthHTTP: healthHTTP,
	}
}

func (p *legacyArchiveProvider) Search(ctx context.Context, query archiveSearchQuery) (*SearchResult, error) {
	opts := legacySearchOptions(query)
	switch query.Kind {
	case KindAuto, KindBook:
		page, err := p.client.FindBook(ctx, query.Query, opts, 0)
		if err != nil {
			return nil, err
		}
		return convertPage(KindBook, page, bookRecord), nil
	case KindArticle:
		page, err := p.client.FindArticle(ctx, query.Query, opts, 0)
		if err != nil {
			return nil, err
		}
		return convertPage(KindArticle, page, paperRecord), nil
	case KindMetadata:
		page, err := p.client.FindMetadata(ctx, query.Query, opts, 0)
		if err != nil {
			return nil, err
		}
		return convertPage(KindMetadata, page, metadataRecord), nil
	case KindOpenAlex:
		return nil, newError("search", CodeInvalidArgument, "kind must be book, article, metadata or openalex")
	default:
		return nil, newError("search", CodeInvalidArgument, "kind must be book, article, metadata or openalex")
	}
}

func (p *legacyArchiveProvider) LookupDOI(ctx context.Context, doi string) (*Record, error) {
	paper, err := p.client.LookupDOI(ctx, doi, 0)
	if err != nil {
		return nil, err
	}
	rec := paperRecord(paper)
	return &rec, nil
}

func (p *legacyArchiveProvider) Details(ctx context.Context, id string) (*RecordDetails, error) {
	details, err := p.client.Details(ctx, id, 0)
	if err != nil {
		return nil, err
	}
	return recordDetails(details), nil
}

func (p *legacyArchiveProvider) FileStats(ctx context.Context, hashes []string) (map[string]Stats, error) {
	raw, err := p.client.FileStats(ctx, hashes)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Stats, len(raw))
	for hash, st := range raw {
		out[hash] = Stats(st)
	}
	return out, nil
}

func (p *legacyArchiveProvider) DownloadHash(ctx context.Context, req archiveDownloadRequest) (*DownloadResult, error) {
	progress := anna.ProgressFunc(req.Progress)
	res, err := p.client.DownloadBook(ctx, &anna.Book{Hash: req.Hash, Title: req.Title, Format: req.Format}, 0, progress)
	if err != nil && ctx.Err() == nil && anna.CodeOf(err) == anna.NotFound {
		article := anna.ArticleDownloadOptions{Hash: req.Hash, Title: req.Title, Format: req.Format}
		fallback, fallbackErr := p.client.DownloadArticle(ctx, article, 0, progress)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if fallbackErr == nil {
			return downloadResult(fallback), nil
		}
	}
	if err != nil {
		return nil, err
	}
	return downloadResult(res), nil
}

func (p *legacyArchiveProvider) DownloadDOI(ctx context.Context, req archiveDownloadRequest) (*DownloadResult, error) {
	article := anna.ArticleDownloadOptions{
		DOI:           req.DOI,
		Title:         req.Title,
		ExpectedTitle: req.ExpectedTitle,
		Format:        req.Format,
	}
	res, err := p.client.DownloadArticle(ctx, article, 0, anna.ProgressFunc(req.Progress))
	if err != nil {
		return nil, err
	}
	return downloadResult(res), nil
}

func (p *legacyArchiveProvider) AccountStatus(ctx context.Context) (*Account, error) {
	status, err := p.client.AccountStatus(ctx, 0)
	if err != nil {
		return nil, err
	}
	return accountResult(status), nil
}

func (p *legacyArchiveProvider) ProbeHealth(ctx context.Context) UpstreamStatus {
	base := strings.TrimSpace(p.baseURL)
	if base == "" {
		base = defaultBaseURL
	}
	if !strings.Contains(base, "://") {
		base = "https://" + base
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, base, nil)
	if err != nil {
		return UpstreamStatus{Status: "unreachable"}
	}
	resp, err := p.healthHTTP.Do(req)
	if err != nil {
		return UpstreamStatus{Status: "unreachable"}
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // HEAD probe response body is empty
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusBadRequest {
		return UpstreamStatus{Status: "ok", HTTPStatus: resp.StatusCode}
	}
	return UpstreamStatus{Status: "challenged", HTTPStatus: resp.StatusCode}
}

func legacySearchOptions(req archiveSearchQuery) anna.SearchOptions {
	var fields []anna.SearchField
	for _, f := range []anna.SearchField{
		{Name: "title", Value: req.Title},
		{Name: "author", Value: req.Author},
		{Name: "publisher", Value: req.Publisher},
		{Name: "year", Value: req.Year},
	} {
		if f.Value != "" {
			fields = append(fields, f)
		}
	}
	return anna.SearchOptions{
		Page:         req.Page,
		Sort:         string(req.Sort),
		Format:       req.Formats,
		Language:     req.Languages,
		Source:       req.Sources,
		Content:      req.Content,
		Fields:       fields,
		Descriptions: req.SearchDescriptions,
	}
}

func convertPage[T any](kind Kind, page anna.SearchPage[T], convert func(T) Record) *SearchResult {
	return &SearchResult{
		Kind:              kind,
		Total:             page.Total,
		TotalIsLowerBound: page.TotalIsLowerBound,
		MorePages:         page.MorePages,
		Results:           convertAll(page.Results, convert),
		PartialMatches:    convertAll(page.Partial, convert),
	}
}

func convertAll[T, U any](items []T, convert func(T) U) []U {
	if items == nil {
		return nil
	}
	out := make([]U, len(items))
	for i, item := range items {
		out[i] = convert(item)
	}
	return out
}

func recordStats(stats *anna.RecordStats) *Stats {
	if stats == nil {
		return nil
	}
	return new(Stats(*stats))
}

func bookRecord(b *anna.Book) Record {
	return Record{
		Kind: "book", Hash: b.Hash, Title: b.Title, Authors: b.Authors,
		Publisher: b.Publisher, Edition: b.Edition, Year: b.Year,
		Language: b.Language, Format: b.Format, Size: b.Size, Content: b.Content,
		Sources: slices.Clone(b.Sources), DOI: b.DOI, URL: b.URL,
		FastDownload: b.FastDownload, PossibleIssues: b.PossibleIssues, Stats: recordStats(b.Stats),
	}
}

func paperRecord(p *anna.Paper) Record {
	return Record{
		Kind: "article", Hash: p.Hash, Title: p.Title, Authors: p.Authors,
		Publisher: p.Journal, Year: p.Year, Format: p.Format, Size: p.Size,
		Sources: slices.Clone(p.Sources), DOI: p.DOI, URL: p.PageURL,
		FastDownload: p.FastDownload, PossibleIssues: p.PossibleIssues, Stats: recordStats(p.Stats),
	}
}

func metadataRecord(m *anna.MetadataRecord) Record {
	return Record{
		Kind: "metadata", ID: m.ID, Title: m.Title, Authors: m.Authors,
		Publisher: m.Publisher, Edition: m.Edition, Year: m.Year,
		Language: m.Language, URL: m.URL,
	}
}

func recordDetails(d *anna.Details) *RecordDetails {
	identifiers := maps.Clone(d.Identifiers)
	for key, values := range identifiers {
		identifiers[key] = slices.Clone(values)
	}
	return &RecordDetails{
		ID: d.ID, Hash: d.Hash, URL: d.URL, Title: d.Title, Authors: d.Authors,
		Publisher: d.Publisher, Edition: d.Edition, Year: d.Year, Languages: slices.Clone(d.Languages),
		Format: d.Format, SizeBytes: d.SizeBytes, Description: shortenDescription(d.Description),
		Identifiers: identifiers, Subjects: slices.Clone(d.Subjects), Sources: slices.Clone(d.Sources),
		FastDownload: d.FastDownload, Stats: recordStats(d.Stats),
		Problems: convertAll(d.Problems, func(p anna.Problem) Problem { return Problem(p) }),
	}
}

func downloadResult(r anna.DownloadResult) *DownloadResult {
	verification := VerificationNone
	if r.MD5 != "" {
		verification = VerificationMD5
	}
	out := &DownloadResult{
		Path: r.Path, Name: filepath.Base(r.Path), Bytes: r.Bytes, Verification: verification,
		MD5: r.MD5, Reused: r.Reused,
	}
	if r.DownloadsLeft != nil {
		out.DownloadsLeft = new(*r.DownloadsLeft)
	}
	return out
}

func accountResult(st *anna.AccountStatus) *Account {
	out := &Account{
		Member: st.Member, DownloadsLeft: st.DownloadsLeft, DownloadsLimit: st.DownloadsLimit,
		WindowHours: st.WindowHours, Memberships: slices.Clone(st.Memberships),
	}
	if st.SearchAllowed != nil {
		out.SearchAllowed = new(*st.SearchAllowed)
	}
	return out
}
