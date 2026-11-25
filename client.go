package aakb

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/SokolskyNikita/annas-mcp/anna"
	"github.com/SokolskyNikita/annas-mcp/papers"

	"github.com/bjoernaagaard/aakb/internal/httpx"
)

const (
	defaultTimeout         = 60 * time.Second
	defaultDownloadTimeout = 30 * time.Minute
	defaultBaseURL         = "https://annas-archive.gl"
)

var defaultMirrorHosts = []string{
	"annas-archive.gl",
	"annas-archive.pk",
	"annas-archive.gd",
}

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
// BaseURL defaults to https://annas-archive.gl. Mirrors lists additional archive
// mirror hosts tried during failover, and AllowedTLDs restricts archive mirror
// hosts to those top-level domains. A nil HTTPClient uses a hardened HTTPS
// transport. A nil Logger discards logs.
type Config struct {
	SecretKey     string `json:"-"`
	AccountCookie string `json:"-"`
	BaseURL       string
	Mirrors       []string
	AllowedTLDs   []string
	Mirror        MirrorMode
	DownloadDir   string
	CacheDir      string
	OpenAlexKey   string       `json:"-"`
	HTTPClient    *http.Client `json:"-"`
	UserAgent     string
	Logger        *slog.Logger `json:"-"`
}

// LogValue reports configuration without credential values or HTTP clients.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Bool("base_url_set", c.BaseURL != ""),
		slog.Int("mirrors_count", len(c.Mirrors)),
		slog.Int("allowed_tlds_count", len(c.AllowedTLDs)),
		slog.String("mirror", c.Mirror.String()),
		slog.Bool("secret_key_set", c.SecretKey != ""),
		slog.Bool("account_cookie_set", c.AccountCookie != ""),
		slog.Bool("openalex_key_set", c.OpenAlexKey != ""),
	)
}

// Client performs operations with one configuration and is safe for concurrent use.
// Use New to construct it.
type Client struct {
	archive     archiveProvider
	papers      papersProvider
	downloadDir string
}

// Capabilities reports optional features available on a Client.
type Capabilities struct {
	OpenAlex bool
}

// New builds a client without network access. It creates the cache directory
// and can read local session state. When AllowedTLDs is non-empty, New verifies
// that BaseURL, Mirrors, and at least one candidate mirror use an allowed TLD
// and returns CodeConfig otherwise. Operations check credentials when needed.
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
	effectiveBaseURL, err := validateMirrorPolicy(cfg.BaseURL, cfg.Mirrors, cfg.AllowedTLDs, cfg.Mirror)
	if err != nil {
		return nil, err
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if err := os.MkdirAll(cfg.CacheDir, 0o700); err != nil {
		return nil, newError(op, CodeInternal, "cannot create the cache directory")
	}
	archiveClient, err := anna.NewClient(anna.Config{
		BaseURL:       effectiveBaseURL,
		SecretKey:     cfg.SecretKey,
		AccountCookie: cfg.AccountCookie,
		DownloadPath:  cfg.DownloadDir,
		SessionFile:   filepath.Join(cfg.CacheDir, "session.json"),
		AutoBaseURL:   cfg.Mirror == MirrorFailover,
		HTTPClient:    cfg.HTTPClient,
		Logger:        cfg.Logger,
	})
	if err != nil {
		return nil, newError(op, CodeConfig, "invalid Anna's Archive configuration")
	}
	healthHTTP := httpx.NewClient(httpx.ClientOptions{
		BaseClient: cfg.HTTPClient,
		UserAgent:  cfg.UserAgent,
		Secrets:    []string{cfg.SecretKey, cfg.AccountCookie, cfg.OpenAlexKey},
	})
	c := &Client{
		archive:     newLegacyArchiveProvider(archiveClient, effectiveBaseURL, healthHTTP),
		downloadDir: cfg.DownloadDir,
	}
	if strings.TrimSpace(cfg.OpenAlexKey) != "" {
		papersClient, err := papers.NewClient(papers.Config{
			OpenAlexAPIKey:    cfg.OpenAlexKey,
			DownloadPath:      cfg.DownloadDir,
			CacheDir:          cfg.CacheDir,
			Archive:           archiveClient,
			HTTPClient:        cfg.HTTPClient,
			ContentHTTPClient: cfg.HTTPClient,
		})
		if err != nil {
			return nil, newError(op, CodeConfig, "invalid OpenAlex configuration")
		}
		c.papers = newLegacyPapersProvider(papersClient)
	}
	return c, nil
}

func validateMirrorPolicy(baseURL string, mirrors, allowedTLDs []string, mode MirrorMode) (string, error) {
	const op = "new"
	baseURL = strings.TrimSpace(baseURL)
	cleanedMirrors := cleanList(mirrors)
	allowed := normalizeTLDs(allowedTLDs)

	if len(allowed) > 0 {
		if baseURL != "" && !hostAllowedByTLDs(baseURL, allowed) {
			return "", newError(op, CodeConfig, "BaseURL top-level domain is not in AllowedTLDs")
		}
		for _, mirror := range cleanedMirrors {
			if !hostAllowedByTLDs(mirror, allowed) {
				return "", newError(op, CodeConfig, "Mirrors contains a top-level domain outside AllowedTLDs")
			}
		}
		if baseURL == "" && len(cleanedMirrors) == 0 {
			if mode != MirrorFixed {
				for _, candidate := range defaultMirrorHosts {
					if hostAllowedByTLDs(candidate, allowed) {
						return "https://" + candidate, nil
					}
				}
				return "", newError(op, CodeConfig, "no archive mirror matches AllowedTLDs")
			}
			if !hostAllowedByTLDs(defaultBaseURL, allowed) {
				return "", newError(op, CodeConfig, "default archive mirror top-level domain is not in AllowedTLDs")
			}
		}
	}

	if baseURL == "" && len(cleanedMirrors) > 0 {
		return cleanedMirrors[0], nil
	}
	return baseURL, nil
}

func cleanList(values []string) []string {
	var out []string
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func normalizeTLDs(tlds []string) []string {
	var out []string
	for _, tld := range tlds {
		cleaned := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(tld), "."))
		if cleaned != "" && !slices.Contains(out, cleaned) {
			out = append(out, cleaned)
		}
	}
	return out
}

func hostAllowedByTLDs(raw string, allowed []string) bool {
	host := strings.TrimSpace(raw)
	if strings.Contains(host, "://") {
		u, err := url.Parse(host)
		if err != nil || u.Hostname() == "" {
			return false
		}
		host = u.Hostname()
	} else {
		u, err := url.Parse("https://" + host)
		if err != nil || u.Hostname() == "" {
			return false
		}
		host = u.Hostname()
	}
	idx := strings.LastIndex(host, ".")
	if idx < 0 || idx+1 >= len(host) {
		return false
	}
	tld := strings.ToLower(host[idx+1:])
	return slices.Contains(allowed, tld)
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

// UpstreamHealth probes the configured archive mirror and reports its status.
func (c *Client) UpstreamHealth(ctx context.Context) UpstreamStatus {
	return c.archive.ProbeHealth(ctx)
}

func withDefaultTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}
