package aakb

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/SokolskyNikita/annas-mcp/anna"
	"github.com/SokolskyNikita/annas-mcp/papers"
)

const (
	defaultTimeout         = 60 * time.Second
	defaultDownloadTimeout = 30 * time.Minute
)

// MirrorMode controls whether Anna's Archive may use alternate mirrors.
type MirrorMode int

const (
	// MirrorFailover uses alternate mirrors when the preferred one is unavailable.
	MirrorFailover MirrorMode = iota
	// MirrorFixed uses only the configured mirror.
	MirrorFixed
)

// String returns the name of the mirror mode.
func (m MirrorMode) String() string {
	switch m {
	case MirrorFailover:
		return "failover"
	case MirrorFixed:
		return "fixed"
	default:
		return "unknown"
	}
}

// Config configures a Client. Empty paths use ~/annas-archive and the aakb
// subdirectory of os.UserCacheDir. Explicit paths must be absolute.
// BaseURL defaults to https://annas-archive.gl. A nil Logger discards logs.
type Config struct {
	SecretKey     string `json:"-"`
	AccountCookie string `json:"-"`
	BaseURL       string
	Mirror        MirrorMode
	DownloadDir   string
	CacheDir      string
	OpenAlexKey   string       `json:"-"`
	Logger        *slog.Logger `json:"-"`
}

// LogValue reports configuration without credential values.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Bool("base_url_set", c.BaseURL != ""),
		slog.String("mirror", c.Mirror.String()),
		slog.Bool("secret_key_set", c.SecretKey != ""),
		slog.Bool("account_cookie_set", c.AccountCookie != ""),
		slog.Bool("openalex_key_set", c.OpenAlexKey != ""),
	)
}

// Client performs operations with one configuration and is safe for concurrent use.
// Use New to construct it.
type Client struct {
	archive     *anna.Client
	papers      *papers.Client
	downloadDir string
}

// Capabilities reports optional features available on a Client.
type Capabilities struct {
	OpenAlex bool
}

// New builds a client without network access. It creates the cache directory
// and can read local session state. Operations check credentials when needed.
func New(cfg Config) (*Client, error) {
	const op = "new"
	if cfg.Mirror != MirrorFailover && cfg.Mirror != MirrorFixed {
		return nil, newError(op, CodeConfig, "unsupported mirror mode")
	}
	if cfg.DownloadDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, newError(op, CodeConfig, "cannot determine the home directory")
		}
		cfg.DownloadDir = filepath.Join(home, "annas-archive")
	}
	if cfg.CacheDir == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return nil, newError(op, CodeConfig, "cannot determine the user cache directory")
		}
		cfg.CacheDir = filepath.Join(cache, "aakb")
	}
	if !absolutePath(cfg.DownloadDir) {
		return nil, newError(op, CodeConfig, "DownloadDir must be an absolute path")
	}
	if !absolutePath(cfg.CacheDir) {
		return nil, newError(op, CodeConfig, "CacheDir must be an absolute path")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if err := os.MkdirAll(cfg.CacheDir, 0o700); err != nil {
		return nil, newError(op, CodeInternal, "cannot create the cache directory")
	}
	archive, err := anna.NewClient(anna.Config{
		BaseURL: cfg.BaseURL, SecretKey: cfg.SecretKey, AccountCookie: cfg.AccountCookie,
		DownloadPath: cfg.DownloadDir, SessionFile: filepath.Join(cfg.CacheDir, "session.json"),
		AutoBaseURL: cfg.Mirror == MirrorFailover, Logger: cfg.Logger,
	})
	if err != nil {
		return nil, newError(op, CodeConfig, "invalid Anna's Archive configuration")
	}
	c := &Client{archive: archive, downloadDir: cfg.DownloadDir}
	if strings.TrimSpace(cfg.OpenAlexKey) != "" {
		c.papers, err = papers.NewClient(papers.Config{
			OpenAlexAPIKey: cfg.OpenAlexKey, DownloadPath: cfg.DownloadDir,
			CacheDir: cfg.CacheDir, Archive: archive,
		})
		if err != nil {
			return nil, newError(op, CodeConfig, "invalid OpenAlex configuration")
		}
	}
	return c, nil
}

func absolutePath(path string) bool {
	return filepath.IsAbs(path) && !strings.ContainsRune(path, 0)
}

// Capabilities reports which optional features are configured.
func (c *Client) Capabilities() Capabilities {
	return Capabilities{OpenAlex: c.papers != nil}
}

// DownloadDir returns the absolute directory where downloads are saved.
func (c *Client) DownloadDir() string {
	return c.downloadDir
}

func withDefaultTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}
