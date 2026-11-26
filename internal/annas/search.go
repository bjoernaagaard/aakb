package annas

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	xhtml "golang.org/x/net/html"

	"github.com/bjoernaagaard/aakb/internal/httpx"
)

// maxHTMLBytes bounds one archive HTML page read for search, account, SciDB,
// and record pages.
const maxHTMLBytes = 8 << 20

// SearchOptions narrows one page of archive results. Index is an archive
// search tab such as "journals" or "meta". The list filters take
// upstream values; a leading "-" excludes a value instead of requiring it.
// book_any and journal are legacy Content values and are not sent.
type SearchOptions struct {
	Index        string
	Page         int
	Sort         string
	Content      []string
	Language     []string
	Format       []string
	Source       []string
	Access       []string
	Fields       []SearchField
	Descriptions bool
}

// SearchField limits one metadata field to a phrase. Name is an archive field
// such as title, author, publisher or year.
type SearchField struct {
	Name  string
	Value string
}

// SearchPage is one page of results. Results honor every filter. Partial
// holds the looser matches the archive adds to a short first page; they
// may ignore the filters. Total counts all matches, and is only a lower
// bound when TotalIsLowerBound is set.
type SearchPage[T any] struct {
	Results           []T
	Partial           []T
	Total             int
	TotalIsLowerBound bool
	MorePages         bool
}

// Book is one file search hit. FastDownload reports whether the archive's fast
// download servers have the file, which downloads need; PossibleIssues
// mirrors the archive's warning that the file may be broken or mislabeled.
type Book struct {
	Title          string
	Authors        string
	Publisher      string
	Edition        string
	Language       string
	Format         string
	Size           string
	Year           string
	Content        string
	Sources        []string
	FastDownload   bool
	PossibleIssues bool
	Hash           string
	URL            string
	DOI            string
	Description    string
	Stats          *RecordStats
}

// Paper is a journal article found by keyword search or DOI lookup.
// DownloadURL is a signed link issued to the member session, so it is used
// internally and never output. Journal is the record's publisher and edition
// line, which for articles usually names the journal, volume and issue.
type Paper struct {
	DOI            string
	Title          string
	Authors        string
	Journal        string
	Year           string
	Format         string
	Size           string
	Sources        []string
	FastDownload   bool
	PossibleIssues bool
	Hash           string
	Description    string
	DownloadURL    string
	PageURL        string
	Stats          *RecordStats
}

// MetadataRecord is a catalog entry from a source such as Open Library,
// WorldCat or ISBNdb. It describes a book but has no file of its own.
type MetadataRecord struct {
	ID          string
	Title       string
	Authors     string
	Publisher   string
	Edition     string
	Language    string
	Year        string
	Description string
	URL         string
}

var (
	languageCodePattern = regexp.MustCompile(`^[a-z]{2,3}(-[a-z0-9]{2,8})?$`)
	searchFormatPattern = regexp.MustCompile(`^[a-z0-9]{1,10}$`)
	sourcePattern       = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)
	yearPattern         = regexp.MustCompile(`^\d{1,4}$`)
)

var contentTypes = map[string]bool{
	"book_fiction": true, "book_nonfiction": true, "book_unknown": true, "book_comic": true,
	"magazine": true, "standards_document": true, "musical_score": true, "audiobook": true,
	"other": true, "journal_article": true,
	// Legacy values that ResolveSearch maps or drops.
	"book_any": true, "journal": true,
}

// The archive's sort values; relevance is its default, sent as no sort at all.
var sortValues = map[string]string{
	"": "", "relevance": "", "newest": "newest", "oldest": "oldest",
	"largest": "largest", "smallest": "smallest",
	"newest_added": "newest_added", "oldest_added": "oldest_added", "random": "random",
}

// ResolveSearch applies legacy content values and the default for a search.
// book_any is not a current archive filter and makes later pages empty.
// journal is the old article default and maps to the journals index, as does
// journal_article outside the metadata index: that tab holds only articles.
func ResolveSearch(options SearchOptions, defaultContent string) SearchOptions {
	options.Index = strings.ToLower(strings.TrimSpace(options.Index))
	if options.Page < 1 {
		options.Page = 1
	}
	content := make([]string, 0, len(options.Content))
	for _, value := range options.Content {
		switch value = strings.ToLower(strings.TrimSpace(value)); value {
		case "", "book_any":
		case "journal", "journal_article":
			if options.Index == "" {
				options.Index = "journals"
			}
			if value == "journal_article" && options.Index != "journals" {
				content = append(content, value)
			}
		default:
			content = append(content, value)
		}
	}
	options.Content = content
	if len(options.Content) == 0 && options.Index == "" {
		switch defaultContent {
		case "journal", "journals":
			options.Index = "journals"
		case "", "book_any":
		default:
			options.Content = []string{defaultContent}
		}
	}
	return options
}

