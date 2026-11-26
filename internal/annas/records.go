package annas

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/bjoernaagaard/aakb/internal/httpx"
)

// Details holds the normalized metadata for one archive file or catalog record.
type Details struct {
	ID                    string              `json:"id"`
	Hash                  string              `json:"hash,omitempty"`
	URL                   string              `json:"url"`
	Title                 string              `json:"title"`
	Authors               string              `json:"authors,omitempty"`
	Publisher             string              `json:"publisher,omitempty"`
	Edition               string              `json:"edition,omitempty"`
	Year                  string              `json:"year,omitempty"`
	Languages             []string            `json:"languages,omitempty"`
	Content               string              `json:"content,omitempty"`
	Format                string              `json:"format,omitempty"`
	SizeBytes             int64               `json:"size_bytes,omitempty"`
	Description           string              `json:"description,omitempty"`
	AlternativeTitles     []string            `json:"alternative_titles,omitempty"`
	AlternativeAuthors    []string            `json:"alternative_authors,omitempty"`
	AlternativePublishers []string            `json:"alternative_publishers,omitempty"`
	AlternativeEditions   []string            `json:"alternative_editions,omitempty"`
	Identifiers           map[string][]string `json:"identifiers,omitempty"`
	Classifications       map[string][]string `json:"classifications,omitempty"`
	Subjects              []string            `json:"subjects,omitempty"`
	OriginalFilename      string              `json:"original_filename,omitempty"`
	AddedDate             string              `json:"added_date,omitempty"`
	Sources               []string            `json:"sources,omitempty"`
	FastDownload          bool                `json:"fast_download"`
	Problems              []Problem           `json:"problems,omitempty"`
	Stats                 *RecordStats        `json:"stats,omitempty"`
	RecentlyDownloaded    bool                `json:"recently_downloaded,omitempty"`
}

// Problem describes a reported file issue and, when known, a better replacement hash.
type Problem struct {
	Type        string `json:"type"`
	Description string `json:"description"`
	BetterHash  string `json:"better_hash,omitempty"`
}

// RecordStats contains community download and quality counters for a file.
type RecordStats struct {
	Downloads    int `json:"downloads"`
	GreatQuality int `json:"great_quality"`
	Reports      int `json:"reports"`
	Lists        int `json:"lists"`
	Comments     int `json:"comments"`
}

// LooseString unmarshals a JSON string, number, boolean, or null into a string.
type LooseString string

// UnmarshalJSON accepts string, numeric, boolean, or null JSON values.
func (s *LooseString) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	switch {
	case bytes.Equal(data, []byte("null")):
		*s = ""
	case len(data) > 0 && data[0] == '"':
		var value string
		if err := json.Unmarshal(data, &value); err != nil {
			return err
		}
		*s = LooseString(value)
	default:
		*s = LooseString(data)
	}
	return nil
}

// LooseStrings unmarshals either a JSON array of loose strings or a single scalar value.
type LooseStrings []string

// UnmarshalJSON accepts a JSON array of loose values or a single scalar value.
func (s *LooseStrings) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '[' {
		var value LooseString
		if err := value.UnmarshalJSON(data); err != nil {
			return err
		}
		*s = nil
		if value != "" {
			*s = LooseStrings{string(value)}
		}
		return nil
	}
	var values []LooseString
	if err := json.Unmarshal(data, &values); err != nil {
		return err
	}
	*s = make(LooseStrings, 0, len(values))
	for _, value := range values {
		*s = append(*s, string(value))
	}
	return nil
}

type recordPayload struct {
	ID              string `json:"id"`
	FileUnifiedData struct {
		TitleBest               LooseString             `json:"title_best"`
		TitleAdditional         LooseStrings            `json:"title_additional"`
		AuthorBest              LooseString             `json:"author_best"`
		AuthorAdditional        LooseStrings            `json:"author_additional"`
		PublisherBest           LooseString             `json:"publisher_best"`
		PublisherAdditional     LooseStrings            `json:"publisher_additional"`
		EditionVariaBest        LooseString             `json:"edition_varia_best"`
		EditionVariaAdditional  LooseStrings            `json:"edition_varia_additional"`
		YearBest                LooseString             `json:"year_best"`
		ExtensionBest           LooseString             `json:"extension_best"`
		FilesizeBest            LooseString             `json:"filesize_best"`
		ContentTypeBest         LooseString             `json:"content_type_best"`
		StrippedDescriptionBest LooseString             `json:"stripped_description_best"`
		MostLikelyLanguageCodes LooseStrings            `json:"most_likely_language_codes"`
		AddedDateBest           LooseString             `json:"added_date_best"`
		OriginalFilenameBest    LooseString             `json:"original_filename_best"`
		HasAADownloads          LooseString             `json:"has_aa_downloads"`
		IdentifiersUnified      map[string]LooseStrings `json:"identifiers_unified"`
		ClassificationsUnified  map[string]LooseStrings `json:"classifications_unified"`
		Problems                []struct {
			Type             LooseString `json:"type"`
			Descr            LooseString `json:"descr"`
			BetterAarecordID LooseString `json:"better_aarecord_id"`
		} `json:"problems"`
	} `json:"file_unified_data"`
	SearchOnlyFields struct {
		SearchRecordSources LooseStrings `json:"search_record_sources"`
	} `json:"search_only_fields"`
	Additional struct {
		Path LooseString `json:"path"`
	} `json:"additional"`
}

