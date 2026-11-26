package archive

import (
	"fmt"
	"html"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	xhtml "golang.org/x/net/html"
)

// The archive's search pages list up to 50 results per page and 100 pages.
const (
	// ResultsPerPage is the number of results one archive search page holds.
	ResultsPerPage = 50
	// MaxResultPages bounds how many search pages the client will read.
	MaxResultPages = 100
)

// Search result cards share one layout for files and catalog records. Each
// is a child of a results list. opacity-40 marks a file the archive warns
// might have issues. The list inside .js-partial-matches-show holds looser
// matches, which can ignore the page's filters.
type Card struct {
	// Prefix and ID name the record, as in /md5/<hash> or /isbndb/<isbn>.
	Prefix         string   `json:"prefix"`
	ID             string   `json:"id"`
	Href           string   `json:"href"`
	Title          string   `json:"title"`
	Authors        string   `json:"authors,omitempty"`
	Publisher      string   `json:"publisher,omitempty"`
	Edition        string   `json:"edition,omitempty"`
	Description    string   `json:"description,omitempty"`
	Meta           CardMeta `json:"meta"`
	PossibleIssues bool     `json:"possible_issues,omitempty"`
	Partial        bool     `json:"partial,omitempty"`
}

// CardMeta is a card's metadata line. Language joins up to three language
// names, most likely first; Content is a content type such as
// book_nonfiction; FastDownload marks files on the archive's fast download servers.
type CardMeta struct {
	Language     string   `json:"language,omitempty"`
	Format       string   `json:"format,omitempty"`
	Size         string   `json:"size,omitempty"`
	Year         string   `json:"year,omitempty"`
	Content      string   `json:"content,omitempty"`
	Sources      []string `json:"sources,omitempty"`
	FastDownload bool     `json:"fast_download,omitempty"`
	// Unparsed holds parts of the line that were not recognized, apart from
	// a leading label such as "Metadata" and the record id after the sources.
	Unparsed []string `json:"unparsed,omitempty"`
}

// Content types are recognized by the emoji before their label.
var contentEmoji = map[rune]string{
	'📗': "book_unknown", '📘': "book_nonfiction", '📕': "book_fiction",
	'📄': "journal_article", '📝': "standards_document", '📰': "magazine",
	'💬': "book_comic", '🎶': "musical_score", '🎧': "audiobook", '🤨': "other",
}

var (
	recordPathPattern   = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	cardLanguagePattern = regexp.MustCompile(`^(.+?)\s*\[[A-Za-z_-]+\]$`)
	cardFormatPattern   = regexp.MustCompile(`^[A-Z0-9]{1,10}$`)
	cardSizePattern     = regexp.MustCompile(`(?i)^\d+(?:[.,]\d+)?\s*[KMGT]?B$`)
	cardYearPattern     = regexp.MustCompile(`^\d{1,4}$`)
	sourceNamePattern   = regexp.MustCompile(`^[a-z0-9_]+$`)
	resultsTotalPattern = regexp.MustCompile(`^Results ([\d,]+)\s*[-–]\s*([\d,]+) \(([\d,]+)(\+?) total\)$`)
)

// CardText is a search card's parts as the page has them, before any
// cleanup: what CardTexts extracts and ReadCard interprets.
type CardText struct {
	Href           string `json:"href"`
	Title          string `json:"title"`
	Authors        string `json:"authors,omitempty"`
	PublisherLine  string `json:"publisher_line,omitempty"`
	PublisherLink  string `json:"publisher_link,omitempty"`
	Description    string `json:"description,omitempty"`
	MetadataLine   string `json:"metadata_line,omitempty"`
	PossibleIssues bool   `json:"possible_issues,omitempty"`
	Partial        bool   `json:"partial,omitempty"`
}

