package adapter

import "github.com/bjoernaagaard/aakb"

// ArticleOutput is the command and MCP wire representation of an article.
type ArticleOutput struct {
	OpenAlexID string         `json:"openalex_id,omitempty"`
	Title      string         `json:"title"`
	URL        string         `json:"url,omitempty"`
	Source     string         `json:"source"`
	Text       string         `json:"text"`
	Passages   []aakb.Passage `json:"passages,omitempty"`
	SHA256     string         `json:"sha256,omitempty"`
	NextArgs   *nextReadArgs  `json:"next_args,omitempty"`
	Warnings   []string       `json:"warnings,omitempty"`
}

type nextReadArgs struct {
	ID           string `json:"id"`
	Continuation string `json:"continuation,omitempty"`
	Limit        int    `json:"limit,omitempty"`
}

// ArticleResponse preserves next_args for callers of the command and MCP tool.
func ArticleResponse(in aakb.ReadRequest, article *aakb.Article) *ArticleOutput {
	out := &ArticleOutput{
		OpenAlexID: article.OpenAlexID, Title: article.Title, URL: article.URL,
		Source: article.Source, Text: article.Text, Passages: article.Passages,
		SHA256: article.SHA256, Warnings: article.Warnings,
	}
	if article.Continuation != "" {
		out.NextArgs = &nextReadArgs{ID: in.ID, Continuation: article.Continuation, Limit: in.Limit}
	}
	return out
}