// NormalizeSearch validates archive search semantics and returns independent slices.
// A zero page becomes page one. defaultContent may select the journals index.
func NormalizeSearch(query string, options SearchOptions, defaultContent string) (SearchOptions, error) {
	if options.Page < 0 {
		return options, httpx.NewError(httpx.CodeInvalidArgument, "invalid_page", "page must be 1 or greater")
	}
	fields := make([]SearchField, 0, len(options.Fields))
	for _, field := range options.Fields {
		field.Name = strings.ToLower(strings.TrimSpace(field.Name))
		switch field.Name {
		case "title", "author", "publisher", "year":
		default:
			return options, httpx.NewError(httpx.CodeInvalidArgument, "unsupported_field", "unsupported search field "+field.Name)
		}
		field.Value = strings.Join(strings.Fields(field.Value), " ")
		if field.Value == "" {
			continue
		}
		if len(field.Value) > 300 {
			return options, httpx.NewError(httpx.CodeInvalidArgument, "field_too_long", field.Name+" must be at most 300 characters")
		}
		if field.Name == "year" && !yearPattern.MatchString(field.Value) {
			return options, httpx.NewError(httpx.CodeInvalidArgument, "invalid_year", "year must be a publication year such as 2008")
		}
		fields = append(fields, field)
	}
	if len(fields) > 9 {
		return options, httpx.NewError(httpx.CodeInvalidArgument, "too_many_fields", "search accepts at most nine fields")
	}
	options.Fields = fields
	if strings.TrimSpace(query) == "" && len(fields) == 0 {
		return options, httpx.NewError(httpx.CodeInvalidArgument, "empty_query", "search query is empty; pass a query or at least one of title, author, publisher or year")
	}
	var err error
	if options.Language, err = normalizeFilter(options.Language, "language", languageCodePattern.MatchString); err != nil {
		return options, httpx.NewError(httpx.CodeInvalidArgument, "invalid_language", "language must be a language code such as en, or a comma-separated list such as en,de")
	}
	for i, v := range options.Language {
		options.Language[i] = canonicalLanguage(v)
	}
	if options.Source, err = normalizeFilter(options.Source, "source", sourcePattern.MatchString); err != nil {
		return options, err
	}
	if options.Content, err = normalizeFilter(options.Content, "content", func(v string) bool { return contentTypes[v] }); err != nil {
		return options, err
	}
	if options.Format, err = normalizeFilter(options.Format, "format", searchFormatPattern.MatchString); err != nil {
		return options, err
	}
	if options.Access, err = normalizeFilter(options.Access, "access", func(v string) bool { return v == "aa_download" || v == "aa_scidb" }); err != nil {
		return options, err
	}
	sortValue, ok := sortValues[strings.ToLower(strings.TrimSpace(options.Sort))]
	if !ok {
		return options, httpx.NewError(httpx.CodeInvalidArgument, "unsupported_sort", fmt.Sprintf("unsupported sort %q; use relevance, newest, oldest, largest, smallest, newest_added, oldest_added or random", options.Sort))
	}
	options.Sort = sortValue
	options = ResolveSearch(options, defaultContent)
	switch options.Index {
	case "", "journals", "meta":
	default:
		return options, httpx.NewError(httpx.CodeInvalidArgument, "unsupported_index", "unsupported search index "+options.Index)
	}
	if options.Index == "meta" && (options.Sort == "largest" || options.Sort == "smallest") {
		return options, httpx.NewError(httpx.CodeInvalidArgument, "unsupported_sort", "catalog records have no file size; sort by relevance, newest, oldest, newest_added, oldest_added or random")
	}
	return options, nil
}

func normalizeFilter(raw []string, name string, valid func(string) bool) ([]string, error) {
	var values []string
	for _, part := range raw {
		part = strings.ToLower(strings.TrimSpace(part))
		excluded := strings.HasPrefix(part, "-")
		value := strings.TrimSpace(strings.TrimPrefix(part, "-"))
		if value == "" {
			if excluded {
				return nil, httpx.NewError(httpx.CodeInvalidArgument, "invalid_"+name, name+" has a - without a value")
			}
			continue
		}
		if !valid(value) {
			return nil, httpx.NewError(httpx.CodeInvalidArgument, "unsupported_"+name, fmt.Sprintf("unsupported %s %q", name, value))
		}
		if excluded {
			value = "-" + value
		}
		values = append(values, value)
	}
	return values, nil
}

