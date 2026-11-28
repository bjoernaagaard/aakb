package archive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/bjoernaagaard/aakb/internal/doi"
	"github.com/bjoernaagaard/aakb/internal/httpx"
)

// doiTimeout bounds DOI.org metadata reads: the archive can find the paper
// without it, so a DOI.org that hangs must not use up the whole lookup.
const doiTimeout = 10 * time.Second

// maxDOICandidates bounds how many record pages one DOI lookup reads.
const maxDOICandidates = 12

// doiRecordChecks is how many record identities are verified at once.
const doiRecordChecks = 4

// NormalizeDOI removes common DOI URL/label prefixes and enclosing wrappers.
func NormalizeDOI(value string) string {
	return doi.Normalize(value)
}

// IsDOIQuery reports whether a query is explicitly trying to address a DOI.
func IsDOIQuery(query string) bool {
	return doi.IsQuery(query)
}

// ParseDOI normalizes and validates a DOI query.
func ParseDOI(query string) (string, bool) {
	return doi.Parse(query)
}

// ValidDOI reports whether value has a DOI's shape: 10.<registrant>/<suffix>.
func ValidDOI(value string) bool {
	return doi.Valid(value)
}

// ArxivID is the identifier in an arXiv DOI (10.48550/arXiv.<id>), in
// lower case and without its version, or "" for any other DOI.
func ArxivID(doi string) string {
	const prefix = "10.48550/arxiv."
	doi = NormalizeDOI(doi)
	if !strings.HasPrefix(strings.ToLower(doi), prefix) {
		return ""
	}
	return strings.ToLower(stripArxivVersion(doi[len(prefix):]))
}

func stripArxivVersion(identifier string) string {
	if i := strings.LastIndex(strings.ToLower(identifier), "v"); i > 0 && i+1 < len(identifier) {
		version := identifier[i+1:]
		if strings.Trim(version, "0123456789") == "" {
			return identifier[:i]
		}
	}
	return identifier
}

// NormalizeArxivID reads an arXiv identifier written as an arXiv DOI,
// "arxiv:<id>" or the bare id, in lower case and without its version.
func NormalizeArxivID(value string) string {
	value = strings.TrimSpace(value)
	if extracted := ArxivID(value); extracted != "" {
		return extracted
	}
	lower := strings.ToLower(value)
	for _, prefix := range []string{"arxiv:", "arxiv."} {
		if strings.HasPrefix(lower, prefix) {
			return strings.ToLower(stripArxivVersion(value[len(prefix):]))
		}
	}
	return strings.ToLower(stripArxivVersion(value))
}

// Citation is DOI.org's description of a work, from its CSL JSON.
type Citation struct {
	DOI     string   `json:"doi"`
	Title   string   `json:"title"`
	Authors []string `json:"authors"`
	Year    string   `json:"year"`
}

// ParseCitation reads DOI.org's CSL JSON. A work without a title is an
// error: there is nothing to check an archive record against.
func ParseCitation(body []byte) (Citation, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return Citation{}, err
	}
	metadata := Citation{
		DOI:     cslString(payload["DOI"]),
		Title:   cslStringOrArray(payload["title"]),
		Authors: cslAuthors(payload["author"]),
		Year:    cslYear(payload),
	}
	if metadata.DOI == "" {
		metadata.DOI = cslString(payload["doi"])
	}
	if metadata.Title == "" {
		return metadata, fmt.Errorf("doi.org metadata had no title")
	}
	return metadata, nil
}

func cslString(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return strings.TrimSpace(value)
	}
	return ""
}

func cslStringOrArray(raw json.RawMessage) string {
	if value := cslString(raw); value != "" {
		return value
	}
	var values []string
	if json.Unmarshal(raw, &values) == nil && len(values) > 0 {
		return strings.TrimSpace(values[0])
	}
	return ""
}