const (
	maxDetailDescription = 4000
	maxDetailAlternates  = 10
	maxDetailIdentifiers = 20
	maxDetailSubjects    = 15
)

var (
	recordPrefixPattern   = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	detailIdentifiers     = []string{"isbn13", "isbn10", "doi", "issn", "iso", "oclc", "lccn", "ol", "goodreads", "gbooks", "librarything", "ocaid", "asin", "pmid", "arxiv"}
	detailClassifications = []string{"ddc", "lcc", "udc"}
	detailSubjectKeys     = []string{"openlib_subject", "edsebk_subject", "rgb_subject", "zlib_category_name"}

	problemLabels = map[string]string{
		"lgrsnf_visible":         "Not visible in Libgen.rs Non-Fiction",
		"lgrsfic_visible":        "Not visible in Libgen.rs Fiction",
		"lgli_visible":           "Not visible in Libgen.li",
		"lgli_broken":            "Marked broken in Libgen.li",
		"zlib_missing":           "Missing from Z-Library",
		"zlib_spam":              "Marked as spam in Z-Library",
		"zlib_bad_file":          "Marked as a bad file in Z-Library",
		"duxiu_pdg_broken_files": "Not all pages could be converted to PDF",
		"upload_exiftool_failed": "Running exiftool failed on this file",
	}

	numberedAffiliations = regexp.MustCompile(`\s*;\s*1\s+\S`)
	affiliationLabel     = regexp.MustCompile(`(?i)\s*\baffiliations?:`)
	affiliationKeywords  = regexp.MustCompile(`(?i)\b(affiliation|universit|institut|department|division|school|college|laborator|hospital|clinic|academy|research|faculty|centre|center|gmbh)`)
	nameThenAffiliation  = regexp.MustCompile(`^([^,;.()]+, [^.;()]{1,40}?)\.\.? (.+)$`)
)

// Details fetches full JSON metadata for a file hash, catalog ID, or archive record URL.
func (c *Client) Details(ctx context.Context, id string) (*Details, error) {
	prefix, value, err := ParseRecordID(id)
	if err != nil {
		return nil, err
	}
	if err := c.requireArchiveAccess(ctx); err != nil {
		return nil, err
	}
	var details *Details
	err = c.onMirror(ctx, func(base string) error {
		var fetchErr error
		details, fetchErr = c.detailsOn(ctx, base, prefix, value)
		return fetchErr
	})
	if err != nil {
		if pe, ok := errors.AsType[*httpx.ProviderError](err); ok && pe != nil && pe.Code == httpx.CodeNotFound {
			return nil, httpx.NewError(httpx.CodeNotFound, "record_not_found", fmt.Sprintf("The archive has no record %s:%s", prefix, value))
		}
		return nil, err
	}
	return details, nil
}

func (c *Client) detailsOn(ctx context.Context, base, prefix, value string) (*Details, error) {
	var summary *MD5Summary
	var wg sync.WaitGroup
	if prefix == "md5" {
		wg.Go(func() {
			// Summary counters are best-effort; record details remain useful without them.
			if fetched, err := c.FetchSummary(ctx, base, value); err == nil {
				summary = fetched
			}
		})
	}
	var record recordPayload
	recordURL := buildPathURL(base, "/db/aarecord_elasticsearch/"+prefix+":"+escapeRecordValue(value)+".json")
	err := c.fetchJSON(ctx, recordURL, &record, "record details")
	wg.Wait()
	if err != nil {
		return nil, err
	}
	details := detailsFromRecord(&record, base, prefix, value)
	if summary != nil {
		details.Stats = &RecordStats{
			Downloads:    summary.DownloadsTotal,
			GreatQuality: summary.GreatQualityCount,
			Reports:      summary.ReportsCount,
			Lists:        summary.ListsCount,
			Comments:     summary.CommentsCount,
		}
		details.RecentlyDownloaded = summary.DownloadStillActive == 1
	}
	return details, nil
}

