// Command aakb is an Anna's Archive MCP server and CLI in one binary.
// With no arguments it serves MCP over stdio. Subcommands: serve (web UI,
// REST API and MCP over HTTP), search, details, read, download and account.
// Use only for material you are entitled to obtain.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bjoernaagaard/aakb"
	"github.com/bjoernaagaard/aakb/internal/adapter"
	"github.com/bjoernaagaard/aakb/internal/config"
	"github.com/bjoernaagaard/aakb/internal/mcpserver"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "aakb:", err)
		os.Exit(1)
	}
}

func run() error {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	args := os.Args[1:]
	cmd := "mcp"
	if len(args) > 0 {
		cmd = args[0]
		args = args[1:]
	}

	// Help and version never build a client or touch the network.
	switch cmd {
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	case "version", "--version":
		fmt.Println("aakb", version())
		return nil
	}

	clientConfig := cfg.Client()
	clientConfig.Logger = logger
	client, err := aakb.New(clientConfig)
	if err != nil {
		return err
	}

	switch cmd {
	case "mcp":
		server := mcpserver.New(client, logger)
		logger.Info("aakb serving MCP over stdio", "version", mcpserver.Version)
		if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("serve stdio: %w", err)
		}
		return nil
	case "serve":
		return serveHTTP(ctx, client, cfg, logger, args)
	case "search":
		return cliSearch(ctx, client, args)
	case "details":
		return cliDetails(ctx, client, args)
	case "download":
		return cliDownload(ctx, client, args)
	case "read":
		return cliRead(ctx, client, args)
	case "account":
		out, err := client.AccountStatus(ctx)
		return printJSON(out, err)
	default:
		return fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
	}
}

func version() string {
	if mcpserver.Version != "dev" {
		return mcpserver.Version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

func cliSearch(ctx context.Context, client *aakb.Client, args []string) error {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	kind := fs.String("kind", "", "book, article, metadata or openalex")
	provider := fs.String("provider", "", "openalex")
	workType := fs.String("work-type", "", "OpenAlex work type")
	format := fs.String("format", "", "comma-separated formats")
	language := fs.String("language", "", "comma-separated language codes")
	source := fs.String("source", "", "comma-separated sources")
	content := fs.String("content", "", "comma-separated content types")
	sortBy := fs.String("sort", "", "sort order")
	title := fs.String("title", "", "title phrase")
	author := fs.String("author", "", "author phrase")
	publisher := fs.String("publisher", "", "publisher phrase")
	year := fs.String("year", "", "publication year")
	page := fs.Int("page", 1, "result page")
	limit := fs.Int("limit", 10, "max results")
	timeout := fs.Int("timeout", 0, "timeout in seconds")
	rank := fs.Bool("rank", true, "rank exact results")
	stats := fs.Bool("stats", false, "include download and quality counts")
	downloadable := fs.Bool("downloadable-only", false, "only downloadable results")
	descriptions := fs.Bool("search-descriptions", false, "search descriptions too")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel, err := adapter.WithTimeout(ctx, "search", *timeout)
	if err != nil {
		return err
	}
	defer cancel()
	out, err := client.Search(ctx, aakb.SearchRequest{
		Kind: adapter.SearchKind(*kind, *provider), WorkType: *workType,
		Query: strings.Join(fs.Args(), " "), Title: *title, Author: *author,
		Publisher: *publisher, Year: *year, Formats: adapter.SplitList(*format), Languages: adapter.SplitList(*language),
		Sources: adapter.SplitList(*source), Content: adapter.SplitList(*content), Sort: aakb.Sort(*sortBy), Page: *page,
		Limit: *limit, Rank: *rank, IncludeStats: *stats,
		DownloadableOnly: *downloadable, SearchDescriptions: *descriptions,
	})
	return printJSON(out, err)
}

func cliDetails(ctx context.Context, client *aakb.Client, args []string) error {
	fs := flag.NewFlagSet("details", flag.ContinueOnError)
	timeout := fs.Int("timeout", 0, "timeout in seconds")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: aakb details [-timeout 60] <hash|doi|catalog-id>")
	}
	ctx, cancel, err := adapter.WithTimeout(ctx, "details", *timeout)
	if err != nil {
		return err
	}
	defer cancel()
	out, err := client.Details(ctx, fs.Arg(0))
	return printJSON(out, err)
}

func cliRead(ctx context.Context, client *aakb.Client, args []string) error {
	fs := flag.NewFlagSet("read", flag.ContinueOnError)
	continuation := fs.String("continuation", "", "continuation token")
	limit := fs.Int("limit", 0, "character budget")
	timeout := fs.Int("timeout", 0, "timeout in seconds")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: aakb read [-continuation t] <openalex-id|doi>")
	}
	ctx, cancel, err := adapter.WithTimeout(ctx, "read", *timeout)
	if err != nil {
		return err
	}
	defer cancel()
	req := aakb.ReadRequest{ID: fs.Arg(0), Continuation: *continuation, Limit: *limit}
	out, err := client.ReadArticle(ctx, req)
	if err != nil {
		return err
	}
	return printJSON(adapter.ArticleResponse(req, out), nil)
}

func cliDownload(ctx context.Context, client *aakb.Client, args []string) error {
	fs := flag.NewFlagSet("download", flag.ContinueOnError)
	hash := fs.String("hash", "", "MD5 hash or /md5/ URL")
	doi := fs.String("doi", "", "paper DOI")
	openalex := fs.String("openalex-id", "", "OpenAlex work id")
	title := fs.String("title", "", "filename title")
	format := fs.String("format", "", "file extension")
	timeout := fs.Int("timeout", 0, "timeout in seconds")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel, err := adapter.WithTimeout(ctx, "download", *timeout)
	if err != nil {
		return err
	}
	defer cancel()
	out, err := client.Download(ctx, aakb.DownloadRequest{Hash: *hash, DOI: *doi, OpenAlexID: *openalex, Title: *title, Format: *format})
	return printJSON(out, err)
}

func printJSON(out any, err error) error {
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("encode output: %w", err)
	}
	return nil
}

const usage = `aakb — Anna's Archive MCP server and CLI

Usage:
  aakb                  serve MCP over stdio (default)
  aakb mcp              serve MCP over stdio
  aakb serve [-addr 127.0.0.1:8080]   serve the web UI at /, REST at /api/v1, MCP at /mcp and health at /healthz
  aakb search [-kind book|article|metadata|openalex] [field flags] [-limit 10] [query]
  aakb details <hash|doi|catalog-id>
  aakb read <openalex-id|doi>
  aakb download -hash <md5> | -doi <doi> | -openalex-id <id> [-title t] [-format epub]
  aakb account
  aakb version

Environment:
  ANNAS_SECRET_KEY      Anna's Archive account secret key (required for search and download)
  ANNAS_DOWNLOAD_PATH   download folder (default ~/annas-archive)
  ANNAS_BASE_URL        preferred mirror (default https://annas-archive.gl)
  ANNAS_AUTO_BASE_URL   mirror failover, true or false (default true)
  ANNAS_ACCOUNT_COOKIE  optional aa_account_id2 cookie
  OPENALEX_API_KEY      enables article_read and OpenAlex search/download
  AAKB_HTTP_TOKEN       bearer token required by aakb serve when set
  AAKB_CACHE_DIR        session and mirror cache (default ~/.cache/aakb)

Use only for material you are entitled to obtain.
`
