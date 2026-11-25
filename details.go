package aakb

import (
	"context"
	"regexp"
	"strings"

	"github.com/SokolskyNikita/annas-mcp/anna"
)

// RecordDetails contains the metadata for one file or catalog entry.
// Descriptions longer than 2000 characters end in an ellipsis.
type RecordDetails struct {
	ID           string              `json:"id"`
	Hash         string              `json:"hash,omitempty"`
	URL          string              `json:"url"`
	Title        string              `json:"title"`
	Authors      string              `json:"authors,omitempty"`
	Publisher    string              `json:"publisher,omitempty"`
	Edition      string              `json:"edition,omitempty"`
	Year         string              `json:"year,omitempty"`
	Languages    []string            `json:"languages,omitempty"`
	Format       string              `json:"format,omitempty"`
	SizeBytes    int64               `json:"size_bytes,omitempty"`
	Description  string              `json:"description,omitempty"`
	Identifiers  map[string][]string `json:"identifiers,omitempty"`
	Subjects     []string            `json:"subjects,omitempty"`
	Sources      []string            `json:"sources,omitempty"`
	FastDownload bool                `json:"fast_download"`
	Problems     []Problem           `json:"problems,omitempty"`
	Stats        *Stats              `json:"stats,omitempty"`
}

// Problem describes a reported file issue and, when known, a better copy.
type Problem struct {
	Type        string `json:"type"`
	Description string `json:"description"`
	BetterHash  string `json:"better_hash,omitempty"`
}

var openAlexID = regexp.MustCompile(`(?i)^W[0-9]+$`)

// Details looks up an MD5, archive record URL, catalog ID, or bare DOI.
// If ctx has no deadline, Details applies a 60-second deadline.
func (c *Client) Details(ctx context.Context, id string) (*RecordDetails, error) {
	const op = "details"
	ctx, cancel := withDefaultTimeout(ctx, defaultTimeout)
	defer cancel()
	if err := classify(ctx, op, nil); err != nil {
		return nil, err
	}
	id = strings.TrimSpace(id)
	if openAlexID.MatchString(id) {
		return nil, newError(op, CodeInvalidArgument, "use ReadArticle or Download for an OpenAlex work ID")
	}
	if strings.HasPrefix(id, "10.") && anna.IsDOIQuery(id) {
		paper, err := c.archive.LookupDOI(ctx, id, 0)
		if err := classify(ctx, op, err); err != nil {
			return nil, err
		}
		if paper.Hash == "" {
			return nil, newError(op, CodeNotFound, "no archive record with a file hash was found for the DOI")
		}
		id = paper.Hash
	}
	details, err := c.archive.Details(ctx, id, 0)
	if err := classify(ctx, op, err); err != nil {
		return nil, err
	}
	return recordDetails(details), nil
}

func shortenDescription(description string) string {
	const maxDescription = 2000
	count := 0
	for i := range description {
		if count == maxDescription {
			return description[:i] + "..."
		}
		count++
	}
	return description
}
