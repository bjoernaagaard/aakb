// Package mcpserver registers aakb's tools, prompt and guide on an MCP server.
package mcpserver

import (
	"context"
	"log/slog"

	"github.com/SokolskyNikita/annas-mcp/anna"
	"github.com/SokolskyNikita/annas-mcp/papers"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bjoernaagaard/aakb/internal/service"
)

// Version is the aakb release version reported to MCP clients.
// Release builds inject it with -ldflags; it defaults to a dev value.
var Version = "dev"

// New returns an MCP server backed by svc. Logs go to the caller's
// logger, which must write to stderr so stdio stdout stays clean.
func New(svc *service.Service, logger *slog.Logger) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "aakb", Version: Version}, &mcp.ServerOptions{
		Instructions: "Search Anna's Archive for books, papers and catalog records, " +
			"inspect one record in full, and download a verified file. " +
			"Start with search, then get_details on a chosen result, then download by its hash or DOI. Use article_read for OpenAlex full text when configured. " +
			"Use only for material the user is entitled to obtain.",
		Logger: logger,
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "search",
		Title:       "Search Anna's Archive",
		Description: "Search books, papers or catalog records with one tool. kind is book, article or metadata; empty auto-detects a DOI query as an article. Exact results are deduped and optionally ranked; looser suggestions come back separately in partial_matches.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(true)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in service.SearchInput) (*mcp.CallToolResult, *service.SearchOutput, error) {
		out, err := svc.Search(ctx, in)
		return nil, out, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_details",
		Title:       "Get record details",
		Description: "Full record for one hash, /md5/ URL, DOI or catalog id: identifiers, edition, description, subjects, reported file problems and a better copy's hash when the archive knows one.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(true)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in service.DetailsInput) (*mcp.CallToolResult, *service.DetailsOutput, error) {
		d, err := svc.Details(ctx, in)
		return nil, d, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "article_read",
		Title:       "Read a paper's full text",
		Description: "Bounded passages of OpenAlex structured full text with provenance and a continuation token. Needs OPENALEX_API_KEY. Abstract-only results say so instead of pretending to be full text.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(true)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in service.ReadInput) (*mcp.CallToolResult, *papers.ReadResult, error) {
		out, err := svc.Read(ctx, in)
		return nil, out, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "download",
		Title:       "Download a verified file",
		Description: "Download one file by MD5 hash or by paper DOI. The file is checked against its MD5, a matching local file is reused without a new request, and an existing file is never overwritten.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: new(false), IdempotentHint: true, OpenWorldHint: new(true)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in service.DownloadInput) (*mcp.CallToolResult, *service.DownloadOutput, error) {
		out, err := svc.Download(ctx, in)
		return nil, out, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "account_status",
		Title:       "Account status",
		Description: "Membership, whether this membership may search, and remaining fast downloads in the current rolling window.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(true)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in service.AccountInput) (*mcp.CallToolResult, *anna.AccountStatus, error) {
		st, err := svc.Account(ctx, in)
		return nil, st, err
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

func ternary(ok bool, a, b string) string {
	if ok {
		return a
	}
	return b
}