// canonicalLanguage restores tag casing that the archive's codes use, such as zh-Hant.
func canonicalLanguage(code string) string {
	prefix := ""
	if strings.HasPrefix(code, "-") {
		prefix, code = "-", code[1:]
	}
	parts := strings.Split(code, "-")
	for i := 1; i < len(parts); i++ {
		switch len(parts[i]) {
		case 4:
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		case 2:
			parts[i] = strings.ToUpper(parts[i])
		}
	}
	return prefix + strings.Join(parts, "-")
}

func buildSearchURL(base, query string, options SearchOptions) string {
	if options.Page < 1 {
		options.Page = 1
	}
	values := url.Values{}
	values.Set("q", query)
	if options.Index != "" {
		values.Set("index", options.Index)
	}
	addSearchFilter(values, "content", options.Content)
	addSearchFilter(values, "lang", options.Language)
	addSearchFilter(values, "ext", options.Format)
	addSearchFilter(values, "src", options.Source)
	addSearchFilter(values, "acc", options.Access)
	if options.Sort != "" {
		values.Set("sort", options.Sort)
	}
	if options.Descriptions {
		values.Set("desc", "1")
	}
	// The archive reads at most nine numbered field terms.
	for i, field := range options.Fields {
		values.Set("termtype_"+strconv.Itoa(i+1), field.Name)
		values.Set("termval_"+strconv.Itoa(i+1), field.Value)
	}
	if options.Page > 1 {
		values.Set("page", strconv.Itoa(options.Page))
	}
	return buildPathURL(base, "/search") + "?" + values.Encode()
}

// addSearchFilter sends each value as a repeated parameter. The archive excludes a
// value written as anti__value; callers mark exclusions with a leading "-".
func addSearchFilter(values url.Values, key string, filters []string) {
	for _, filter := range filters {
		if excluded, ok := strings.CutPrefix(filter, "-"); ok {
			filter = "anti__" + excluded
		}
		values.Add(key, filter)
	}
}

// SearchBooks returns one page of book search results.
func (c *Client) SearchBooks(ctx context.Context, query string, options SearchOptions) (SearchPage[*Book], error) {
	results, err := c.search(ctx, query, options, "")
	if err != nil {
		return SearchPage[*Book]{}, err
	}
	return filePage(results, func(book *Book) *Book { return book }), nil
}

// SearchArticles returns one page of journal article search results.
func (c *Client) SearchArticles(ctx context.Context, query string, options SearchOptions) (SearchPage[*Paper], error) {
	results, err := c.search(ctx, query, options, "journals")
	if err != nil {
		return SearchPage[*Paper]{}, err
	}
	return filePage(results, func(book *Book) *Paper { return paperFromBook(book, book.DOI) }), nil
}

// SearchMetadata searches catalog records, which describe books without
// holding a file, such as Open Library, WorldCat and ISBNdb entries.
func (c *Client) SearchMetadata(ctx context.Context, query string, options SearchOptions) (SearchPage[*MetadataRecord], error) {
	options.Index = "meta"
	results, err := c.search(ctx, query, options, "")
	if err != nil {
		return SearchPage[*MetadataRecord]{}, err
	}
	return metadataPage(results), nil
}

// searchResults is one parsed results page.
type searchResults struct {
	cards   []Card
	root    *xhtml.Node
	pageURL string
	page    int
}

func (c *Client) search(ctx context.Context, query string, options SearchOptions, defaultContent string) (searchResults, error) {
	query = strings.TrimSpace(query)
	var err error
	options, err = NormalizeSearch(query, options, defaultContent)
	if err != nil {
		return searchResults{}, err
	}
	if err := c.requireArchiveAccess(ctx); err != nil {
		return searchResults{}, err
	}
	var results searchResults
	err = c.onMirror(ctx, func(base string) error {
		var err error
		results, err = c.searchOn(ctx, base, query, options)
		return err
	})
	return results, c.explainSearchRefusal(ctx, err)
}