// ParseRecordID accepts a 32-character MD5 hash, md5:<hash>, a catalog ID such
// as isbndb:9780521879286 or ol:OL123M, or an archive record URL or path.
func ParseRecordID(input string) (prefix, value string, err error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", "", httpx.NewError(httpx.CodeInvalidArgument, "empty_id", "id is empty")
	}
	if strings.HasPrefix(input, "/") || strings.Contains(input, "://") {
		if parsed, parseErr := url.Parse(input); parseErr == nil {
			if p, v, ok := recordPath(parsed.Path); ok {
				if unescaped, unescapeErr := url.PathUnescape(v); unescapeErr == nil {
					v = unescaped
				}
				input = p + ":" + v
			}
		}
	}
	if hash, hashErr := normalizeHash(input); hashErr == nil {
		return "md5", hash, nil
	}
	prefix, value, found := strings.Cut(input, ":")
	prefix = strings.ToLower(prefix)
	if !found || !recordPrefixPattern.MatchString(prefix) || value == "" || len(value) > 300 || strings.ContainsAny(value, " \t\r\n?#") {
		return "", "", httpx.NewError(httpx.CodeInvalidArgument, "invalid_id", "id must be a 32-character hash from search or a record id such as isbndb:9780521879286")
	}
	if prefix == "md5" {
		hash, hashErr := normalizeHash(value)
		if hashErr != nil {
			return "", "", hashErr
		}
		return prefix, hash, nil
	}
	return prefix, value, nil
}

func recordPath(href string) (prefix, id string, ok bool) {
	parsed, err := url.Parse(href)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || !strings.HasPrefix(parsed.Path, "/") {
		return "", "", false
	}
	prefix, id, found := strings.Cut(strings.TrimPrefix(parsed.Path, "/"), "/")
	id = strings.TrimSuffix(id, "/")
	if !found || id == "" || !recordPrefixPattern.MatchString(prefix) {
		return "", "", false
	}
	return prefix, id, true
}

func escapeRecordValue(value string) string {
	segments := strings.Split(value, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	return strings.Join(segments, "/")
}

func detailsFromRecord(record *recordPayload, base, prefix, value string) *Details {
	file := &record.FileUnifiedData
	id := record.ID
	if id == "" {
		id = prefix + ":" + value
	}
	recordURLPath := string(record.Additional.Path)
	if !strings.HasPrefix(recordURLPath, "/") {
		recordURLPath = "/" + prefix + "/" + escapeRecordValue(value)
	}
	details := &Details{
		ID:                    id,
		URL:                   buildPathURL(base, recordURLPath),
		Title:                 strings.TrimSpace(string(file.TitleBest)),
		Authors:               CleanAuthors(string(file.AuthorBest)),
		Publisher:             strings.TrimSpace(string(file.PublisherBest)),
		Edition:               strings.TrimSpace(string(file.EditionVariaBest)),
		Year:                  strings.TrimSpace(string(file.YearBest)),
		Languages:             distinctValues(file.MostLikelyLanguageCodes, 0, ""),
		Content:               string(file.ContentTypeBest),
		Format:                strings.ToLower(string(file.ExtensionBest)),
		Description:           truncateDetailText(strings.TrimSpace(string(file.StrippedDescriptionBest)), maxDetailDescription),
		AlternativeTitles:     distinctValues(file.TitleAdditional, maxDetailAlternates, file.TitleBest),
		AlternativeAuthors:    distinctValues(file.AuthorAdditional, maxDetailAlternates, file.AuthorBest),
		AlternativePublishers: distinctValues(file.PublisherAdditional, maxDetailAlternates, file.PublisherBest),
		AlternativeEditions:   distinctValues(file.EditionVariaAdditional, maxDetailAlternates, file.EditionVariaBest),
		OriginalFilename:      string(file.OriginalFilenameBest),
		AddedDate:             string(file.AddedDateBest),
		Sources:               distinctValues(record.SearchOnlyFields.SearchRecordSources, 0, ""),
		FastDownload:          string(file.HasAADownloads) == "1" || string(file.HasAADownloads) == "true",
	}
	if prefix == "md5" {
		details.Hash = value
	}
	if size, err := strconv.ParseInt(string(file.FilesizeBest), 10, 64); err == nil && size > 0 {
		details.SizeBytes = size
	}
	for _, identifier := range detailIdentifiers {
		if values := distinctValues(file.IdentifiersUnified[identifier], maxDetailIdentifiers, ""); len(values) > 0 {
			if details.Identifiers == nil {
				details.Identifiers = make(map[string][]string)
			}
			details.Identifiers[identifier] = values
		}
	}
	for _, key := range detailClassifications {
		if values := distinctValues(file.ClassificationsUnified[key], maxDetailIdentifiers, ""); len(values) > 0 {
			if details.Classifications == nil {
				details.Classifications = make(map[string][]string)
			}
			details.Classifications[key] = values
		}
	}
	var subjects LooseStrings
	for _, key := range detailSubjectKeys {
		for _, subject := range file.ClassificationsUnified[key] {
			if scheme, rest, found := strings.Cut(subject, "/"); found && key == "edsebk_subject" && !strings.Contains(scheme, " ") {
				subject = rest
			}
			subjects = append(subjects, strings.TrimSpace(subject))
		}
	}
	details.Subjects = distinctValues(subjects, maxDetailSubjects, "")
	for _, problem := range file.Problems {
		label := problemLabels[string(problem.Type)]
		if label == "" {
			label = strings.ReplaceAll(string(problem.Type), "_", " ")
		}
		if reason := strings.TrimSpace(string(problem.Descr)); reason != "" && !strings.EqualFold(reason, label) {
			label += " (" + reason + ")"
		}
		details.Problems = append(details.Problems, Problem{
			Type:        string(problem.Type),
			Description: label,
			BetterHash:  strings.TrimPrefix(string(problem.BetterAarecordID), "md5:"),
		})
	}
	return details
}

func distinctValues(values LooseStrings, limit int, except LooseString) []string {
	seen := map[string]bool{strings.ToLower(strings.TrimSpace(string(except))): true}
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		key := strings.ToLower(value)
		if value == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, value)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out
}

