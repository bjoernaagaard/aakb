package aakb

import (
	"context"
	"strings"

	"github.com/SokolskyNikita/annas-mcp/papers"
)

// ReadRequest selects a work and a character budget (default 8000, maximum
// 16000). Replay a continuation with the same ID.
type ReadRequest struct {
	ID, Continuation string
	Limit            int
}

// Article is a bounded excerpt. Source is "openalex_tei" for full text
// or "abstract" for an abstract. An empty Continuation marks the end.
type Article struct {
	OpenAlexID   string    `json:"openalex_id,omitempty"`
	Title        string    `json:"title"`
	URL          string    `json:"url,omitempty"`
	Source       string    `json:"source"`
	Text         string    `json:"text"`
	Passages     []Passage `json:"passages"`
	SHA256       string    `json:"sha256,omitempty"`
	Continuation string    `json:"continuation,omitempty"`
	Warnings     []string  `json:"warnings"`
}

// Passage is an extracted paragraph with a section heading when available.
type Passage struct {
	Section   string `json:"section,omitempty"`
	Paragraph int    `json:"paragraph"`
	Text      string `json:"text"`
}

// ReadArticle reads one excerpt from OpenAlex by work ID, work URL, or DOI.
// If ctx has no deadline, ReadArticle applies a 60-second deadline.
func (c *Client) ReadArticle(ctx context.Context, req ReadRequest) (*Article, error) {
	const op = "read"
	ctx, cancel := withDefaultTimeout(ctx, defaultTimeout)
	defer cancel()
	if err := classify(ctx, op, nil); err != nil {
		return nil, err
	}
	if c.papers == nil {
		return nil, newError(op, CodeConfig, "OpenAlexKey is required to read articles")
	}
	if strings.TrimSpace(req.ID) == "" {
		return nil, newError(op, CodeInvalidArgument, "id is required")
	}
	if req.Limit < 0 || req.Limit > 16000 {
		return nil, newError(op, CodeInvalidArgument, "limit must be between 1 and 16000 characters, or zero for the default")
	}
	result, err := c.papers.Read(ctx, papers.ReadOptions{ID: req.ID, Continuation: req.Continuation, Limit: req.Limit})
	if err := classify(ctx, op, err); err != nil {
		return nil, err
	}
	return articleResult(result), nil
}
