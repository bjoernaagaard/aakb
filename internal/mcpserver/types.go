package mcpserver

import "github.com/google/jsonschema-go/jsonschema"

type searchInput struct {
	Kind               string `json:"kind,omitempty" jsonschema:"book, article, metadata or openalex; empty auto-detects a DOI as an article"`
	Provider           string `json:"provider,omitempty" jsonschema:"openalex routes article search to OpenAlex; needs OPENALEX_API_KEY"`
	WorkType           string `json:"work_type,omitempty" jsonschema:"OpenAlex work type such as dissertation (openalex only)"`
	AuthorID           string `json:"author_id,omitempty" jsonschema:"OpenAlex author id (openalex only)"`
	InstitutionID      string `json:"institution_id,omitempty" jsonschema:"OpenAlex institution id (openalex only)"`
	RepositoryID       string `json:"repository_id,omitempty" jsonschema:"OpenAlex repository id (openalex only)"`
	Relation           string `json:"relation,omitempty" jsonschema:"citing, references or related, together with work_id (openalex only)"`
	WorkID             string `json:"work_id,omitempty" jsonschema:"OpenAlex work id that relation refers to"`
	Query              string `json:"query,omitempty" jsonschema:"title, author, topic, ISBN, DOI or MD5"`
	Title              string `json:"title,omitempty" jsonschema:"phrase the title must contain"`
	Author             string `json:"author,omitempty" jsonschema:"phrase the author must contain"`
	Publisher          string `json:"publisher,omitempty" jsonschema:"phrase the publisher must contain"`
	Year               string `json:"year,omitempty" jsonschema:"publication year, such as 2008"`
	Format             string `json:"format,omitempty" jsonschema:"comma-separated formats such as epub,pdf; prefix - to exclude"`
	Language           string `json:"language,omitempty" jsonschema:"comma-separated language codes such as en,de"`
	Source             string `json:"source,omitempty" jsonschema:"comma-separated source libraries or catalogs"`
	Content            string `json:"content,omitempty" jsonschema:"comma-separated content types such as book_nonfiction,magazine"`
	Sort               string `json:"sort,omitempty" jsonschema:"newest, oldest, largest, smallest, newest_added, oldest_added or random"`
	Page               int    `json:"page,omitempty" jsonschema:"result page, starting at 1"`
	Limit              int    `json:"limit,omitempty" jsonschema:"max results to return, 1 to 50"`
	IncludeStats       bool   `json:"include_stats,omitempty" jsonschema:"add download and quality counts; costs extra requests"`
	DownloadableOnly   bool   `json:"downloadable_only,omitempty" jsonschema:"only results the download tool can fetch"`
	SearchDescriptions bool   `json:"search_descriptions,omitempty" jsonschema:"also match query against descriptions"`
	Rank               bool   `json:"rank,omitempty" jsonschema:"rank exact results by availability, quality and popularity"`
	TimeoutSeconds     int    `json:"timeout_seconds,omitempty" jsonschema:"whole-operation timeout in seconds; 0 uses the default"`
}

type detailsInput struct {
	ID             string `json:"id" jsonschema:"MD5 hash, Anna's Archive /md5/ URL, DOI, catalog id such as isbndb:9780521879286, or an OpenAlex work id when OpenAlex is configured"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"whole-operation timeout in seconds; 0 uses the default"`
}

type readInput struct {
	ID             string `json:"id" jsonschema:"OpenAlex work id, DOI or work URL"`
	Continuation   string `json:"continuation,omitempty" jsonschema:"continuation token from a previous article_read result"`
	Limit          int    `json:"limit,omitempty" jsonschema:"max passages to return"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"whole-operation timeout in seconds; 0 uses the default"`
}

type downloadInput struct {
	Hash           string `json:"hash,omitempty" jsonschema:"MD5 hash or Anna's Archive /md5/ URL; give exactly one of hash, doi or openalex_id"`
	DOI            string `json:"doi,omitempty" jsonschema:"paper DOI, resolved to a file first"`
	OpenAlexID     string `json:"openalex_id,omitempty" jsonschema:"OpenAlex work id; downloads through OpenAlex first when configured"`
	Title          string `json:"title,omitempty" jsonschema:"filename title for a fresh download"`
	Format         string `json:"format,omitempty" jsonschema:"file extension such as epub or pdf; does not convert"`
	Artifact       string `json:"artifact,omitempty" jsonschema:"pdf or tei_xml, with openalex_id"`
	LocationID     string `json:"location_id,omitempty" jsonschema:"OpenAlex location id to download from, with openalex_id"`
	Version        string `json:"version,omitempty" jsonschema:"published, accepted or submitted, with openalex_id"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"whole-operation timeout in seconds; 0 uses the default"`
}

type accountInput struct {
	TimeoutSeconds int `json:"timeout_seconds,omitempty" jsonschema:"whole-operation timeout in seconds; 0 uses the default"`
}

// limits holds the bounds and choices the inferred schema cannot express.
type limits struct {
	min, max float64
	enum     []any
}

// inputSchema infers the schema of T and adds the given bounds, so a
// client sees the valid range and the SDK rejects a value outside it.
func inputSchema[T any](bounds map[string]limits) *jsonschema.Schema {
	schema, err := jsonschema.For[T](nil)
	if err != nil {
		panic("mcpserver: input type has no JSON schema: " + err.Error())
	}
	for name, bound := range bounds {
		property, ok := schema.Properties[name]
		if !ok {
			panic("mcpserver: no input property " + name)
		}
		if bound.enum != nil {
			property.Enum = bound.enum
			continue
		}
		property.Minimum = new(bound.min)
		property.Maximum = new(bound.max)
	}
	return schema
}

func timeoutLimit(maxSeconds float64) map[string]limits {
	return map[string]limits{"timeout_seconds": {min: 0, max: maxSeconds}}
}
