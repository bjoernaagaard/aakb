package aakb

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bjoernaagaard/aakb/internal/archive"
	"github.com/bjoernaagaard/aakb/internal/doimirror"
	"github.com/bjoernaagaard/aakb/internal/httpx"
	"github.com/bjoernaagaard/aakb/internal/openalex"
	"github.com/bjoernaagaard/aakb/internal/store"
	"github.com/bjoernaagaard/aakb/internal/unpaywall"
)

const (
	defaultTimeout         = 60 * time.Second
	defaultDownloadTimeout = 30 * time.Minute
)

// MirrorMode controls whether the archive may use alternate mirrors.
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

// Config configures a Client. Empty paths use ~/aakb-downloads and the aakb
// subdirectory of os.UserCacheDir. Explicit paths must be absolute.
// BaseURL is the preferred English-language archive origin. Mirrors supplies
// alternate origins. Without either setting, archive operations are disabled.
// AllowedTLDs restricts archive mirror hosts to those top-level domains.
// DOIMirrorBaseURL and DOIMirrors configure optional HTTPS origins for
// DOI-only PDF downloads, restricted by DOIMirrorAllowedTLDs when set.
// UnpaywallEmail sets the contact email for Unpaywall open-access PDF lookups;
// leave empty for the polite default or set to "off" to disable Unpaywall.
// A nil HTTPClient uses a hardened HTTPS transport. A nil Logger discards logs.
type Config struct {
	SecretKey            string `json:"-"`
	AccountCookie        string `json:"-"`
	BaseURL              string
	Mirrors              []string
	AllowedTLDs          []string
	DOIMirrorBaseURL     string
	DOIMirrors           []string
	DOIMirrorAllowedTLDs []string
	DOIMirrorCookie      string `json:"-"`
	UnpaywallEmail       string
	Mirror               MirrorMode
	DownloadDir          string
	CacheDir             string
	OpenAlexKey          string       `json:"-"`
	HTTPClient           *http.Client `json:"-"`
	UserAgent            string
	Logger               *slog.Logger `json:"-"`
}

// LogValue reports configuration without credential values or HTTP clients.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Bool("base_url_set", c.BaseURL != ""),
		slog.Int("mirrors_count", len(c.Mirrors)),
		slog.Int("allowed_tlds_count", len(c.AllowedTLDs)),
		slog.Bool("doi_mirror_base_url_set", c.DOIMirrorBaseURL != ""),
		slog.Int("doi_mirrors_count", len(c.DOIMirrors)),
		slog.Int("doi_mirror_allowed_tlds_count", len(c.DOIMirrorAllowedTLDs)),
		slog.Bool("doi_mirror_cookie_set", c.DOIMirrorCookie != ""),
		slog.Bool("unpaywall_email_set", c.UnpaywallEmail != ""),
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
	unpaywall   doiDownloadProvider
	doiMirror   doiDownloadProvider
	downloadDir string
}

// Capabilities reports optional features available on a Client.
type Capabilities struct {
	OpenAlex  bool
	DOIMirror bool
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
		cfg.DownloadDir = filepath.Join(home, "aakb-downloads")
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

	userAgent := strings.TrimSpace(cfg.UserAgent)
	if userAgent == "" {
		userAgent = "aakb/" + openalex.Version
	}
	storeHTTP := httpx.NewClient(httpx.ClientOptions{
		BaseClient: cfg.HTTPClient,
		UserAgent:  userAgent,
		Secrets:    []string{cfg.SecretKey, cfg.AccountCookie, cfg.OpenAlexKey, cfg.DOIMirrorCookie},
		PublicOnly: true,
	})
	files, err := store.New(store.Config{
		DownloadDir: cfg.DownloadDir,
		CacheDir:    cfg.CacheDir,
		HTTPClient:  storeHTTP,
	})
	if err != nil {
		return nil, classifyError(op, err)
	}

	archiveClient, err := archive.NewClient(archive.Config{
		BaseURL:       cfg.BaseURL,
		Mirrors:       cfg.Mirrors,
		AllowedTLDs:   cfg.AllowedTLDs,
		AutoBaseURL:   cfg.Mirror == MirrorFailover,
		SecretKey:     cfg.SecretKey,
		AccountCookie: cfg.AccountCookie,
		DownloadDir:   cfg.DownloadDir,
		CacheDir:      cfg.CacheDir,
		HTTPClient:    cfg.HTTPClient,
		UserAgent:     cfg.UserAgent,
		Logger:        cfg.Logger,
		Store:         files,
	})
	if err != nil {
		return nil, classifyError(op, err)
	}

	doiClient, err := doimirror.NewClient(doimirror.Config{
		BaseURL:     cfg.DOIMirrorBaseURL,
		Mirrors:     cfg.DOIMirrors,
		AllowedTLDs: cfg.DOIMirrorAllowedTLDs,
		AutoBaseURL: cfg.Mirror == MirrorFailover,
		Cookie:      cfg.DOIMirrorCookie,
		DownloadDir: cfg.DownloadDir,
		CacheDir:    cfg.CacheDir,
		HTTPClient:  cfg.HTTPClient,
		UserAgent:   cfg.UserAgent,
		Logger:      cfg.Logger,
		Store:       files,
	})
	if err != nil {
		return nil, classifyError(op, err)
	}

	unpaywallClient, err := unpaywall.NewClient(unpaywall.Config{
		Email:      cfg.UnpaywallEmail,
		HTTPClient: cfg.HTTPClient,
		UserAgent:  userAgent,
		Store:      files,
	})
	if err != nil {
		return nil, classifyError(op, err)
	}

	c := &Client{
		archive:     newArchiveProvider(archiveClient),
		downloadDir: cfg.DownloadDir,
	}
	if unpaywallClient.Enabled() {
		c.unpaywall = newUnpaywallProvider(unpaywallClient)
	}
	if doiClient.Configured() {
		c.doiMirror = newDOIMirrorProvider(doiClient)
	}
	if strings.TrimSpace(cfg.OpenAlexKey) != "" {
		c.papers = newOpenAlexProvider(openalex.NewClient(openalex.Config{
			APIKey:     cfg.OpenAlexKey,
			HTTPClient: cfg.HTTPClient,
			UserAgent:  cfg.UserAgent,
		}), files, c.archive, c.unpaywall, c.doiMirror)
	}
	return c, nil
}

func absolutePath(path string) bool {
	return filepath.IsAbs(path) && !strings.ContainsRune(path, 0)
}

// Capabilities reports which optional features are configured.
func (c *Client) Capabilities() Capabilities {
	return Capabilities{OpenAlex: c.papers != nil, DOIMirror: c.doiMirror != nil}
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
