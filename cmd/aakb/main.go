// Command aakb is a research MCP server and CLI in one binary.
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
	"io"
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
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		if failure, ok := errors.AsType[*jsonError](err); ok {
			printJSONError(failure.err)
		} else {
			fmt.Fprintln(os.Stderr, "aakb:", err)
		}
		os.Exit(1)
	}
}

// jsonError marks an error that the caller asked to read as JSON.
type jsonError struct{ err error }

func (f *jsonError) Error() string { return f.err.Error() }
func (f *jsonError) Unwrap() error { return f.err }

// common holds the flags every one-shot subcommand accepts.
type common struct {
	timeout    *int
	jsonErrors *bool
}

func commonFlags(fs *flag.FlagSet) common {
	return common{
		timeout:    fs.Int("timeout", 0, "timeout in seconds, at most 600, or 7200 for download"),
		jsonErrors: fs.Bool("json-errors", false, "print a failure on stderr as one JSON object"),
	}
}

func (c common) parse(fs *flag.FlagSet, args []string) error {
	fs.SetOutput(io.Discard)
	err := fs.Parse(args)
	fs.SetOutput(os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		fs.Usage()
		return err
	}
	if err != nil {
		return c.invalidArgument(fs, err.Error())
	}
	return nil
}

func (c common) invalidArgument(fs *flag.FlagSet, message string) error {
	return c.report(nil, &aakb.Error{Code: aakb.CodeInvalidArgument, Op: fs.Name(), Message: message})
}

// report prints out, or returns err in the form the flags asked for.
func (c common) report(out any, err error) error {
	if err = printJSON(out, err); err != nil && *c.jsonErrors {
		return &jsonError{err: err}
	}
	return err
}

func printJSONError(err error) {
	failure := struct {
		Code    string `json:"code"`
		Op      string `json:"op,omitempty"`
		Message string `json:"message"`
	}{Code: string(aakb.CodeInternal), Message: err.Error()}
	if libErr, ok := errors.AsType[*aakb.Error](err); ok {
		failure.Code, failure.Op, failure.Message = string(libErr.Code), libErr.Op, libErr.Message
	}
	data, marshalErr := json.Marshal(failure)
	if marshalErr != nil {
		fmt.Fprintln(os.Stderr, "aakb:", err)
		return
	}
	fmt.Fprintln(os.Stderr, string(data))
}

func run() error {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	args := os.Args[1:]
	cmd := "mcp"
	if len(args) > 0 {
		cmd = args[0]
		args = args[1:]
	}

	switch cmd {
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	case "version", "--version":
		fmt.Println(strings.TrimSpace("aakb " + version() + " " + buildDetail()))
		return nil
	case "health":
		return cliHealth(ctx, args)
	case "mcp":
		client, _, err := loadClient(logger)
		if err != nil {
			return err
		}
		server := mcpserver.New(client, logger)
		logger.Info("aakb serving MCP over stdio", "version", mcpserver.Version)
		if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("serve stdio: %w", err)
		}
		return nil
	case "serve":
		return serveHTTP(ctx, logger, args)
	case "search":
		return cliSearch(ctx, logger, args)
	case "details":
		return cliDetails(ctx, logger, args)
	case "download":
		return cliDownload(ctx, logger, args)
	case "read":
		return cliRead(ctx, logger, args)
	case "account":
		return cliAccount(ctx, logger, args)
	default:
		return fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
	}
}

func loadClient(logger *slog.Logger) (*aakb.Client, config.Config, error) {
	cfg, err := config.FromEnv()
	if err != nil {
		return nil, config.Config{}, &aakb.Error{Code: aakb.CodeConfig, Op: "config", Message: err.Error()}
	}
	clientConfig := cfg.Client()
	clientConfig.Logger = logger
	client, err := aakb.New(clientConfig)
	return client, cfg, err
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

// buildDetail reports the commit and commit time stamped into the binary.
func buildDetail() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	var revision, when string
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value[:min(len(setting.Value), 12)]
		case "vcs.time":
			when = setting.Value
		}
	}
	if revision == "" {
		return ""
	}
	return strings.TrimSpace("commit " + revision + " " + when)
}