func cslAuthors(raw json.RawMessage) []string {
	var authors []struct {
		Given   string `json:"given"`
		Family  string `json:"family"`
		Literal string `json:"literal"`
	}
	if json.Unmarshal(raw, &authors) != nil {
		return nil
	}
	names := make([]string, 0, len(authors))
	for _, author := range authors {
		name := strings.TrimSpace(author.Literal)
		if name == "" {
			name = strings.TrimSpace(strings.TrimSpace(author.Given) + " " + strings.TrimSpace(author.Family))
		}
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

func cslYear(payload map[string]json.RawMessage) string {
	for _, key := range []string{"issued", "published-print", "published-online", "published"} {
		var date struct {
			DateParts [][]int `json:"date-parts"`
		}
		if json.Unmarshal(payload[key], &date) == nil && len(date.DateParts) > 0 && len(date.DateParts[0]) > 0 && date.DateParts[0][0] > 0 {
			return fmt.Sprint(date.DateParts[0][0])
		}
	}
	return ""
}

// Identity is a record's own description of its work: the title and authors
// the archive shows, and the DOI, arXiv id and year in its identifiers. DOIConflict
// marks a record listing several DOIs.
type Identity struct {
	DOI         string   `json:"doi"`
	DOIConflict bool     `json:"doi_conflict"`
	ArxivID     string   `json:"arxiv_id"`
	Title       string   `json:"title"`
	Authors     []string `json:"authors"`
	Year        string   `json:"year"`
}

// identityFromJSON reads a record's own identity from its lightweight JSON
// metadata endpoint: identifiers_unified.doi plus the normalized best title
// and author. The full record HTML page is never fetched for verification.
func identityFromJSON(record *recordPayload) Identity {
	var identity Identity
	file := &record.FileUnifiedData
	identity.Title = CleanText(string(file.TitleBest))
	identity.Authors = splitAuthors([]string{string(file.AuthorBest)})
	identity.Year = yearValue(string(file.YearBest))
	dois := make(map[string]struct{})
	for _, candidate := range file.IdentifiersUnified["doi"] {
		normalized := NormalizeDOI(strings.TrimSpace(candidate))
		if normalized == "" || strings.HasPrefix(normalized, "10.0000/") {
			continue
		}
		dois[strings.ToLower(normalized)] = struct{}{}
		if identity.DOI == "" {
			identity.DOI = normalized
		}
	}
	identity.DOIConflict = len(dois) > 1
	if identity.DOI != "" {
		identity.DOI = NormalizeDOI(identity.DOI)
	}
	for _, candidate := range file.IdentifiersUnified["arxiv"] {
		if identity.ArxivID == "" {
			identity.ArxivID = NormalizeArxivID(candidate)
		}
	}
	return identity
}

// yearValue reads a publication year from an identifier value.
func yearValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 4 && value[0] >= '1' && value[0] <= '2' {
		allDigits := true
		for _, digit := range value[:4] {
			if digit < '0' || digit > '9' {
				allDigits = false
				break
			}
		}
		if allDigits {
			return value[:4]
		}
	}
	return ""
}

var authorRoleSuffix = regexp.MustCompile(`\s*\((?:author|editor|translator|ed\.|eds\.)\)\s*$`)

// splitAuthors splits the archive's author fields into given-first names.
func splitAuthors(values []string) []string {
	var authors []string
	for _, value := range values {
		// The archive displays its current full author field as a comma-separated list
		// in one data-content node. Older templates expose one linked author per
		// node. Records from some libraries list "Family, Given (role)" names
		// separated by semicolons, where the comma belongs to the name.
		value = CleanAuthors(value)
		separators := func(r rune) bool { return r == ',' || r == ';' || r == '\n' || r == '\r' }
		if strings.Contains(value, ";") {
			separators = func(r rune) bool { return r == ';' || r == '\n' || r == '\r' }
		}
		for _, part := range strings.FieldsFunc(value, separators) {
			if name := givenFirst(part); name != "" {
				authors = append(authors, name)
			}
		}
	}
	return authors
}

// givenFirst turns "Family, Given (author)" into "Given Family".
func givenFirst(name string) string {
	name = strings.TrimSpace(authorRoleSuffix.ReplaceAllString(name, ""))
	if family, given, ok := strings.Cut(name, ","); ok && !strings.Contains(given, ",") {
		name = strings.TrimSpace(given) + " " + strings.TrimSpace(family)
	}
	return strings.TrimSpace(name)
}

// SameWork reports whether an archive record is the work a DOI names, as
// DOI.org describes it. A record's own DOI or arXiv id decides when it has
// one, as long as its title, authors and year don't contradict DOI.org's;
// otherwise the title and every author must agree.
func SameWork(doi string, metadata Citation, record Identity) bool {
	if record.DOIConflict {
		return false
	}
	if record.DOI != "" && !strings.EqualFold(NormalizeDOI(record.DOI), doi) {
		return false
	}
	wantArxiv := ArxivID(doi)
	if record.ArxivID != "" && wantArxiv != "" && NormalizeArxivID(record.ArxivID) != wantArxiv {
		return false
	}

	ownIdentifierMatches := record.DOI != "" && strings.EqualFold(NormalizeDOI(record.DOI), doi) ||
		wantArxiv != "" && record.ArxivID == wantArxiv
	if ownIdentifierMatches {
		if metadata.Title != "" && record.Title != "" && NormalizeTitle(metadata.Title) != NormalizeTitle(record.Title) {
			return false
		}
		if len(metadata.Authors) > 0 && len(record.Authors) > 0 && !sameAuthorSet(metadata.Authors, record.Authors) {
			return false
		}
		if metadata.Year != "" && record.Year != "" && !sameYear(doi, metadata.Year, record.Year) {
			return false
		}
		return true
	}
	// Some genuine archive records do not expose a DOI code. In that case require
	// the CSL title and every author to agree; a title by itself is never enough.
	return metadata.Title != "" && len(metadata.Authors) > 0 &&
		NormalizeTitle(metadata.Title) != "" && NormalizeTitle(metadata.Title) == NormalizeTitle(record.Title) &&
		len(record.Authors) > 0 && sameAuthorSet(metadata.Authors, record.Authors) &&
		(metadata.Year == "" || record.Year == "" || sameYear(doi, metadata.Year, record.Year))
}

// sameYear compares publication years. An arXiv DOI is dated by the
// preprint, and the journal or conference version the archive holds is often
// published the following year.
func sameYear(doi, want, got string) bool {
	if want == got {
		return true
	}
	wantYear, err1 := strconv.Atoi(want)
	gotYear, err2 := strconv.Atoi(got)
	return err1 == nil && err2 == nil && ArxivID(doi) != "" && gotYear == wantYear+1
}

// NormalizeTitle lowercases a title and reduces it to letters and digits
// separated by single spaces, for comparing titles.
func NormalizeTitle(title string) string {
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

// sameAuthorSet compares authors by family name and first initial, so
// "Diederik P. Kingma" and "Jimmy Lei Ba" match "Diederik Kingma" and
// "Jimmy Ba". The lists must have the same length.
func sameAuthorSet(expected, actual []string) bool {
	if len(expected) == 0 || len(expected) != len(actual) {
		return false
	}
	want := make(map[string]int, len(expected))
	for _, name := range expected {
		key := personKey(name)
		if key == "" {
			return false
		}
		want[key]++
	}
	for _, name := range actual {
		key := personKey(name)
		if key == "" || want[key] == 0 {
			return false
		}
		want[key]--
	}
	for _, count := range want {
		if count != 0 {
			return false
		}
	}
	return true
}

// personKey is a name's family name and first initial, from a name written
// given name first. Generational suffixes are ignored.
func personKey(name string) string {
	words := strings.Fields(normalizePersonName(name))
	for len(words) > 1 {
		switch words[len(words)-1] {
		case "jr", "sr", "ii", "iii", "iv":
			words = words[:len(words)-1]
			continue
		}
		break
	}
	if len(words) == 0 {
		return ""
	}
	family := words[len(words)-1]
	if len(words) == 1 {
		return family
	}
	initial, _ := utf8.DecodeRuneInString(words[0])
	return family + " " + string(initial)
}

// normalizePersonName lowercases a name, folds the accents of Latin letters
// and keeps letters and digits, separated by single spaces.
func normalizePersonName(name string) string {
	var normalized strings.Builder
	space := true
	for _, r := range foldLatinAccents(strings.ToLower(name)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			normalized.WriteRune(r)
			space = false
		} else if !space {
			normalized.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSpace(normalized.String())
}

// latinLetters are the Latin letters that carry their difference in the
// letter itself rather than in a combining accent.
var latinLetters = map[rune]rune{'ł': 'l', 'ø': 'o', 'đ': 'd', 'ð': 'd', 'ı': 'i'}

// foldLatinAccents drops the accents of Latin letters, so the archive's
// ASCII transliteration and DOI.org's native spelling can agree: "Łukasz
// Kaiser" and "Lukasz Kaiser", "Gülşen" and "Gulsen". Marks on other scripts
// are kept: they tell letters apart there, as in Cyrillic "й" and "и".
// Letters such as "æ", "œ" and "þ" stay as they are.
func foldLatinAccents(text string) string {
	var folded strings.Builder
	var base rune
	for _, r := range norm.NFD.String(text) {
		if unicode.Is(unicode.Mn, r) {
			if unicode.Is(unicode.Latin, base) {
				continue
			}
		} else {
			base = r
			if plain, ok := latinLetters[r]; ok {
				r = plain
			}
		}
		folded.WriteRune(r)
	}
	return norm.NFC.String(folded.String())
}

// LookupDOI resolves a DOI to its archive paper, preferring SciDB's
// authoritative DOI-to-file mapping and verifying search candidates through
// their lightweight JSON metadata endpoints.
func (c *Client) LookupDOI(ctx context.Context, doi string) (*Paper, error) {
	doi = NormalizeDOI(doi)
	if doi == "" {
		return nil, httpx.NewError(httpx.CodeInvalidArgument, "empty_doi", "doi is empty")
	}
	if !ValidDOI(doi) {
		return nil, httpx.NewError(httpx.CodeInvalidArgument, "invalid_doi", fmt.Sprintf("invalid DOI: %s", doi))
	}
	if err := c.requireArchiveAccess(ctx); err != nil {
		return nil, err
	}
	// Without a session or a mirror, every step below would fail the same way.
	if _, err := c.baseURLFor(ctx); err != nil {
		return nil, err
	}

	var paper *Paper
	scidbErr := c.onMirror(ctx, func(base string) error {
		var err error
		paper, err = c.paperFromSciDBPage(ctx, base, doi)
		return err
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if scidbErr == nil {
		return paper, nil
	}

	// A miss is only conclusive when every archive request answered: SciDB
	// without the record and each search with a page. Remember the last
	// request that failed instead, so an outage is not reported as the paper
	// being absent. A NOT_FOUND answer is itself one of those misses.
	var failure error
	noteFailure := func(err error) {
		if err != nil && !isNotFound(err) {
			failure = err
		}
	}
	noteFailure(scidbErr)

	metadata, metadataErr := c.citationMetadataFromDOI(ctx, doi)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if metadata.DOI != "" && !strings.EqualFold(NormalizeDOI(metadata.DOI), doi) {
		return nil, httpx.NewError(httpx.CodeUpstream, "doi_mismatch", "DOI metadata resolved to a different DOI")
	}

	queries := make([]string, 0, 3)
	if metadata.Title != "" {
		queries = append(queries, metadata.Title)
	}
	queries = append(queries, doi)
	if id := ArxivID(doi); id != "" {
		queries = append(queries, id)
	}
	// Each query's candidates are checked before the next query is searched,
	// so a paper the title search finds costs no further searches. The order
	// of checking, and so the result, is the same as checking all at the end.
	seenQueries := make(map[string]struct{}, len(queries))
	seenBooks := make(map[string]struct{})
	checked := 0
	for _, query := range queries {
		key := NormalizeTitle(query)
		if key == "" {
			continue
		}
		if _, seen := seenQueries[key]; seen {
			continue
		}
		seenQueries[key] = struct{}{}
		var candidates []*Book
		for _, results := range c.searchIndexes(ctx, query) {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if results.err != nil {
				noteFailure(results.err)
				continue
			}
			// Partial matches are welcome here: each candidate must still
			// agree on the title and carry the DOI on its record page.
			for _, book := range results.books() {
				if book == nil || book.Hash == "" {
					continue
				}
				// When CSL identity is available, only inspect cards whose own
				// title agrees. A conflicting title cannot be repaired by a DOI
				// mention in its description.
				if metadata.Title != "" && NormalizeTitle(book.Title) != NormalizeTitle(metadata.Title) {
					continue
				}
				if _, seen := seenBooks[book.Hash]; seen {
					continue
				}
				// A common title may have many identical editions. Keep DOI
				// lookup bounded to a small set of likely record pages.
				if checked+len(candidates) >= maxDOICandidates {
					break
				}
				seenBooks[book.Hash] = struct{}{}
				candidates = append(candidates, book)
			}
		}
		paper, err := c.verifyCandidates(ctx, doi, metadata, candidates, noteFailure)
		if err != nil || paper != nil {
			return paper, err
		}
		if checked += len(candidates); checked >= maxDOICandidates {
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// The archive failure keeps its own code (UPSTREAM, UPSTREAM_BLOCKED or
	// REQUEST_TIMEOUT); DOI.org's failure is context only, rendered as plain
	// text so a structured doi.org error never matches before the archive failure.
	if failure != nil {
		if metadataErr != nil {
			return nil, fmt.Errorf("could not check the archive for DOI %s: %w (DOI metadata: %s)", doi, failure, metadataErr.Error())
		}
		return nil, fmt.Errorf("could not check the archive for DOI %s: %w", doi, failure)
	}
	return nil, httpx.NewError(httpx.CodeNotFound, "doi_not_found", fmt.Sprintf("no verified archive record found for DOI: %s. The archive may hold another version of the paper, such as the published one under its own DOI: search article_search by the title and download a match by its hash", doi))
}

func isNotFound(err error) bool {
	pe, ok := errors.AsType[*httpx.ProviderError](err)
	return ok && pe != nil && pe.Code == httpx.CodeNotFound
}

// indexResults is one index's search for a DOI lookup.
type indexResults struct {
	searchResults
	err error
}

// searchIndexes searches the main and journal indexes for query at once, and
// returns them in that order.
func (c *Client) searchIndexes(ctx context.Context, query string) [2]indexResults {
	var results [2]indexResults
	var wait sync.WaitGroup
	for i, index := range []string{"", "journals"} {
		wait.Go(func() {
			results[i].searchResults, results[i].err = c.search(ctx, query, SearchOptions{Index: index, Page: 1}, "")
		})
	}
	wait.Wait()
	return results
}

func (r searchResults) books() []*Book {
	var books []*Book
	for _, card := range r.cards {
		if card.Prefix == "md5" {
			books = append(books, bookFromCard(card, r.pageURL))
		}
	}
	return books
}

// verifyCandidates reads the candidates' record identities a few at a time
// through the lightweight JSON metadata endpoint and returns the first, in
// candidate order, whose record carries the DOI. It returns as soon as that
// candidate and every one before it have been read, and cancels the rest. It
// returns an error only when ctx ends.
func (c *Client) verifyCandidates(ctx context.Context, doi string, metadata Citation, candidates []*Book, noteFailure func(error)) (*Paper, error) {
	type checked struct {
		identity Identity
		err      error
	}
	for len(candidates) > 0 {
		batch := candidates[:min(len(candidates), doiRecordChecks)]
		candidates = candidates[len(batch):]
		batchCtx, cancel := context.WithCancel(ctx)
		results := make([]chan checked, len(batch))
		for i, book := range batch {
			results[i] = make(chan checked, 1)
			go func() {
				identity, err := c.archiveIdentityForBook(batchCtx, book)
				results[i] <- checked{identity, err}
			}()
		}
		for i, book := range batch {
			var result checked
			select {
			case result = <-results[i]:
			case <-ctx.Done():
				cancel()
				return nil, ctx.Err()
			}
			if result.err != nil {
				noteFailure(result.err)
				continue
			}
			if SameWork(doi, metadata, result.identity) {
				cancel()
				return paperFromBook(book, doi), nil
			}
		}
		cancel()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

func (c *Client) archiveIdentityForBook(ctx context.Context, book *Book) (Identity, error) {
	if book == nil || book.Hash == "" {
		return Identity{}, httpx.NewError(httpx.CodeNotFound, "missing_hash", "archive search result has no record hash")
	}
	hash, err := normalizeHash(book.Hash)
	if err != nil {
		return Identity{}, err
	}
	var identity Identity
	err = c.onMirror(ctx, func(base string) error {
		var err error
		identity, err = c.archiveIdentityOn(ctx, base, hash)
		return err
	})
	return identity, err
}

// archiveIdentityOn reads one record's own identity from its lightweight
// JSON metadata endpoint, never from its full HTML page.
func (c *Client) archiveIdentityOn(ctx context.Context, base, hash string) (Identity, error) {
	recordURL := buildPathURL(base, "/db/aarecord_elasticsearch/md5:"+escapeRecordValue(hash)+".json")
	var record recordPayload
	if err := c.fetchJSON(ctx, recordURL, &record, "record details"); err != nil {
		return Identity{}, err
	}
	return identityFromJSON(&record), nil
}

// citationMetadataFromDOI reads DOI.org's CSL representation. Search snippets
// are only candidate generators; this metadata is the external work identity
// used to verify the archive's own title and complete author list.
//
// DOI.org has its own deadline: the archive can find the paper without it, so
// a DOI.org that hangs must not use up the whole lookup.
func (c *Client) citationMetadataFromDOI(ctx context.Context, doi string) (Citation, error) {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, doiTimeout)
	defer cancel()
	metadata, err := c.fetchCitationMetadata(ctx, doi)
	if err != nil && parent.Err() == nil && ctx.Err() != nil {
		err = fmt.Errorf("doi.org did not answer within %s", doiTimeout)
	}
	return metadata, err
}

func (c *Client) fetchCitationMetadata(ctx context.Context, doi string) (Citation, error) {
	metadataURL := &url.URL{Scheme: "https", Host: "doi.org", Path: "/" + doi}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metadataURL.String(), nil)
	if err != nil {
		return Citation{}, err
	}
	req.Header.Set("Accept", "application/vnd.citationstyles.csl+json")
	resp, err := c.http.Do(req)
	if err != nil {
		return Citation{}, err
	}
	body, readErr := httpx.ReadBounded(resp.Body, 1<<20)
	closeErr := resp.Body.Close()
	if readErr != nil {
		return Citation{}, readErr
	}
	if closeErr != nil {
		return Citation{}, closeErr
	}
	if resp.StatusCode != http.StatusOK {
		return Citation{}, fmt.Errorf("doi.org returned status %d", resp.StatusCode)
	}
	return ParseCitation(body)
}
