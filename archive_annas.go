package aakb

import (
	"context"
	"maps"
	"path/filepath"
	"slices"

	"github.com/bjoernaagaard/aakb/internal/annas"
)

// annasArchiveProvider implements archiveProvider on internal/annas with
// result mapping to root types.
type annasArchiveProvider struct {
	client *annas.Client
}

func newAnnasArchiveProvider(client *annas.Client) *annasArchiveProvider {
	return &annasArchiveProvider{client: client}
}

func (p *annasArchiveProvider) Search(ctx context.Context, query archiveSearchQuery) (*SearchResult, error) {
	opts := annas.SearchOptions{
		Page:         query.Page,
		Sort:         string(query.Sort),
		Format:       query.Formats,
		Language:     query.Languages,
		Source:       query.Sources,
		Content:      query.Content,
		Fields:       annasSearchFields(query),
		Descriptions: query.SearchDescriptions,
	}
	switch query.Kind {
	case KindAuto, KindBook:
		page, err := p.client.SearchBooks(ctx, query.Query, opts)
		if err != nil {
			return nil, err
		}
		return annasFilePage(KindBook, page, annasBookRecord), nil
	case KindArticle:
		page, err := p.client.SearchArticles(ctx, query.Query, opts)
		if err != nil {
			return nil, err
		}
		return annasFilePage(KindArticle, page, annasPaperRecord), nil
	case KindMetadata:
		page, err := p.client.SearchMetadata(ctx, query.Query, opts)
		if err != nil {
			return nil, err
		}
		return &SearchResult{
			Kind:              KindMetadata,
			Total:             page.Total,
			TotalIsLowerBound: page.TotalIsLowerBound,
			MorePages:         page.MorePages,
			Results:           convertAll(page.Results, annasMetadataRecord),
			PartialMatches:    convertAll(page.Partial, annasMetadataRecord),
		}, nil
	case KindOpenAlex:
		return nil, newError("search", CodeInvalidArgument, "kind must be book, article, metadata or openalex")
	default:
		return nil, newError("search", CodeInvalidArgument, "kind must be book, article, metadata or openalex")
	}
}

func (p *annasArchiveProvider) LookupDOI(ctx context.Context, doi string) (*Record, error) {
	paper, err := p.client.LookupDOI(ctx, doi)
	if err != nil {
		return nil, err
	}
	rec := annasPaperRecord(paper)
	return &rec, nil
}

func (p *annasArchiveProvider) Details(ctx context.Context, id string) (*RecordDetails, error) {
	details, err := p.client.Details(ctx, id)
	if err != nil {
		return nil, err
	}
	return annasDetails(details), nil
}

