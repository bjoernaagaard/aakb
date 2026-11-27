package aakb

import (
	"cmp"
	"context"
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/bjoernaagaard/aakb/internal/httpx"
	"github.com/bjoernaagaard/aakb/internal/openalex"
	"github.com/bjoernaagaard/aakb/internal/store"
)

// openalexProvider implements papersProvider on internal/openalex with result
// mapping to root types. Archive fallback preserves the previous
// auto-provider behavior: a failed or empty OpenAlex search retries through
// archive journals, and an OpenAlex download without a compatible copy
// retries verified OpenAlex sibling versions, Unpaywall, the archive, and any
// configured DOI mirror.
type openalexProvider struct {
	client    *openalex.Client
	files     *store.Store
	archive   archiveProvider
	unpaywall doiDownloadProvider
	doiMirror doiDownloadProvider
}

func newOpenAlexProvider(client *openalex.Client, files *store.Store, archive archiveProvider, unpaywall, doiMirror doiDownloadProvider) *openalexProvider {
	return &openalexProvider{client: client, files: files, archive: archive, unpaywall: unpaywall, doiMirror: doiMirror}
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
// that the archive cannot serve, so failures must not fall back to it.
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
	if options.LocationID == "" && options.Version == "" && options.Artifact == "pdf" {
		siblings := p.findSiblingWorks(ctx, work)
		if res, sibErr := p.trySiblingDownloads(ctx, work, siblings, options, req.Progress); res != nil || sibErr != nil {
			return res, sibErr
		}
		dois := candidateDOIs(work, options, siblings)
		var fallbackErr error
		for _, doi := range dois {
			res, dlErr := p.downloadDOI(ctx, req, doi, work.DisplayName)
			if dlErr == nil {
				return res, nil
			}
			if terminalDownloadError(ctx, dlErr) {
				return nil, dlErr
			}
			if fallbackErr == nil {
				fallbackErr = dlErr
			}
		}
		if fallbackErr != nil {
			return nil, fallbackErr
		}
	}
	return nil, openalex.UnavailableDownload(lastErr)
}

func (p *openalexProvider) downloadArchive(ctx context.Context, req openAlexDownloadRequest, doi, expectedTitle string) (*DownloadResult, error) {
	return p.downloadDOI(ctx, req, doi, expectedTitle)
}

func (p *openalexProvider) downloadDOI(ctx context.Context, req openAlexDownloadRequest, doi, expectedTitle string) (*DownloadResult, error) {
	return downloadDOIChain(ctx, p.unpaywall, p.archive, p.doiMirror, archiveDownloadRequest{
		DOI: doi, Title: req.Title, ExpectedTitle: expectedTitle, Format: req.Format, Progress: req.Progress,
	})
}

func (p *openalexProvider) findSiblingWorks(ctx context.Context, work *openalex.Work) []openalex.Work {
	if work == nil || strings.TrimSpace(work.DisplayName) == "" || ctx.Err() != nil {
		return nil
	}
	searchCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	page, err := p.client.Search(searchCtx, openalex.SearchOptions{
		Title:     work.DisplayName,
		WorkTypes: []string{"any"},
		PerPage:   8,
	})
	if err != nil {
		return nil
	}
	var siblings []openalex.Work
	for i := range page.Results {
		cand := &page.Results[i]
		if cand.ShortID() == work.ShortID() {
			continue
		}
		if isVerifiedSibling(work, cand) {
			siblings = append(siblings, *cand)
		}
	}
	slices.SortStableFunc(siblings, compareSiblingPriority)
	if len(siblings) > 3 {
		siblings = siblings[:3]
	}
	return siblings
}