func (c *Client) searchOn(ctx context.Context, base, query string, options SearchOptions) (searchResults, error) {
	pageURL := buildSearchURL(base, query, options)
	root, err := c.fetchHTML(ctx, pageURL)
	if err != nil {
		return searchResults{pageURL: pageURL}, err
	}
	cards := SearchCards(root)
	c.logUnread("search", max(options.Page, 1), CheckSearchPage(root, cards))
	if len(cards) == 0 && !IsArchivePage(root) {
		c.mirrorFailed(ctx, pageURL, MirrorFailure{Reason: "answered a search with a page that is not the archive", Blocked: true})
		return searchResults{pageURL: pageURL}, httpx.NewError(httpx.CodeUpstreamBlocked, "not_archive_page", "archive returned an unexpected page; check the mirror and account access")
	}
	if len(cards) == 0 && ShowsResults(root, options.Page) {
		return searchResults{pageURL: pageURL}, httpx.NewError(httpx.CodeUpstream, "layout_drift", "The archive returned search results this version cannot read; its page layout may have changed")
	}
	if err := checkFacetFilter(root, "src", "source", options.Source); err != nil {
		return searchResults{pageURL: pageURL}, err
	}
	if err := checkFacetFilter(root, "content", "content", options.Content); err != nil {
		return searchResults{pageURL: pageURL}, err
	}
	return searchResults{cards: cards, root: root, pageURL: pageURL, page: options.Page}, nil
}

// fetchHTML reads one archive HTML page: status handling mirrors the JSON
// endpoints, then the body is bounded and parsed with x/net/html.
func (c *Client) fetchHTML(ctx context.Context, rawURL string) (*xhtml.Node, error) {
	resp, err := c.doGetWith(ctx, rawURL, nil)
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusForbidden:
		httpx.DrainAndClose(resp.Body)
		return nil, &forbiddenError{message: "The archive refused the page; the page is available only to signed-in members, so check that your membership is active"}
	case http.StatusNotFound:
		httpx.DrainAndClose(resp.Body)
		return nil, httpx.NewError(httpx.CodeNotFound, "not_found", "archive record was not found")
	default:
		if resp.StatusCode >= http.StatusMultipleChoices && resp.StatusCode < http.StatusBadRequest {
			httpx.DrainAndClose(resp.Body)
			return nil, httpx.NewError(httpx.CodeNotFound, "redirected_away", "archive redirected away from the requested page")
		}
		httpx.DrainAndClose(resp.Body)
		return nil, httpResponseError(resp, rawURL)
	}
	body, readErr := httpx.ReadBounded(resp.Body, maxHTMLBytes+1)
	closeErr := resp.Body.Close()
	if readErr != nil {
		return nil, c.requestFailure(readErr, rawURL)
	}
	if closeErr != nil {
		return nil, c.requestFailure(closeErr, rawURL)
	}
	if int64(len(body)) > maxHTMLBytes {
		return nil, httpx.NewError(httpx.CodeUpstream, "html_too_large", "archive response exceeded the HTML size limit")
	}
	root, err := parseHTML(body)
	if err != nil {
		return nil, httpx.WrapError(httpx.CodeUpstream, "invalid_html", "archive returned a page this version cannot read", err)
	}
	return root, nil
}

// filePage builds a page of file results, converting each card with convert.
func filePage[T any](results searchResults, convert func(*Book) T) SearchPage[T] {
	page := splitPage(results.cards,
		func(card Card) bool { return card.Prefix == "md5" },
		func(card Card) T { return convert(bookFromCard(card, results.pageURL)) })
	pageTotals(&page, results.root, results.page)
	return page
}

// metadataPage builds a page of catalog records.
func metadataPage(results searchResults) SearchPage[*MetadataRecord] {
	page := splitPage(results.cards,
		func(card Card) bool { return card.Prefix != "md5" },
		func(card Card) *MetadataRecord { return metadataFromCard(card, results.pageURL) })
	pageTotals(&page, results.root, results.page)
	return page
}

func bookFromCard(card Card, pageURL string) *Book {
	return &Book{
		Title:          card.Title,
		Authors:        card.Authors,
		Publisher:      card.Publisher,
		Edition:        card.Edition,
		Language:       card.Meta.Language,
		Format:         card.Meta.Format,
		Size:           card.Meta.Size,
		Year:           card.Meta.Year,
		Content:        card.Meta.Content,
		Sources:        card.Meta.Sources,
		FastDownload:   card.Meta.FastDownload,
		PossibleIssues: card.PossibleIssues,
		Description:    card.Description,
		URL:            absoluteURL(pageURL, card.Href),
		Hash:           card.ID,
	}
}