func truncateDetailText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := text[:limit]
	for !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	if i := strings.LastIndex(cut, " "); i > limit*3/4 {
		cut = cut[:i]
	}
	return cut + "…"
}

// CleanAuthors removes appended institutional affiliations and normalizes spacing
// around commas and semicolons.
func CleanAuthors(authors string) string {
	authors = spaceAuthorSeparators(authors)
	if loc := numberedAffiliations.FindStringIndex(authors); loc != nil {
		authors = authors[:loc[0]]
	}
	if loc := affiliationLabel.FindStringIndex(authors); loc != nil {
		authors = authors[:loc[0]]
	}
	groups := strings.Split(trimAffiliationBlock(authors), ";")
	for i, group := range groups {
		groups[i] = trimNameAffiliation(strings.TrimSpace(group))
	}
	return spaceAuthorSeparators(strings.Join(groups, ";"))
}

func spaceAuthorSeparators(authors string) string {
	var groups []string
	for group := range strings.SplitSeq(authors, ";") {
		var names []string
		for name := range strings.SplitSeq(group, ",") {
			if name = strings.Join(strings.Fields(name), " "); name != "" {
				names = append(names, name)
			}
		}
		if len(names) > 0 {
			groups = append(groups, strings.Join(names, ", "))
		}
	}
	return strings.Join(groups, "; ")
}

func trimAffiliationBlock(authors string) string {
	for offset := 0; ; {
		open := strings.IndexAny(authors[offset:], "(（")
		if open < 0 {
			return authors
		}
		open += offset
		block := enclosedBlock(authors[open:])
		fullWidth := strings.HasPrefix(block, "（")
		if strings.Contains(block, "@") || (affiliationKeywords.MatchString(block) && (len(block) >= 40 || fullWidth)) {
			return strings.TrimSpace(authors[:open])
		}
		offset = open + len(block)
	}
}

func enclosedBlock(text string) string {
	depth := 0
	for i, r := range text {
		switch r {
		case '(', '（':
			depth++
		case ')', '）':
			depth--
		}
		if depth == 0 {
			return text[:i+utf8.RuneLen(r)]
		}
	}
	return text
}

func trimNameAffiliation(group string) string {
	match := nameThenAffiliation.FindStringSubmatch(group)
	if match == nil || !affiliationKeywords.MatchString(match[2]) {
		return group
	}
	name := match[1]
	if fields := strings.Fields(name); utf8.RuneCountInString(fields[len(fields)-1]) == 1 {
		name += "."
	}
	return name
}
