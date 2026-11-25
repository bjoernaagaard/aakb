package aakb

import (
	"context"
	"errors"
	"maps"
	"path/filepath"
	"slices"

	"github.com/SokolskyNikita/annas-mcp/anna"
	"github.com/SokolskyNikita/annas-mcp/papers"
)

// classify uses fixed messages because provider errors can contain request
// URLs, server bodies, and user-supplied values.
func classify(ctx context.Context, op string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		err = ctxErr
	}
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled):
		return &Error{Code: CodeCanceled, Op: op, Message: "operation canceled", cause: context.Canceled}
	case errors.Is(err, context.DeadlineExceeded):
		return &Error{Code: CodeTimeout, Op: op, Message: "operation timed out", cause: context.DeadlineExceeded}
	}

	switch anna.CodeOf(err) {
	case anna.InvalidArgument:
		return newError(op, CodeInvalidArgument, "provider rejected the request parameters")
	case anna.CodeConfig:
		return newError(op, CodeConfig, "provider access requires valid configuration and credentials")
	case anna.NotFound:
		return newError(op, CodeNotFound, "provider could not find the requested item")
	case anna.UpstreamBlocked:
		return newError(op, CodeUpstreamBlocked, "provider blocked access to the request")
	case anna.RequestTimeout:
		return newError(op, CodeTimeout, "provider request timed out")
	case anna.Upstream:
		return newError(op, CodeUpstream, "provider could not complete the request")
	default:
		return newError(op, CodeInternal, "unexpected provider failure")
	}
}

func searchOptions(req SearchRequest) anna.SearchOptions {
	var fields []anna.SearchField
	for _, f := range []anna.SearchField{
		{Name: "title", Value: req.Title}, {Name: "author", Value: req.Author},
		{Name: "publisher", Value: req.Publisher}, {Name: "year", Value: req.Year},
	} {
		if f.Value != "" {
			fields = append(fields, f)
		}
	}
	return anna.SearchOptions{
		Page: req.Page, Sort: string(req.Sort), Format: req.Formats,
		Language: req.Languages, Source: req.Sources, Content: req.Content,
		Fields: fields, Descriptions: req.SearchDescriptions,
	}
}

func convertPage[T any](kind Kind, page anna.SearchPage[T], convert func(T) Record) *SearchResult {
	return &SearchResult{
		Kind: kind, Total: page.Total, TotalIsLowerBound: page.TotalIsLowerBound, MorePages: page.MorePages,
		Results: convertAll(page.Results, convert), PartialMatches: convertAll(page.Partial, convert),
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

func workRecord(w *papers.Work) Record {
	return Record{
		Kind: "article", ID: w.OpenAlexID, Hash: w.Hash, Title: w.Title,
		Authors: w.Authors, Publisher: w.Journal, Year: w.Year, Format: w.Format,
		Size: w.Size, Sources: slices.Clone(w.Sources), DOI: w.DOI, URL: w.PageURL,
		FastDownload: w.FastDownload, PossibleIssues: w.PossibleIssues, Stats: recordStats(w.Stats),
	}
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

func articleResult(r *papers.ReadResult) *Article {
	out := &Article{
		OpenAlexID: r.OpenAlexID, Title: r.Title, URL: r.URL, Source: r.Source, Text: r.Text,
		Passages: convertAll(r.Passages, func(p papers.Passage) Passage { return Passage(p) }),
		SHA256:   r.Digest, Warnings: slices.Clone(r.Warnings),
	}
	if r.NextArgs != nil {
		out.Continuation = r.NextArgs.Continuation
	}
	return out
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