// CardTexts extracts the cards of a search page, partial matches included,
// in page order. A card is a child of a results list with a title link; the
// list's scripts and ads are skipped.
func CardTexts(root *xhtml.Node) []CardText {
	var texts []CardText
	if root == nil {
		return texts
	}
	for _, list := range findNodes(root, withClass("js-aarecord-list-outer")) {
		partial := ancestorHasClass(list, "js-partial-matches-show")
		for _, card := range elementChildren(list) {
			link := findFirstNode(card, titleLinkNode)
			if link == nil {
				continue
			}
			var publisherLine, publisherLink string
			if span := findFirstNode(card, iconSpanNode("icon-[mdi--company]")); span != nil && isElement(span.Parent, "a") {
				publisherLine = nodeText(span.Parent)
				publisherLink = nodeAttr(span.Parent, "href")
			}
			var authors string
			if span := findFirstNode(card, iconSpanNode("icon-[mdi--user-edit]")); span != nil && isElement(span.Parent, "a") {
				authors = nodeText(span.Parent)
			}
			var description string
			if more := findFirstNode(card, readMoreNode); more != nil {
				description = nodeText(prevElementSibling(more))
			}
			texts = append(texts, CardText{
				Href:           nodeAttr(link, "href"),
				Title:          nodeText(link),
				Authors:        authors,
				PublisherLine:  publisherLine,
				PublisherLink:  publisherLink,
				Description:    description,
				MetadataLine:   nodeOwnText(findFirstNode(card, metadataLineNode)),
				PossibleIssues: nodeHasClass(card, "opacity-40"),
				Partial:        partial,
			})
		}
	}
	return texts
}

// ReadCard interprets a card's text. A card without a record link or a
// title is not read.
func ReadCard(text CardText) (Card, bool) {
	prefix, id, ok := RecordPath(text.Href)
	title := CleanText(text.Title)
	if !ok || title == "" {
		return Card{}, false
	}
	publisher, edition := publisherAndEdition(text.PublisherLine, text.PublisherLink)
	return Card{
		Prefix:         prefix,
		ID:             id,
		Href:           text.Href,
		Title:          title,
		Authors:        CleanAuthors(CleanText(text.Authors)),
		Publisher:      publisher,
		Edition:        edition,
		Description:    Snippet(CleanText(text.Description)),
		Meta:           ParseCardMeta(text.MetadataLine),
		PossibleIssues: text.PossibleIssues,
		Partial:        text.Partial,
	}, true
}

// SearchCards reads the result cards of a search page, partial matches
// included, in page order.
func SearchCards(root *xhtml.Node) []Card {
	var cards []Card
	for _, text := range CardTexts(root) {
		if card, ok := ReadCard(text); ok {
			cards = append(cards, card)
		}
	}
	return cards
}

// publisherAndEdition splits the line "publisher, edition". Its link searches
// for the publisher alone, so the link's query says where the publisher ends;
// a record without a publisher links to an empty search. When the link and
// the line disagree, the whole line counts as the publisher.
func publisherAndEdition(line, link string) (publisher, edition string) {
	line = CleanText(line)
	if line == "" {
		return "", ""
	}
	query, ok := strings.CutPrefix(link, "/search?q=")
	if !ok {
		return line, ""
	}
	candidates := []string{query}
	if decoded, err := url.QueryUnescape(query); err == nil && decoded != query {
		candidates = append(candidates, decoded)
	}
	for _, candidate := range candidates {
		candidate = CleanText(candidate)
		if candidate == "" {
			return "", line
		}
		if line == candidate {
			return line, ""
		}
		if rest, ok := strings.CutPrefix(line, candidate+", "); ok {
			return candidate, rest
		}
	}
	return line, ""
}

// RecordPath splits a record link such as /md5/<hash> or /isbndb/<isbn>.
func RecordPath(href string) (prefix, id string, ok bool) {
	parsed, err := url.Parse(href)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || !strings.HasPrefix(parsed.Path, "/") {
		return "", "", false
	}
	prefix, id, found := strings.Cut(strings.TrimPrefix(parsed.Path, "/"), "/")
	id = strings.TrimSuffix(id, "/")
	if !found || id == "" || !recordPathPattern.MatchString(prefix) {
		return "", "", false
	}
	return prefix, id, true
}

