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
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
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
		fmt.Println("aakb", version())
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
		return serveHTTP(ctx, svc, cfg, logger, args)
	case "search":
		return cliSearch(ctx, svc, args)
	case "details":
		return cliDetails(ctx, svc, args)
	case "download":
		return cliDownload(ctx, svc, args)
	case "read":
		return cliRead(ctx, svc, args)
	case "account":
		out, err := svc.Account(ctx, service.AccountInput{})
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

type rateLimiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

func (r *rateLimiter) allow(ip string) bool {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	var kept []time.Time
	for _, at := range r.hits[ip] {
		if now.Sub(at) < time.Minute {
			kept = append(kept, at)
		}
	}
	if len(kept) >= 120 {
		r.hits[ip] = kept
		return false
	}
	r.hits[ip] = append(kept, now)
	return true
}

func serveHTTP(ctx context.Context, svc *service.Service, cfg config.Config, logger *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8080", "listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return mcpserver.New(svc, logger)
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, Logger: logger})

	limiter := &rateLimiter{hits: make(map[string][]time.Time)}
	auth := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				ip = r.RemoteAddr
			}
			if !limiter.allow(ip) {
				http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
				return
			}
			if cfg.HTTPToken != "" && r.Header.Get("Authorization") != "Bearer "+cfg.HTTPToken {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}

	mux := http.NewServeMux()
	mux.Handle("/mcp", auth(handler))
	mux.Handle("GET /downloads/{name}", auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := filepath.Base(r.PathValue("name"))
		http.ServeFile(w, r, filepath.Join(svc.DownloadPath, name))
	})))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("upstream") == "1" {
			probeCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			defer cancel()
			status, code := svc.UpstreamReachable(probeCtx)
			_, _ = fmt.Fprintf(w, `{"ok":true,"upstream":{"status":%q,"http_status":%d}}`, status, code) //nolint:errcheck // health response
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`)) //nolint:errcheck // health response
	})
	srv := &http.Server{
		Addr:              *addr,
		Handler:           http.NewCrossOriginProtection().Handler(mux),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      40 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	done := make(chan struct{})
	go func() { //nolint:gosec // shutdown waits on the parent ctx, then uses a fresh deadline
		defer close(done)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx) //nolint:errcheck,contextcheck // parent ctx is already cancelled; shutdown needs a fresh deadline
	}()
	logger.InfoContext(ctx, "aakb serving MCP over HTTP", "addr", *addr, "path", "/mcp", "auth", cfg.HTTPToken != "")
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve http: %w", err)
	}
	<-done
	return nil
}

func cliSearch(ctx context.Context, svc *service.Service, args []string) error {
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
	out, err := svc.Search(ctx, service.SearchInput{
		Kind: *kind, Provider: *provider, WorkType: *workType,
		Query: strings.Join(fs.Args(), " "), Title: *title, Author: *author,
		Publisher: *publisher, Year: *year, Format: *format, Language: *language,
		Source: *source, Content: *content, Sort: *sortBy, Page: *page,
		Limit: *limit, TimeoutSeconds: *timeout, Rank: *rank, IncludeStats: *stats,
		DownloadableOnly: *downloadable, SearchDescriptions: *descriptions,
	})
	return printJSON(out, err)
}

func cliDetails(ctx context.Context, svc *service.Service, args []string) error {
	fs := flag.NewFlagSet("details", flag.ContinueOnError)
	timeout := fs.Int("timeout", 0, "timeout in seconds")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: aakb details [-timeout 60] <hash|doi|catalog-id>")
	}
	out, err := svc.Details(ctx, service.DetailsInput{ID: fs.Arg(0), TimeoutSeconds: *timeout})
	return printJSON(out, err)
}

func cliRead(ctx context.Context, svc *service.Service, args []string) error {
	fs := flag.NewFlagSet("read", flag.ContinueOnError)
	continuation := fs.String("continuation", "", "continuation token")
	limit := fs.Int("limit", 0, "max passages")
	timeout := fs.Int("timeout", 0, "timeout in seconds")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: aakb read [-continuation t] <openalex-id|doi>")
	}
	out, err := svc.Read(ctx, service.ReadInput{ID: fs.Arg(0), Continuation: *continuation, Limit: *limit, TimeoutSeconds: *timeout})
	return printJSON(out, err)
}

func cliDownload(ctx context.Context, svc *service.Service, args []string) error {
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
	out, err := svc.Download(ctx, service.DownloadInput{Hash: *hash, DOI: *doi, OpenAlexID: *openalex, Title: *title, Format: *format, TimeoutSeconds: *timeout})
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
  aakb serve [-addr 127.0.0.1:8080]   serve MCP over Streamable HTTP at /mcp, health at /healthz
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
