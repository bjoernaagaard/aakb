// Package doimirror downloads PDFs by DOI from explicitly configured HTTPS
// mirror origins with TLD enforcement, backoff failover, challenge detection,
// and SSRF-safe streaming into the shared download store.
package doimirror

import (
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/bjoernaagaard/aakb/internal/httpx"
	"github.com/bjoernaagaard/aakb/internal/store"
)

const (
	defaultBrowserUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

	privateDirMode  os.FileMode = 0o700
	privateFileMode os.FileMode = 0o600
)

var diagnosticURLPattern = regexp.MustCompile(`https?://[^\s<>"']+`)

// Config configures a DOI mirror client. Without BaseURL or Mirrors, the
// client is inactive and reports Configured() == false.
type Config struct {
	BaseURL     string
	Mirrors     []string
	AllowedTLDs []string
	AutoBaseURL bool
	Cookie      string `json:"-"`
	DownloadDir string
	CacheDir    string
	HTTPClient  *http.Client `json:"-"`
	UserAgent   string
	Logger      *slog.Logger `json:"-"`
	Store       *store.Store
}

// Client downloads PDFs by DOI from configured mirrors using internal/httpx
// and internal/store. It is safe for concurrent use.
type Client struct {
	autoBaseURL bool
	baseURL     string
	cookie      string
	http        *httpx.Client
	store       *store.Store
	log         *slog.Logger

	health   *mirrorHealth
	selector *mirrorSelector
}

// NewClient validates configuration and builds a Client without touching the network.
func NewClient(cfg Config) (*Client, error) {
	for _, setting := range []struct{ field, path string }{
		{"DownloadDir", cfg.DownloadDir},
		{"CacheDir", cfg.CacheDir},
	} {
		if setting.path != "" && (!filepath.IsAbs(setting.path) || strings.ContainsRune(setting.path, 0)) {
			return nil, httpx.NewError(httpx.CodeConfig, "invalid_path", setting.field+" must be an absolute path")
		}
	}

	cookie, err := normalizeCookie(cfg.Cookie)
	if err != nil {
		return nil, err
	}

	candidates, err := buildCandidates(cfg.BaseURL, cfg.Mirrors, cfg.AllowedTLDs, cfg.AutoBaseURL)
	if err != nil {
		return nil, err
	}
	var baseURL string
	if len(candidates) > 0 {
		baseURL = "https://" + candidates[0]
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	userAgent := strings.TrimSpace(cfg.UserAgent)
	if userAgent == "" {
		userAgent = defaultBrowserUserAgent
	}

	guardedHTTP := httpx.NewClient(httpx.ClientOptions{
		BaseClient: cfg.HTTPClient,
		UserAgent:  userAgent,
		Secrets:    []string{cookie},
		PublicOnly: true,
	})

	fileStore := cfg.Store
	if fileStore == nil && cfg.DownloadDir != "" && cfg.CacheDir != "" {
		fileStore, err = store.New(store.Config{
			DownloadDir: cfg.DownloadDir,
			CacheDir:    cfg.CacheDir,
			HTTPClient:  guardedHTTP,
		})
		if err != nil {
			return nil, err
		}
	}

	health := newMirrorHealth()
	if cfg.CacheDir != "" {
		attachMirrorStore(cfg.CacheDir, health, logger)
	}
	prober := newMirrorProber(guardedHTTP, cookie)

	return &Client{
		autoBaseURL: cfg.AutoBaseURL,
		baseURL:     baseURL,
		cookie:      cookie,
		http:        guardedHTTP,
		store:       fileStore,
		log:         logger,
		health:      health,
		selector:    newMirrorSelector(candidates, health, prober, logger),
	}, nil
}

// Configured reports whether at least one DOI mirror host is configured.
func (c *Client) Configured() bool {
	return c != nil && c.baseURL != ""
}

// Candidates returns the validated, TLD-enforced mirror hosts in preference order.
func (c *Client) Candidates() []string {
	if c == nil {
		return nil
	}
	return c.selector.Hosts()
}

func normalizeCookie(raw string) (string, error) {
	value := strings.Trim(strings.TrimSpace(raw), `"'`)
	if value == "" {
		return "", nil
	}
	if strings.IndexFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return "", httpx.NewError(httpx.CodeConfig, "invalid_cookie", "DOIMirrorCookie contains invalid cookie characters")
	}
	return value, nil
}

func requestHost(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Hostname() == "" {
		return "upstream server"
	}
	return parsed.Hostname()
}

func responseHost(resp *http.Response, rawURL string) string {
	if resp != nil && resp.Request != nil && resp.Request.URL != nil {
		rawURL = resp.Request.URL.String()
	}
	return requestHost(rawURL)
}