// ParseCardMeta reads the metadata line. Its parts are optional but always
// in this order: languages, extension, size, year, content type, sources
// (prefixed by 🧬 for SciDB and 🚀 for fast downloads) and a record id.
func ParseCardMeta(line string) CardMeta {
	const (
		stageLanguage = iota
		stageFormat
		stageSize
		stageYear
		stageContent
		stageSources
		stageDone
	)
	var meta CardMeta
	stage := stageLanguage
	index, recordID := -1, false
	for part := range strings.SplitSeq(line, "·") {
		part = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(part), "✅"))
		if part == "" {
			continue
		}
		index++
		if stage <= stageLanguage {
			// A record can list up to three languages, most likely first.
			if match := cardLanguagePattern.FindStringSubmatch(part); match != nil {
				if meta.Language != "" {
					meta.Language += ", "
				}
				meta.Language += match[1]
				continue
			}
		}
		if stage <= stageFormat && cardFormatPattern.MatchString(part) && strings.IndexFunc(part, unicode.IsLetter) >= 0 {
			meta.Format, stage = part, stageSize
			continue
		}
		if stage <= stageSize && cardSizePattern.MatchString(part) {
			meta.Size, stage = part, stageYear
			continue
		}
		if stage <= stageYear && cardYearPattern.MatchString(part) {
			meta.Year, stage = part, stageContent
			continue
		}
		if stage <= stageContent {
			first, _ := utf8.DecodeRuneInString(part)
			if content, ok := contentEmoji[first]; ok {
				meta.Content, stage = content, stageSources
				continue
			}
		}
		// Sources follow the content type, which every record has. Before it,
		// only a part with a slash, such as "🚀/lgli", can be the sources.
		if stage == stageSources || (stage < stageSources && strings.Contains(part, "/")) {
			if sources, fast, ok := parseSources(part); ok {
				meta.Sources, meta.FastDownload, stage = sources, fast, stageDone
				continue
			}
		}
		switch {
		case index == 0 && stage == stageLanguage:
			// A label such as "Metadata" leads catalog records' lines.
		case stage == stageDone && !recordID:
			recordID = true
		default:
			meta.Unparsed = append(meta.Unparsed, part)
		}
	}
	return meta
}

func parseSources(part string) (sources []string, fastDownload, ok bool) {
	for token := range strings.SplitSeq(part, "/") {
		switch token = strings.TrimSpace(token); {
		case token == "🚀":
			fastDownload = true
		case token == "🧬":
		case sourceNamePattern.MatchString(token):
			sources = append(sources, token)
		default:
			return nil, false, false
		}
	}
	return sources, fastDownload, len(sources) > 0 || fastDownload
}

// ResultsTotal reads the archive's count, "Results 1-50 (500+ total)". The client
// reads the archive's pages in English; see ParseBaseURL.
func ResultsTotal(root *xhtml.Node) (total int, lowerBound, found bool) {
	count := resultsCount(root)
	return count.total, count.lowerBound, count.found
}

// resultsLine is the archive's "Results 1-50 (500+ total)": the page shows results
// from to to, of total.
type resultsLine struct {
	from, to, total   int
	lowerBound, found bool
}

func resultsCount(root *xhtml.Node) resultsLine {
	var line resultsLine
	if root == nil {
		return line
	}
	// The count stands just before the first results list's wrapper.
	lists := findNodes(root, withClass("js-aarecord-list-outer"))
	if len(lists) == 0 || lists[0].Parent == nil {
		return line
	}
	for sibling := lists[0].Parent.PrevSibling; sibling != nil; sibling = sibling.PrevSibling {
		if sibling.Type != xhtml.ElementNode {
			continue
		}
		match := resultsTotalPattern.FindStringSubmatch(strings.TrimSpace(nodeText(sibling)))
		if match == nil {
			continue
		}
		number := func(text string) (int, error) { return strconv.Atoi(strings.ReplaceAll(text, ",", "")) }
		from, err1 := number(match[1])
		to, err2 := number(match[2])
		total, err3 := number(match[3])
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		return resultsLine{from: from, to: to, total: total, lowerBound: match[4] == "+", found: true}
	}
	return line
}

// ShowsResults reports whether a page with no parsed cards still shows
// results, by its count or by links to files. If so, the archive changed its markup,
// and returning no results would read as the work being absent.
func ShowsResults(root *xhtml.Node, pageNumber int) bool {
	if root == nil {
		return false
	}
	if total, _, found := ResultsTotal(root); found && total > (max(pageNumber, 1)-1)*ResultsPerPage {
		return true
	}
	return findFirstNode(root, func(node *xhtml.Node) bool {
		return isElement(node, "a") && strings.HasPrefix(nodeAttr(node, "href"), "/md5/")
	}) != nil
}