func cliAccount(ctx context.Context, logger *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("account", flag.ContinueOnError)
	flags := commonFlags(fs)
	if err := flags.parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return flags.invalidArgument(fs, "unexpected positional arguments")
	}
	client, _, err := loadClient(logger)
	if err != nil {
		return flags.report(nil, err)
	}
	ctx, cancel, err := adapter.WithTimeout(ctx, "account", *flags.timeout)
	if err != nil {
		return flags.report(nil, err)
	}
	defer cancel()
	out, err := client.AccountStatus(ctx)
	return flags.report(out, err)
}

func cliSearch(ctx context.Context, logger *slog.Logger, args []string) error {
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
	authorID := fs.String("author-id", "", "OpenAlex author id")
	institutionID := fs.String("institution-id", "", "OpenAlex institution id")
	repositoryID := fs.String("repository-id", "", "OpenAlex repository id")
	relation := fs.String("relation", "", "citing, references or related, with -work-id")
	workID := fs.String("work-id", "", "OpenAlex work id for -relation")
	flags := commonFlags(fs)
	rank := fs.Bool("rank", true, "rank exact results")
	stats := fs.Bool("stats", false, "include download and quality counts")
	downloadable := fs.Bool("downloadable-only", false, "only downloadable results")
	descriptions := fs.Bool("search-descriptions", false, "search descriptions too")
	if err := flags.parse(fs, args); err != nil {
		return err
	}
	client, _, err := loadClient(logger)
	if err != nil {
		return flags.report(nil, err)
	}
	ctx, cancel, err := adapter.WithTimeout(ctx, "search", *flags.timeout)
	if err != nil {
		return flags.report(nil, err)
	}
	defer cancel()
	out, err := client.Search(ctx, aakb.SearchRequest{
		Kind: adapter.SearchKind(*kind, *provider), WorkType: *workType,
		Query: strings.Join(fs.Args(), " "), Title: *title, Author: *author,
		Publisher: *publisher, Year: *year, Formats: adapter.SplitList(*format), Languages: adapter.SplitList(*language),
		Sources: adapter.SplitList(*source), Content: adapter.SplitList(*content), Sort: aakb.Sort(*sortBy), Page: *page,
		Limit: *limit, Rank: *rank, IncludeStats: *stats,
		DownloadableOnly: *downloadable, SearchDescriptions: *descriptions,
		OpenAlex: aakb.OpenAlexFilter{
			AuthorID: *authorID, InstitutionID: *institutionID, RepositoryID: *repositoryID,
			Relation: *relation, WorkID: *workID,
		},
	})
	return flags.report(out, err)
}

func cliDetails(ctx context.Context, logger *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("details", flag.ContinueOnError)
	flags := commonFlags(fs)
	if err := flags.parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return flags.invalidArgument(fs, "expected one hash, DOI, catalog ID or OpenAlex ID")
	}
	client, _, err := loadClient(logger)
	if err != nil {
		return flags.report(nil, err)
	}
	ctx, cancel, err := adapter.WithTimeout(ctx, "details", *flags.timeout)
	if err != nil {
		return flags.report(nil, err)
	}
	defer cancel()
	out, err := client.Details(ctx, fs.Arg(0))
	return flags.report(out, err)
}

func cliRead(ctx context.Context, logger *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("read", flag.ContinueOnError)
	continuation := fs.String("continuation", "", "continuation token")
	limit := fs.Int("limit", 0, "character budget")
	flags := commonFlags(fs)
	if err := flags.parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return flags.invalidArgument(fs, "expected one OpenAlex ID or DOI")
	}
	client, _, err := loadClient(logger)
	if err != nil {
		return flags.report(nil, err)
	}
	ctx, cancel, err := adapter.WithTimeout(ctx, "read", *flags.timeout)
	if err != nil {
		return flags.report(nil, err)
	}
	defer cancel()
	req := aakb.ReadRequest{ID: fs.Arg(0), Continuation: *continuation, Limit: *limit}
	out, err := client.ReadArticle(ctx, req)
	if err != nil {
		return flags.report(nil, err)
	}
	return flags.report(adapter.ArticleResponse(req, out), nil)
}

