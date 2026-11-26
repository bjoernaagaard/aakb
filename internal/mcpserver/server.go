// Package mcpserver registers aakb's tools, prompt and guide on an MCP server.
package mcpserver

import (
	"context"
	"log/slog"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bjoernaagaard/aakb"
	"github.com/bjoernaagaard/aakb/internal/adapter"
)

// Version is the aakb release version reported to MCP clients.
// Release builds inject it with -ldflags; it defaults to a dev value.
var Version = "dev"

// New returns an MCP server backed by client. Logs go to the caller's
// logger, which must write to stderr so stdio stdout stays clean.
func New(client *aakb.Client, logger *slog.Logger) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "aakb", Version: Version}, &mcp.ServerOptions{
		Instructions: "Search for books, papers and catalog records, " +
			"inspect one record in full, and download a verified file. " +
			"Start with search, then get_details on a chosen result, then download by its hash or DOI. Use article_read for OpenAlex full text when configured. " +
			"Use only for material the user is entitled to obtain.",
		Logger: logger,
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "search",
		Title:       "Search books, papers and catalog records",
		Description: "Search books, papers or catalog records with one tool. kind is book, article, metadata or openalex; empty auto-detects a DOI query as an article. Exact results are deduped and optionally ranked; looser suggestions come back separately in partial_matches.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(true)},
		InputSchema: inputSchema[searchInput](map[string]limits{
			"kind":            {enum: []any{"", "book", "article", "metadata", "openalex"}},
			"sort":            {enum: []any{"", "relevance", "newest", "oldest", "largest", "smallest", "newest_added", "oldest_added", "random", "most_cited"}},
			"limit":           {min: 1, max: 50},
			"page":            {min: 1, max: 100000},
			"timeout_seconds": {min: 0, max: 600},
		}),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in searchInput) (*mcp.CallToolResult, *aakb.SearchResult, error) {
		ctx, cancel, err := adapter.WithTimeout(ctx, "search", in.TimeoutSeconds)
		if err != nil {
			return nil, nil, err
		}
		defer cancel()
		out, err := client.Search(ctx, aakb.SearchRequest{
			Kind: adapter.SearchKind(in.Kind, in.Provider), WorkType: in.WorkType,
			Query: in.Query, Title: in.Title, Author: in.Author, Publisher: in.Publisher, Year: in.Year,
			Formats: adapter.SplitList(in.Format), Languages: adapter.SplitList(in.Language),
			Sources: adapter.SplitList(in.Source), Content: adapter.SplitList(in.Content),
			Sort: aakb.Sort(in.Sort), Page: in.Page, Limit: in.Limit, Rank: in.Rank,
			IncludeStats: in.IncludeStats, DownloadableOnly: in.DownloadableOnly, SearchDescriptions: in.SearchDescriptions,
			OpenAlex: aakb.OpenAlexFilter{
				AuthorID: in.AuthorID, InstitutionID: in.InstitutionID, RepositoryID: in.RepositoryID,
				Relation: in.Relation, WorkID: in.WorkID,
			},
		})
		return nil, out, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_details",
		Title:       "Get record details",
		Description: "Full record for one hash, /md5/ URL, DOI, catalog id or OpenAlex work id: identifiers, edition, description, subjects, reported file problems and a better copy's hash when the archive knows one.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(true)},
		InputSchema: inputSchema[detailsInput](timeoutLimit(600)),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in detailsInput) (*mcp.CallToolResult, *aakb.RecordDetails, error) {
		ctx, cancel, err := adapter.WithTimeout(ctx, "details", in.TimeoutSeconds)
		if err != nil {
			return nil, nil, err
		}
		defer cancel()
		out, err := client.Details(ctx, in.ID)
		return nil, out, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "article_read",
		Title:       "Read a paper's full text",
		Description: "Bounded passages of OpenAlex structured full text with provenance and a continuation token. Needs OPENALEX_API_KEY. Abstract-only results say so instead of pretending to be full text.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(true)},
		InputSchema: inputSchema[readInput](map[string]limits{
			"limit":           {min: 0, max: 16000},
			"timeout_seconds": {min: 0, max: 600},
		}),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in readInput) (*mcp.CallToolResult, *adapter.ArticleOutput, error) {
		ctx, cancel, err := adapter.WithTimeout(ctx, "read", in.TimeoutSeconds)
		if err != nil {
			return nil, nil, err
		}
		defer cancel()
		req := aakb.ReadRequest{ID: in.ID, Continuation: in.Continuation, Limit: in.Limit}
		out, err := client.ReadArticle(ctx, req)
		if err != nil {
			return nil, nil, err
		}
		return nil, adapter.ArticleResponse(req, out), nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "download",
		Title:       "Download a verified file",
		Description: "Download one file by MD5 hash, paper DOI or OpenAlex work id. Sends progress notifications when the call carries a progress token. The file is checked against its MD5, a matching local file is reused without a new request, and an existing file is never overwritten.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: new(false), IdempotentHint: true, OpenWorldHint: new(true)},
		InputSchema: inputSchema[downloadInput](timeoutLimit(7200)),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in downloadInput) (*mcp.CallToolResult, *aakb.DownloadResult, error) {
		ctx, cancel, err := adapter.WithTimeout(ctx, "download", in.TimeoutSeconds)
		if err != nil {
			return nil, nil, err
		}
		defer cancel()
		out, err := client.Download(ctx, aakb.DownloadRequest{
			Hash: in.Hash, DOI: in.DOI, OpenAlexID: in.OpenAlexID, Title: in.Title, Format: in.Format,
			Artifact: in.Artifact, LocationID: in.LocationID, Version: in.Version,
			Progress: progressNotifier(ctx, req, logger),
		})
		return nil, out, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "account_status",
		Title:       "Account status",
		Description: "Membership, whether this membership may search, and remaining fast downloads in the current rolling window.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(true)},
		InputSchema: inputSchema[accountInput](timeoutLimit(600)),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in accountInput) (*mcp.CallToolResult, *aakb.Account, error) {
		ctx, cancel, err := adapter.WithTimeout(ctx, "account", in.TimeoutSeconds)
		if err != nil {
			return nil, nil, err
		}
		defer cancel()
		out, err := client.AccountStatus(ctx)
		return nil, out, err
	})

	server.AddPrompt(&mcp.Prompt{
		Name:        "find_best_edition",
		Title:       "Find the best edition",
		Description: "Search, compare editions by format and quality, then download the best copy.",
		Arguments: []*mcp.PromptArgument{
			{Name: "query", Description: "title, author, ISBN or DOI", Required: true},
			{Name: "format", Description: "preferred format such as epub"},
		},
	}, func(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		q := req.Params.Arguments["query"]
		format := req.Params.Arguments["format"]
		text := "Use aakb to find the best edition of " + q + ".\n" +
			"1. Call search with kind book, rank true and include_stats true" +
			ternary(format != "", ", format "+format, "") + ".\n" +
			"2. Call get_details on the top two hashes and compare identifiers, edition and problems.\n" +
			"3. Download the better hash and report its path and bytes.\n" +
			"Only proceed for material the user is entitled to obtain."
		return &mcp.GetPromptResult{
			Description: "Find the best edition",
			Messages: []*mcp.PromptMessage{
				{Role: "user", Content: &mcp.TextContent{Text: text}},
			},
		}, nil
	})

	return server
}

// progressNotifier reports transfer progress to a client that sent a
// progress token: once at the start, then at most once a second.
func progressNotifier(ctx context.Context, req *mcp.CallToolRequest, logger *slog.Logger) func(done, total int64) {
	token := req.Params.GetProgressToken()
	if token == nil || req.Session == nil {
		return nil
	}
	var last time.Time
	return func(done, total int64) {
		now := time.Now()
		if done != total && now.Sub(last) < time.Second {
			return
		}
		last = now
		err := req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
			ProgressToken: token, Progress: float64(done), Total: float64(total), Message: "downloading",
		})
		if err != nil {
			logger.DebugContext(ctx, "progress notification not sent", "error", err)
		}
	}
}

func ternary(ok bool, a, b string) string {
	if ok {
		return a
	}
	return b
}
