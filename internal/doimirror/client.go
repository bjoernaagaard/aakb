// Package doimirror downloads PDFs by DOI from explicitly configured HTTPS
// mirror origins with TLD enforcement, backoff failover, challenge detection,
// and SSRF-safe streaming into the shared download store.
package doimirror

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/bjoernaagaard/aakb/internal/httpx"
	"github.com/bjoernaagaard/aakb/internal/mirror"
	"github.com/bjoernaagaard/aakb/internal/store"
)

const defaultBrowserUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

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

	health   *mirror.Health
	selector *mirror.Selector
}

// NewClient validates configuration and builds a Client without touching the network.
func NewClient(cfg Config) (*Client, error) {
	if err := store.ValidateDirs(cfg.DownloadDir, cfg.CacheDir); err != nil {
		return nil, err
	}

	cookie, err := normalizeCookie(cfg.Cookie)
	if err != nil {
		return nil, err
	}

	candidates, err := mirror.BuildCandidates(mirror.CandidateConfig{
		BaseURL:     cfg.BaseURL,
		Mirrors:     cfg.Mirrors,
		AllowedTLDs: cfg.AllowedTLDs,
		AutoBaseURL: cfg.AutoBaseURL,
		Origin:      doiOriginOptions,
	})
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

	fileStore, err := store.Ensure(cfg.Store, cfg.DownloadDir, cfg.CacheDir, guardedHTTP)
	if err != nil {
		return nil, err
	}

	health := mirror.NewHealth()
	if cfg.CacheDir != "" {
		health.AttachStore(mirror.StoreConfig{
			CacheDir:    cfg.CacheDir,
			Filename:    mirrorStateFilename,
			TempPattern: ".doi-mirrors-*.tmp",
			LogLabel:    "DOI mirror",
			Logger:      logger,
		})
	}
	prober := mirror.NewProber(mirror.ProberConfig{
		Client:       guardedHTTP,
		Path:         "/",
		MaxBodyBytes: maxProbeBodyBytes,
		Cookie:       func() string { return cookie },
		Validate:     validateDOIMirrorProbe,
		Label:        "DOI mirror",
	})

	return &Client{
		autoBaseURL: cfg.AutoBaseURL,
		baseURL:     baseURL,
		cookie:      cookie,
		http:        guardedHTTP,
		store:       fileStore,
		log:         logger,
		health:      health,
		selector: mirror.NewSelector(mirror.SelectorConfig{
			Hosts:              candidates,
			AutoBaseURL:        cfg.AutoBaseURL,
			Health:             health,
			Prober:             prober,
			ClassifyProbeError: classifyDOIMirrorProbeError,
			Label:              "DOI mirror",
			Logger:             logger,
		}),
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