func metadataFromCard(card Card, pageURL string) *MetadataRecord {
	id, err := url.PathUnescape(card.ID)
	if err != nil {
		id = card.ID
	}
	return &MetadataRecord{
		ID:          card.Prefix + ":" + id,
		Title:       card.Title,
		Authors:     card.Authors,
		Publisher:   card.Publisher,
		Edition:     card.Edition,
		Language:    card.Meta.Language,
		Year:        card.Meta.Year,
		Description: card.Description,
		URL:         absoluteURL(pageURL, card.Href),
	}
}

// splitPage sorts the chosen cards into exact and partial matches.
func splitPage[T any](cards []Card, keep func(Card) bool, convert func(Card) T) SearchPage[T] {
	page := SearchPage[T]{Results: []T{}}
	for _, card := range cards {
		if !keep(card) {
			continue
		}
		if card.Partial {
			page.Partial = append(page.Partial, convert(card))
		} else {
			page.Results = append(page.Results, convert(card))
		}
	}
	return page
}

func paperFromBook(book *Book, doi string) *Paper {
	if book == nil {
		return nil
	}
	if doi == "" {
		doi = book.DOI
	}
	return &Paper{
		DOI:            doi,
		Title:          book.Title,
		Authors:        book.Authors,
		Journal:        joinNonEmpty(", ", book.Publisher, book.Edition),
		Year:           book.Year,
		Format:         book.Format,
		Size:           book.Size,
		Sources:        book.Sources,
		FastDownload:   book.FastDownload,
		PossibleIssues: book.PossibleIssues,
		Hash:           book.Hash,
		Description:    book.Description,
		PageURL:        book.URL,
		Stats:          book.Stats,
	}
}

func joinNonEmpty(separator string, values ...string) string {
	var kept []string
	for _, value := range values {
		if value != "" {
			kept = append(kept, value)
		}
	}
	return strings.Join(kept, separator)
}

// pageTotals fills in totals for a parsed page. Without the archive's count, a
// full page is taken to mean that more results may follow. A page past the
// last result shows no count either; the total is then unknown, so it is
// reported as a lower bound of zero, with no more pages.
func pageTotals[T any](page *SearchPage[T], root *xhtml.Node, pageNumber int) {
	if pageNumber < 1 {
		pageNumber = 1
	}
	total, lowerBound, found := ResultsTotal(root)
	if !found && pageNumber > 1 && len(page.Results) == 0 && len(page.Partial) == 0 {
		page.Total, page.TotalIsLowerBound, page.MorePages = 0, true, false
		return
	}
	if !found {
		total = (pageNumber-1)*ResultsPerPage + len(page.Results)
		lowerBound = len(page.Results) >= ResultsPerPage
	}
	page.Total, page.TotalIsLowerBound = total, lowerBound
	page.MorePages = pageNumber < MaxResultPages && (total > pageNumber*ResultsPerPage || lowerBound)
}

// checkFacetFilter rejects requested values the page does not offer. The archive
// ignores an unknown value and returns no exact matches, which would read
// as the work being absent. Pages without the facet are not checked.
func checkFacetFilter(root *xhtml.Node, name, label string, requested []string) error {
	if len(requested) == 0 || root == nil {
		return nil
	}
	offered := FacetValues(root, name)
	if len(offered) == 0 {
		return nil
	}
	known := make(map[string]bool, len(offered))
	for _, value := range offered {
		known[value] = true
	}
	for _, value := range requested {
		value = strings.TrimPrefix(value, "-")
		if !known[value] {
			sort.Strings(offered)
			return httpx.NewError(httpx.CodeInvalidArgument, "unknown_"+label, fmt.Sprintf("unknown %s %q for this search; use one of: %s", label, value, strings.Join(offered, ", ")))
		}
	}
	return nil
}

// absoluteURL resolves href against the search page URL, falling back to
// the raw href when either side does not parse.
func absoluteURL(pageURL, href string) string {
	page, err := url.Parse(pageURL)
	if err != nil {
		return href
	}
	ref, err := url.Parse(href)
	if err != nil {
		return href
	}
	return page.ResolveReference(ref).String()
}

// logUnread records what the search parser could not read. Reports are
// bounded short text the parser already extracted, never markup.
func (c *Client) logUnread(kind string, page int, report Report) {
	if report.Empty() {
		return
	}
	c.log.Warn("archive page had unreadable parts",
		"kind", kind, "page", page, "report", report.String())
}