func cliDownload(ctx context.Context, logger *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("download", flag.ContinueOnError)
	hash := fs.String("hash", "", "MD5 hash or /md5/ URL")
	doi := fs.String("doi", "", "paper DOI")
	openalex := fs.String("openalex-id", "", "OpenAlex work id")
	title := fs.String("title", "", "filename title")
	format := fs.String("format", "", "file extension")
	artifact := fs.String("artifact", "", "pdf or tei_xml, with -openalex-id")
	location := fs.String("location-id", "", "OpenAlex location id, with -openalex-id")
	fileVersion := fs.String("version", "", "published, accepted or submitted, with -openalex-id")
	quiet := fs.Bool("quiet", false, "print no progress lines on stderr")
	flags := commonFlags(fs)
	if err := flags.parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return flags.invalidArgument(fs, "unexpected positional arguments")
	}
	client, _, err := loadClient(logger)
	if err != nil {
		return flags.report(nil, err)
	}
	ctx, cancel, err := adapter.WithTimeout(ctx, "download", *flags.timeout)
	if err != nil {
		return flags.report(nil, err)
	}
	defer cancel()
	req := aakb.DownloadRequest{
		Hash: *hash, DOI: *doi, OpenAlexID: *openalex, Title: *title, Format: *format,
		Artifact: *artifact, LocationID: *location, Version: *fileVersion,
	}
	if !*quiet {
		req.Progress = stderrProgress()
	}
	out, err := client.Download(ctx, req)
	return flags.report(out, err)
}

// stderrProgress prints one line when the transfer starts, one for each
// further tenth of the file or 8 MiB when the size is unknown, and one at
// the end.
func stderrProgress() func(done, total int64) {
	const step = 8 << 20
	last := int64(-1)
	return func(done, total int64) {
		var mark int64
		if total > 0 {
			mark = done * 10 / total
		} else {
			mark = done / step
		}
		if mark == last && done != total {
			return
		}
		last = mark
		if total > 0 {
			fmt.Fprintf(os.Stderr, "aakb: downloaded %d of %d bytes\n", done, total)
			return
		}
		fmt.Fprintf(os.Stderr, "aakb: downloaded %d bytes\n", done)
	}
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

const usage = `aakb — Research MCP server and CLI

Usage:
  aakb                  serve MCP over stdio (default)
  aakb mcp              serve MCP over stdio
  aakb serve [-addr 127.0.0.1:8080]   serve the web UI at /, REST at /api/v1, MCP at /mcp and health at /healthz
  aakb search [-kind book|article|metadata|openalex] [field flags] [-limit 10] [query]
  aakb details <hash|doi|catalog-id|openalex-id>
  aakb read <openalex-id|doi>
  aakb download -hash <md5> | -doi <doi> | -openalex-id <id> [-title t] [-format epub]
  aakb account
  aakb health [-addr 127.0.0.1:8080]   exit 0 when a running aakb serve answers /healthz
  aakb version

The search, details, read, download and account commands take -timeout <seconds>
and -json-errors. Put -json-errors first to get JSON for flag errors too.
Run a subcommand that takes flags with -h to list them.

Environment:
  ANNAS_SECRET_KEY      archive account secret key (required for search and download)
  ANNAS_DOWNLOAD_PATH   download folder (default ~/annas-archive)
  ANNAS_BASE_URL        preferred mirror (default https://annas-archive.gl)
  ANNAS_MIRRORS         comma-separated extra mirror hosts for failover (use trusted hosts)
  ANNAS_AUTO_BASE_URL   mirror failover, true or false (default true)
  ANNAS_ACCOUNT_COOKIE  optional aa_account_id2 cookie
  OPENALEX_API_KEY      enables article_read and OpenAlex search/download
  ANNAS_ALLOWED_TLDS    optional comma-separated top-level domains archive mirrors may use
  AAKB_HTTP_TOKEN       bearer token required by aakb serve when set
  AAKB_TRUSTED_PROXIES  proxy addresses or CIDR networks whose X-Forwarded-For is believed
  AAKB_CACHE_DIR        session and mirror cache (default ~/.cache/aakb)

Use only for material you are entitled to obtain.
`