func (p *annasArchiveProvider) FileStats(ctx context.Context, hashes []string) (map[string]Stats, error) {
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

func (p *annasArchiveProvider) DownloadHash(ctx context.Context, req archiveDownloadRequest) (*DownloadResult, error) {
	res, err := p.client.DownloadHash(ctx, annas.DownloadRequest{
		Hash:     req.Hash,
		Title:    req.Title,
		Format:   req.Format,
		Progress: req.Progress,
	})
	if err != nil {
		return nil, err
	}
	return annasDownloadResult(res), nil
}

func (p *annasArchiveProvider) DownloadDOI(ctx context.Context, req archiveDownloadRequest) (*DownloadResult, error) {
	res, err := p.client.DownloadDOI(ctx, annas.DownloadDOIRequest{
		DOI:           req.DOI,
		Hash:          req.Hash,
		Title:         req.Title,
		ExpectedTitle: req.ExpectedTitle,
		Format:        req.Format,
		Progress:      req.Progress,
	})
	if err != nil {
		return nil, err
	}
	return annasDownloadResult(res), nil
}

func (p *annasArchiveProvider) AccountStatus(ctx context.Context) (*Account, error) {
	status, err := p.client.AccountStatus(ctx)
	if err != nil {
		return nil, err
	}
	return annasAccount(status), nil
}

func (p *annasArchiveProvider) ProbeHealth(ctx context.Context) UpstreamStatus {
	health := p.client.ProbeHealth(ctx)
	return UpstreamStatus{
		Status:     health.Status,
		HTTPStatus: health.HTTPStatus,
	}
}

func annasSearchFields(req archiveSearchQuery) []annas.SearchField {
	var fields []annas.SearchField
	for _, f := range []annas.SearchField{
		{Name: "title", Value: req.Title},
		{Name: "author", Value: req.Author},
		{Name: "publisher", Value: req.Publisher},
		{Name: "year", Value: req.Year},
	} {
		if f.Value != "" {
			fields = append(fields, f)
		}
	}
	return fields
}

func annasFilePage[T any](kind Kind, page annas.SearchPage[T], convert func(T) Record) *SearchResult {
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

func annasBookRecord(b *annas.Book) Record {
	return Record{
		Kind: "book", Hash: b.Hash, Title: b.Title, Authors: b.Authors,
		Publisher: b.Publisher, Edition: b.Edition, Year: b.Year,
		Language: b.Language, Format: b.Format, Size: b.Size, Content: b.Content,
		Sources: slices.Clone(b.Sources), DOI: b.DOI, URL: b.URL,
		FastDownload: b.FastDownload, PossibleIssues: b.PossibleIssues, Stats: annasRecordStats(b.Stats),
	}
}

func annasPaperRecord(p *annas.Paper) Record {
	return Record{
		Kind: "article", Hash: p.Hash, Title: p.Title, Authors: p.Authors,
		Publisher: p.Journal, Year: p.Year, Format: p.Format, Size: p.Size,
		Sources: slices.Clone(p.Sources), DOI: p.DOI, URL: p.PageURL,
		FastDownload: p.FastDownload, PossibleIssues: p.PossibleIssues, Stats: annasRecordStats(p.Stats),
	}
}

func annasMetadataRecord(m *annas.MetadataRecord) Record {
	return Record{
		Kind: "metadata", ID: m.ID, Title: m.Title, Authors: m.Authors,
		Publisher: m.Publisher, Edition: m.Edition, Year: m.Year,
		Language: m.Language, URL: m.URL,
	}
}

func annasRecordStats(stats *annas.RecordStats) *Stats {
	if stats == nil {
		return nil
	}
	return new(Stats(*stats))
}

func annasDetails(d *annas.Details) *RecordDetails {
	identifiers := maps.Clone(d.Identifiers)
	for key, values := range identifiers {
		identifiers[key] = slices.Clone(values)
	}
	return &RecordDetails{
		ID: d.ID, Hash: d.Hash, URL: d.URL, Title: d.Title, Authors: d.Authors,
		Publisher: d.Publisher, Edition: d.Edition, Year: d.Year, Languages: slices.Clone(d.Languages),
		Format: d.Format, SizeBytes: d.SizeBytes, Description: shortenDescription(d.Description),
		Identifiers: identifiers, Subjects: slices.Clone(d.Subjects), Sources: slices.Clone(d.Sources),
		FastDownload: d.FastDownload, Stats: annasRecordStats(d.Stats),
		Problems: convertAll(d.Problems, func(p annas.Problem) Problem { return Problem(p) }),
	}
}

func annasDownloadResult(r *annas.DownloadResult) *DownloadResult {
	verification := VerificationNone
	if r.MD5 != "" {
		verification = VerificationMD5
	}
	name := r.Name
	if name == "" && r.Path != "" {
		name = filepath.Base(r.Path)
	}
	out := &DownloadResult{
		Path: r.Path, Name: name, Bytes: r.Bytes, Verification: verification,
		MD5: r.MD5, Reused: r.Reused,
	}
	if r.DownloadsLeft != nil {
		out.DownloadsLeft = new(*r.DownloadsLeft)
	}
	return out
}

func annasAccount(st *annas.AccountStatus) *Account {
	out := &Account{
		Member: st.Member, DownloadsLeft: st.DownloadsLeft, DownloadsLimit: st.DownloadsLimit,
		WindowHours: st.WindowHours, Memberships: slices.Clone(st.Memberships),
	}
	if st.SearchAllowed != nil {
		out.SearchAllowed = new(*st.SearchAllowed)
	}
	return out
}
