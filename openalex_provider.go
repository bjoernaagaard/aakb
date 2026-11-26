package aakb

import (
	"context"
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bjoernaagaard/aakb/internal/httpx"
	"github.com/bjoernaagaard/aakb/internal/openalex"
	"github.com/bjoernaagaard/aakb/internal/store"
)

// openalexProvider implements papersProvider on internal/openalex with result
// mapping to root types. Archive fallback preserves the previous
// auto-provider behavior: a failed or empty OpenAlex search retries through
// Anna's Archive journals, and an OpenAlex download without a compatible copy
// retries a DOI through Anna's Archive.
type openalexProvider struct {
	client  *openalex.Client
	files   *store.Store
	archive archiveProvider
}

func newOpenAlexProvider(client *openalex.Client, files *store.Store, archive archiveProvider) *openalexProvider {
	return &openalexProvider{client: client, files: files, archive: archive}
}

func (p *openalexProvider) Search(ctx context.Context, req openAlexSearchRequest) (*SearchResult, error) {
	if openalex.IsWorkID(req.Query) || openalex.NormalizeDOI(req.Query) != "" {
		return p.searchExact(ctx, req)
	}
	// OpenAlex full-text hosting covers a fraction of works, so the query
	// stays unfiltered and DownloadableOnly applies to the returned page
	// locally: hosted PDF or open-access external PDF.
	workTypes := parseWorkTypes(req.WorkType, req.Filter.Relation)
	remote := openalex.SearchOptions{
		Query: req.Query, Title: req.Title, Author: req.Author,
		AuthorID: req.Filter.AuthorID, InstitutionID: req.Filter.InstitutionID,
		RepositoryID: req.Filter.RepositoryID, Year: req.Year,
		Language:  strings.Join(req.Languages, ","),
		WorkTypes: workTypes,
		Sort:      string(req.Sort),
		Page:      req.Page, PerPage: req.Limit,
		Relation: req.Filter.Relation, WorkID: req.Filter.WorkID,
	}
	page, err := p.client.Search(ctx, remote)
	if err != nil {
		if req.Page == 1 && p.archive != nil && !openAlexOnly(req) && fallbackAllowed(ctx, err) {
			if fallback, fallbackErr := p.searchArchiveFallback(ctx, req); fallbackErr == nil {
				return fallback, nil
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}
		return nil, err
	}
	if page.Meta.Count == 0 && req.Page == 1 && p.archive != nil && !openAlexOnly(req) {
		if fallback, fallbackErr := p.searchArchiveFallback(ctx, req); fallbackErr == nil {
			return fallback, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	out := &SearchResult{Kind: KindOpenAlex, Total: page.Meta.Count, Results: []Record{}}
	for i := range page.Results {
		if req.DownloadableOnly && !openalex.HasDownload(&page.Results[i]) {
			continue
		}
		out.Results = append(out.Results, mapOpenAlexWork(&page.Results[i]))
	}
	if req.DownloadableOnly {
		out.TotalIsLowerBound = true
		out.Total = len(out.Results)
	}
	out.MorePages = moreOpenAlexPages(page.Meta.Count, "", req.Page, req.Limit)
	return out, nil
}

// openAlexOnly reports whether the request carries an OpenAlex-only filter
// that Anna's Archive cannot serve, so failures must not fall back to it.
func openAlexOnly(req openAlexSearchRequest) bool {
	return strings.TrimSpace(req.WorkType) != "" || !req.Filter.empty() ||
		openalex.IsWorkID(req.Query) || strings.Contains(req.Year, "-") ||
		req.Sort == SortMostCited
}

func (p *openalexProvider) searchExact(ctx context.Context, req openAlexSearchRequest) (*SearchResult, error) {
	work, err := p.client.Lookup(ctx, req.Query)
	if err != nil {
		if p.archive != nil && !openAlexOnly(req) && fallbackAllowed(ctx, err) {
			if fallback, fallbackErr := p.searchArchiveFallback(ctx, req); fallbackErr == nil {
				return fallback, nil
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}
		return nil, err
	}
	out := &SearchResult{Kind: KindOpenAlex, Total: 0, Results: []Record{}}
	if !req.DownloadableOnly || openalex.HasDownload(work) {
		out.Results = []Record{mapOpenAlexWorkFull(work)}
		out.Total = 1
	}
	return out, nil
}

func (p *openalexProvider) searchArchiveFallback(ctx context.Context, req openAlexSearchRequest) (*SearchResult, error) {
	// An exact DOI resolves through the archive's DOI lookup, not a search.
	if doi := openalex.NormalizeDOI(req.Query); doi != "" {
		rec, err := p.archive.LookupDOI(ctx, req.Query)
		if err != nil {
			return nil, err
		}
		out := &SearchResult{Kind: KindOpenAlex, Total: 1, Results: []Record{*rec}}
		if req.DownloadableOnly {
			out.Results = filterDownloadable(out.Results)
			out.Total = len(out.Results)
		}
		out.Results = dedupe(out.Results)
		normalizeSearch(SearchRequest{Page: req.Page, Limit: req.Limit}, out)
		return out, nil
	}
	out, err := p.archive.Search(ctx, archiveSearchQuery{
		Kind: KindArticle, Query: req.Query, Title: req.Title, Author: req.Author,
		Year: req.Year, Languages: req.Languages, Sort: req.Sort, Page: req.Page,
	})
	if err != nil {
		return nil, err
	}
	out.Kind = KindOpenAlex
	if req.DownloadableOnly {
		out.Results = filterDownloadable(out.Results)
		out.PartialMatches = filterDownloadable(out.PartialMatches)
	}
	out.Results = dedupe(out.Results)
	normalizeSearch(SearchRequest{Page: req.Page, Limit: req.Limit}, out)
	return out, nil
}

func (p *openalexProvider) Details(ctx context.Context, id string) (*RecordDetails, error) {
	work, err := p.client.Lookup(ctx, id)
	if err != nil {
		return nil, err
	}
	return openAlexDetails(work), nil
}

func (p *openalexProvider) Read(ctx context.Context, req ReadRequest) (*Article, error) {
	result, err := p.client.Read(ctx, p.files, req.ID, req.Continuation, req.Limit)
	if err != nil {
		return nil, err
	}
	out := &Article{
		OpenAlexID: result.OpenAlexID, Title: result.Title, URL: result.URL,
		Source: result.Source, Text: result.Text, SHA256: result.Digest,
		Warnings: slices.Clone(result.Warnings),
	}
	out.Passages = make([]Passage, len(result.Passages))
	for i, passage := range result.Passages {
		out.Passages[i] = Passage(passage)
	}
	if result.Continuation != "" {
		out.Continuation = result.Continuation
	}
	return out, nil
}

func (p *openalexProvider) Download(ctx context.Context, req openAlexDownloadRequest) (*DownloadResult, error) {
	options, err := openalex.NormalizeDownload(openalex.DownloadOptions{
		OpenAlexID: req.OpenAlexID, Title: req.Title, Format: req.Format,
		Artifact: req.Artifact, LocationID: req.LocationID, Version: req.Version,
	})
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	work, lookupErr := p.client.Lookup(lookupCtx, openAlexIDOrDOI(options))
	cancel()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if lookupErr != nil {
		if doi, ok := openalex.ArchiveFallbackDOI(nil, options); ok {
			return p.downloadArchive(ctx, req, doi, "")
		}
		return nil, openalex.UnavailableDownload(lookupErr)
	}
	candidates, err := openalex.DownloadCandidates(work, options)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, candidate := range candidates {
		result, fileErr := p.client.DownloadArtifact(ctx, p.files, work, candidate, options.Title, options.Format, req.Progress)
		if fileErr == nil {
			return openAlexDownloadResult(result), nil
		}
		if terminalDownloadError(ctx, fileErr) {
			return nil, fileErr
		}
		lastErr = fileErr
	}
	if len(candidates) == 0 && lastErr == nil {
		lastErr = httpx.NewError(httpx.CodeNotFound, "no_content", "OpenAlex found the work but no compatible downloadable file is advertised")
	}
	if doi, ok := openalex.ArchiveFallbackDOI(work, options); ok {
		return p.downloadArchive(ctx, req, doi, work.DisplayName)
	}
	return nil, openalex.UnavailableDownload(lastErr)
}

func (p *openalexProvider) downloadArchive(ctx context.Context, req openAlexDownloadRequest, doi, expectedTitle string) (*DownloadResult, error) {
	return p.archive.DownloadDOI(ctx, archiveDownloadRequest{
		DOI: doi, Title: req.Title, ExpectedTitle: expectedTitle, Format: req.Format, Progress: req.Progress,
	})
}

func openAlexIDOrDOI(options openalex.DownloadOptions) string {
	if options.OpenAlexID != "" {
		return options.OpenAlexID
	}
	return options.DOI
}

// fallbackAllowed mirrors the previous coordinator: invalid arguments never
// fall back, and neither do canceled or expired contexts.
func fallbackAllowed(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	if providerErr, ok := errors.AsType[*httpx.ProviderError](err); ok && providerErr != nil {
		return providerErr.Code != httpx.CodeInvalidArgument
	}
	return true
}

// terminalDownloadError reports failures that must not fall back to another
// source: canceled contexts, configuration or local I/O problems.
func terminalDownloadError(ctx context.Context, err error) bool {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if pathError, ok := errors.AsType[*os.PathError](err); ok && pathError != nil {
		return true
	}
	if providerErr, ok := errors.AsType[*httpx.ProviderError](err); ok && providerErr != nil {
		return providerErr.Code == httpx.CodeConfig || providerErr.Reason == "local_io"
	}
	return false
}

func parseWorkTypes(workType, relation string) []string {
	workTypes := csv(workType)
	if len(workTypes) == 0 {
		workTypes = []string{"article", "review", "preprint", "conference-paper"}
	}
	if workType == "any" || (workType == "" && relation != "") {
		workTypes = []string{"any"}
	}
	return workTypes
}

func csv(raw string) []string {
	var result []string
	for value := range strings.SplitSeq(raw, ",") {
		if value = strings.TrimSpace(value); value != "" {
			result = append(result, value)
		}
	}
	return result
}

func moreOpenAlexPages(upstreamTotal int, nextCursor string, page, limit int) bool {
	if nextCursor != "" {
		return true
	}
	lastPage := 0
	if upstreamTotal > 0 {
		lastPage = 1 + (upstreamTotal-1)/limit
	}
	return page < lastPage && page < 10000/limit
}

func mapOpenAlexWork(work *openalex.Work) Record {
	return openAlexBaseRecord(work, etAlAuthors(work))
}

func mapOpenAlexWorkFull(work *openalex.Work) Record {
	return openAlexBaseRecord(work, fullAuthors(work))
}

func etAlAuthors(work *openalex.Work) string {
	authors := make([]string, 0, min(len(work.Authorships), 5))
	for _, authorship := range work.Authorships {
		if len(authors) == 5 {
			break
		}
		authors = append(authors, authorship.Author.DisplayName)
	}
	text := strings.Join(authors, ", ")
	if len(work.Authorships) > 5 {
		text += " et al."
	}
	return text
}

func fullAuthors(work *openalex.Work) string {
	authors := make([]string, 0, len(work.Authorships))
	for _, authorship := range work.Authorships {
		authors = append(authors, authorship.Author.DisplayName)
	}
	return strings.Join(authors, ", ")
}

func openAlexBaseRecord(work *openalex.Work, authors string) Record {
	var journal string
	if work.PrimaryLocation != nil && work.PrimaryLocation.Source != nil {
		journal = work.PrimaryLocation.Source.DisplayName
	}
	var year string
	if work.PublicationYear > 0 {
		year = strconv.Itoa(work.PublicationYear)
	}
	return Record{
		Kind: "article", ID: work.ShortID(), Title: work.DisplayName,
		Authors: authors, Publisher: journal, Year: year,
		DOI: openalex.NormalizeDOI(work.DOI), URL: openalex.PublicLandingURL(work),
	}
}

func openAlexDetails(work *openalex.Work) *RecordDetails {
	identifiers := map[string][]string{"openalex": {work.ShortID()}}
	if doi := openalex.NormalizeDOI(work.DOI); doi != "" {
		identifiers["doi"] = []string{doi}
	}
	out := &RecordDetails{
		ID: work.ShortID(), URL: openalex.PublicLandingURL(work), Title: work.DisplayName,
		Authors:   fullAuthors(work),
		Publisher: openAlexJournal(work), Year: openAlexYear(work),
		Description: shortenDescription(work.Abstract()),
		Identifiers: identifiers,
	}
	if work.Language != "" {
		out.Languages = []string{work.Language}
	}
	if work.Type != "" {
		out.Subjects = []string{work.Type}
	}
	return out
}

func openAlexJournal(work *openalex.Work) string {
	if work.PrimaryLocation != nil && work.PrimaryLocation.Source != nil {
		return work.PrimaryLocation.Source.DisplayName
	}
	return ""
}

func openAlexYear(work *openalex.Work) string {
	if work.PublicationYear > 0 {
		return strconv.Itoa(work.PublicationYear)
	}
	return ""
}

// openAlexDownloadResult maps a stored OpenAlex copy. OpenAlex downloads are
// content fingerprints, not Anna's Archive checksum matches, so verification
// is SHA-256 and no MD5 is reported.
func openAlexDownloadResult(result openalex.DownloadResult) *DownloadResult {
	out := &DownloadResult{
		Path: result.Path, Name: result.Name, Bytes: result.Bytes,
		Verification: VerificationNone, Reused: result.Reused,
	}
	if result.SHA256 != "" {
		out.SHA256 = result.SHA256
		out.Verification = VerificationSHA256
	}
	return out
}