// FacetValues lists the values the archive offers for a filter on this page.
func FacetValues(root *xhtml.Node, name string) []string {
	var values []string
	seen := map[string]bool{}
	for _, input := range findNodes(root, func(node *xhtml.Node) bool {
		return isElement(node, "input") && nodeAttr(node, "name") == name
	}) {
		value := nodeAttr(input, "value")
		if value == "" || strings.HasPrefix(value, "anti__") || seen[value] {
			continue
		}
		seen[value] = true
		values = append(values, value)
	}
	return values
}

// IsArchivePage reports whether root is one of the archive's pages,
// rather than a browser check or another site's answer.
func IsArchivePage(root *xhtml.Node) bool {
	if root == nil {
		return false
	}
	return findFirstNode(root, func(node *xhtml.Node) bool {
		if !isElement(node, "form") || nodeAttr(node, "action") != "/search" {
			return false
		}
		return findFirstNode(node, func(input *xhtml.Node) bool {
			return isElement(input, "input") && nodeAttr(input, "name") == "q"
		}) != nil
	}) != nil
}

// Kinds of problems the page checks report.
const (
	ProblemUnparsedMeta  = "unrecognized parts of metadata lines"
	ProblemDroppedCard   = "result links without a readable card"
	ProblemIssueMismatch = "cards whose grey-out and issues line disagree"
	ProblemPartialCount  = "partial matches that differ from the page's count"
	ProblemResultCount   = "results that differ from the page's count"
)

// issuesLine is the line the archive adds to the cards it greys out.
const issuesLine = "This file might have issues"

var firstNumberPattern = regexp.MustCompile(`\d+`)

// CheckSearchPage compares what SearchCards read with what the page itself
// shows: each result link should give a card, the numbers of results and
// partial matches should be the ones the archive prints, a greyed-out card should
// carry the issues line, and every part of each metadata line should be
// recognized.
func CheckSearchPage(root *xhtml.Node, cards []Card) Report {
	var report Report
	if root == nil {
		return report
	}
	read := make(map[string]bool, len(cards))
	exact, partial := 0, 0
	for _, card := range cards {
		read[card.Href] = true
		if card.Partial {
			partial++
		} else {
			exact++
		}
		for _, part := range card.Meta.Unparsed {
			report.Add(ProblemUnparsedMeta, part)
		}
	}
	for _, list := range findNodes(root, withClass("js-aarecord-list-outer")) {
		for _, card := range elementChildren(list) {
			link := findFirstNode(card, titleLinkNode)
			if link != nil && strings.Contains(nodeText(card), issuesLine) != nodeHasClass(card, "opacity-40") {
				report.Add(ProblemIssueMismatch, nodeAttr(link, "href"))
			}
		}
		for _, link := range findNodes(list, func(node *xhtml.Node) bool {
			return isElement(node, "a") && nodeAttr(node, "href") != ""
		}) {
			href := nodeAttr(link, "href")
			if _, _, ok := RecordPath(href); ok && !read[href] {
				read[href] = true
				report.Add(ProblemDroppedCard, href)
			}
		}
	}
	if line := resultsCount(root); line.found {
		if shown := max(line.to-line.from+1, 0); shown != exact {
			report.Add(ProblemResultCount, fmt.Sprintf("read %d, the page shows %d", exact, shown))
		}
	}
	shown := 0
	if remover := findFirstNode(root, withClass("js-partial-matches-remove")); remover != nil {
		if button := findFirstNode(remover, func(node *xhtml.Node) bool { return isElement(node, "button") }); button != nil {
			shown = atoiOrZero(firstNumberPattern.FindString(nodeText(button)))
		}
	}
	if shown != partial {
		report.Add(ProblemPartialCount, fmt.Sprintf("read %d, the page shows %d", partial, shown))
	}
	return report
}

// Limits on what a Report keeps, so a page the archive changed cannot flood a log
// with markup or long text.
const (
	maxReportSamples     = 3
	maxReportSampleRunes = 80
	maxReportBytes       = 600
)

// Report lists what a parser met on a page and could not read, by kind,
// with a few short examples of each. Examples are text the parser had
// already extracted, or record paths; never markup.
type Report struct {
	Problems []ReportProblem
}

// ReportProblem is one kind of thing a page had that a parser could not read.
type ReportProblem struct {
	Kind    string
	Count   int
	Samples []string
}

// Empty reports whether nothing went unread.
func (r *Report) Empty() bool {
	return len(r.Problems) == 0
}

