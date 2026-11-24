// Command aakb is an Anna's Archive MCP server and CLI in one binary.
// With no arguments it serves MCP over stdio. Subcommands: serve, search,
// details, download and account. Use only for material you are entitled
// to obtain.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bjoernaagaard/aakb/internal/config"
	"github.com/bjoernaagaard/aakb/internal/mcpserver"
	"github.com/bjoernaagaard/aakb/internal/service"
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
		fmt.Println("aakb", mcpserver.Version)
		return nil
	}

	svc, err := service.New(cfg, logger)
	if err != nil {
		return err
	}

	switch cmd {
	case "mcp":
		server := mcpserver.New(svc, logger)
		logger.Info("aakb serving MCP over stdio", "version", mcpserver.Version)
		if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("serve stdio: %w", err)
		}
		return nil
	case "serve":
		return serveHTTP(ctx, svc, logger, args)
	case "search":
		return cliSearch(ctx, svc, args)
	case "details":
		if len(args) != 1 {
			return fmt.Errorf("usage: aakb details <hash|doi|catalog-id>")
		}
		out, err := svc.Details(ctx, service.DetailsInput{ID: args[0]})
		return printJSON(out, err)
	case "download":
		return cliDownload(ctx, svc, args)
	case "account":
		out, err := svc.Account(ctx, service.AccountInput{})
		return printJSON(out, err)
	default:
		return fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
	}
}

func serveHTTP(ctx context.Context, svc *service.Service, logger *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", ":8080", "listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return mcpserver.New(svc, logger)
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, Logger: logger})

	mux := http.NewServeMux()
	mux.Handle("/mcp", handler)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	logger.Info("aakb serving MCP over HTTP", "addr", *addr, "path", "/mcp")
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve http: %w", err)
	}
	return nil
}

func cliSearch(ctx context.Context, svc *service.Service, args []string) error {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	kind := fs.String("kind", "", "book, article or metadata")
	format := fs.String("format", "", "comma-separated formats")
	language := fs.String("language", "", "comma-separated language codes")
	limit := fs.Int("limit", 10, "max results")
	rank := fs.Bool("rank", true, "rank exact results")
	stats := fs.Bool("stats", false, "include download and quality counts")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: aakb search [flags] <query>")
	}
	out, err := svc.Search(ctx, service.SearchInput{
		Kind: *kind, Query: strings.Join(fs.Args(), " "),
		Format: *format, Language: *language, Limit: *limit, Rank: *rank, IncludeStats: *stats,
	})
	return printJSON(out, err)
}

func cliDownload(ctx context.Context, svc *service.Service, args []string) error {
	fs := flag.NewFlagSet("download", flag.ContinueOnError)
	hash := fs.String("hash", "", "MD5 hash or /md5/ URL")
	doi := fs.String("doi", "", "paper DOI")
	title := fs.String("title", "", "filename title")
	format := fs.String("format", "", "file extension")
	if err := fs.Parse(args); err != nil {
		return err
	}
	out, err := svc.Download(ctx, service.DownloadInput{Hash: *hash, DOI: *doi, Title: *title, Format: *format})
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
  aakb serve [-addr :8080]   serve MCP over Streamable HTTP at /mcp, health at /healthz
  aakb search [-kind book|article|metadata] [-format epub,pdf] [-limit 10] <query>
  aakb details <hash|doi|catalog-id>
  aakb download -hash <md5> | -doi <doi> [-title t] [-format epub]
  aakb account
  aakb version

Environment:
  ANNAS_SECRET_KEY      Anna's Archive account secret key (required for search and download)
  ANNAS_DOWNLOAD_PATH   download folder (default ~/annas-archive)
  ANNAS_BASE_URL        preferred mirror (default https://annas-archive.gl)
  ANNAS_AUTO_BASE_URL   mirror failover, true or false (default true)
  ANNAS_ACCOUNT_COOKIE  optional aa_account_id2 cookie
  AAKB_CACHE_DIR        session and mirror cache (default ~/.cache/aakb)

Use only for material you are entitled to obtain.
`