func (p *openalexProvider) trySiblingDownloads(ctx context.Context, primary *openalex.Work, siblings []openalex.Work, options openalex.DownloadOptions, progress func(done, total int64)) (*DownloadResult, error) {
	title := options.Title
	if title == "" {
		title = primary.DisplayName
	}
	for i := range siblings {
		sib := &siblings[i]
		sibOpts := openalex.DownloadOptions{
			OpenAlexID: sib.ShortID(),
			Title:      title,
			Format:     options.Format,
			Artifact:   "pdf",
		}
		candidates, err := openalex.DownloadCandidates(sib, sibOpts)
		if err != nil {
			continue
		}
		for _, candidate := range candidates {
			res, fileErr := p.client.DownloadArtifact(ctx, p.files, sib, candidate, title, options.Format, progress)
			if fileErr == nil {
				return openAlexDownloadResult(res), nil
			}
			if terminalDownloadError(ctx, fileErr) {
				return nil, fileErr
			}
		}
	}
	return nil, nil
}

func candidateDOIs(work *openalex.Work, options openalex.DownloadOptions, siblings []openalex.Work) []string {
	var published, working []string
	seen := make(map[string]bool)
	add := func(raw string) {
		doi := openalex.NormalizeDOI(raw)
		if doi == "" {
			return
		}
		key := strings.ToLower(doi)
		if seen[key] {
			return
		}
		seen[key] = true
		if isSSRNDOI(doi) {
			working = append(working, doi)
		} else {
			published = append(published, doi)
		}
	}
	if doi, ok := openalex.ArchiveFallbackDOI(work, options); ok {
		add(doi)
	}
	for i := range siblings {
		add(siblings[i].DOI)
	}
	out := slices.Concat(published, working)
	if len(out) > 3 {
		out = out[:3]
	}
	return out
}

func isVerifiedSibling(primary, cand *openalex.Work) bool {
	wantTitle := normalizePaperTitle(primary.DisplayName)
	if wantTitle == "" || wantTitle != normalizePaperTitle(cand.DisplayName) {
		return false
	}
	wantAuthors := workAuthorSurnames(primary)
	gotAuthors := workAuthorSurnames(cand)
	if len(wantAuthors) > 0 && len(gotAuthors) > 0 {
		shared := 0
		for name := range wantAuthors {
			if gotAuthors[name] {
				shared++
			}
		}
		smaller := min(len(wantAuthors), len(gotAuthors))
		return shared >= 1 && shared*2 >= smaller
	}
	if primary.PublicationYear > 0 && cand.PublicationYear > 0 {
		diff := primary.PublicationYear - cand.PublicationYear
		if diff < 0 {
			diff = -diff
		}
		return diff <= 5
	}
	return false
}

func compareSiblingPriority(a, b openalex.Work) int {
	score := func(w *openalex.Work) int {
		s := 0
		if openalex.HasDownload(w) {
			s += 100
		}
		doi := openalex.NormalizeDOI(w.DOI)
		if doi != "" && !isSSRNDOI(doi) {
			s += 20
		}
		return s
	}
	if c := cmp.Compare(score(&b), score(&a)); c != 0 {
		return c
	}
	return cmp.Compare(b.CitedByCount, a.CitedByCount)
}

func isSSRNDOI(doi string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(doi)), "10.2139/ssrn.")
}

func workAuthorSurnames(work *openalex.Work) map[string]bool {
	out := make(map[string]bool)
	for _, a := range work.Authorships {
		name := normalizePaperTitle(a.Author.DisplayName)
		fields := strings.Fields(name)
		if len(fields) == 0 {
			continue
		}
		last := fields[len(fields)-1]
		if len(last) >= 2 {
			out[last] = true
		}
	}
	return out
}

func normalizePaperTitle(title string) string {
	var out strings.Builder
	lastSpace := true
	for _, r := range strings.ToLower(title) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			out.WriteRune(r)
			lastSpace = false
			continue
		}
		if !lastSpace {
			out.WriteByte(' ')
			lastSpace = true
		}
	}
	return strings.TrimSpace(out.String())
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
		workTypes = []string{"article", "review", "preprint", "conference-paper", "book", "book-chapter", "editorial", "report"}
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
		FastDownload: openalex.HasDownload(work),
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
// content fingerprints, not archive checksum matches, so verification
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