// Add counts one occurrence of kind, keeping sample when it is new and
// there is room for it.
func (r *Report) Add(kind, sample string) {
	sample = reportSample(sample)
	for i := range r.Problems {
		problem := &r.Problems[i]
		if problem.Kind != kind {
			continue
		}
		problem.Count++
		if len(problem.Samples) < maxReportSamples && !slices.Contains(problem.Samples, sample) {
			problem.Samples = append(problem.Samples, sample)
		}
		return
	}
	r.Problems = append(r.Problems, ReportProblem{Kind: kind, Count: 1, Samples: []string{sample}})
}

// String renders the report as one line of at most 600 bytes, such as
// `unrecognized parts of metadata lines: 7 (e.g. "KFX", "Ms. Word")`.
func (r *Report) String() string {
	parts := make([]string, 0, len(r.Problems))
	for _, problem := range r.Problems {
		quoted := make([]string, len(problem.Samples))
		for i, sample := range problem.Samples {
			quoted[i] = fmt.Sprintf("%q", sample)
		}
		parts = append(parts, fmt.Sprintf("%s: %d (e.g. %s)", problem.Kind, problem.Count, strings.Join(quoted, ", ")))
	}
	return truncateBytes(strings.Join(parts, "; "), maxReportBytes)
}

// reportSample collapses whitespace, drops control characters and cuts the
// sample to 80 characters.
func reportSample(sample string) string {
	sample = strings.Join(strings.FieldsFunc(sample, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
	if utf8.RuneCountInString(sample) <= maxReportSampleRunes {
		return sample
	}
	runes := []rune(sample)
	return string(runes[:maxReportSampleRunes-1]) + "…"
}

func truncateBytes(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := text[:limit-len("…")]
	for !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + "…"
}

// Some records' text was HTML-escaped before the archive escaped it for its page,
// so it reads "Smith &amp; Jones" after the page itself is decoded.
// Only complete entities are decoded: legacy forms without a semicolon,
// such as "&para", are ordinary text here.
var escapedEntity = regexp.MustCompile(`&(?:#[0-9]{1,7}|#[xX][0-9a-fA-F]{1,6}|[A-Za-z][A-Za-z0-9]{1,31});`)

func decodeEntities(text string) string {
	if !strings.Contains(text, "&") {
		return text
	}
	return escapedEntity.ReplaceAllStringFunc(text, html.UnescapeString)
}

// Records imported from publishers can carry formatting markup in their
// text, such as "<i>AlphaFold</i>2", Crossref's <jats:p>, Wiley's <scp> or
// the <title> SPIE wraps paper titles in. Only known formatting tags are
// removed, so text such as "Vector<T>" is kept.
var (
	inlineMarkup = regexp.MustCompile(`(?i)</?(?:a|b|i|em|strong|sub|sup|u|span|small|sc|scp|tt|code|(?:jats|mml):[a-z-]+)(?:\s[^<>]*)?/?>`)
	blockMarkup  = regexp.MustCompile(`(?i)</?(?:p|br|div|li|ul|ol|h[1-6]|title)(?:\s[^<>]*)?/?>`)
)

// CleanText decodes doubly escaped entities, removes formatting markup and
// direction marks, and collapses whitespace.
func CleanText(text string) string {
	text = decodeEntities(text)
	if strings.Contains(text, "<") {
		text = blockMarkup.ReplaceAllString(inlineMarkup.ReplaceAllString(text, ""), " ")
	}
	return strings.Join(strings.Fields(strings.Map(dropDirectionMark, text)), " ")
}

// dropDirectionMark removes the invisible marks that steer right-to-left
// text in a browser, such as the right-to-left mark catalogs put around
// Hebrew titles. They mean nothing in plain text. Joiners, which shape
// letters in scripts such as Bengali and Persian, are kept.
func dropDirectionMark(r rune) rune {
	switch r {
	case '\u200e', '\u200f', '\u061c', '\ufeff',
		'\u202a', '\u202b', '\u202c', '\u202d', '\u202e',
		'\u2066', '\u2067', '\u2068', '\u2069':
		return -1
	}
	return r
}

// Snippet collapses whitespace and shortens text to at most 480 bytes,
// cutting at a word where it can.
func Snippet(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	const limit = 480
	if len(text) <= limit {
		return text
	}
	cut := text[:limit]
	for !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	if i := strings.LastIndex(cut, " "); i > 320 {
		cut = cut[:i]
	}
	return cut
}
