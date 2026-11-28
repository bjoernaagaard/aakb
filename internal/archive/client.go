// Package archive implements a first-party archive client built over
// internal/httpx and internal/store, with TLD-enforced mirror selection,
// singleflight session renewal, JSON record/stats endpoints, and fast downloads.
package archive

import (
	"context"
	"crypto/md5" //nolint:gosec // G501: MD5 is required to validate archive file hashes
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/bjoernaagaard/aakb/internal/httpx"
	"github.com/bjoernaagaard/aakb/internal/mirror"
	"github.com/bjoernaagaard/aakb/internal/store"
)

const (
	defaultBrowserUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

	defaultSignInTimeout = 15 * time.Second
)

var diagnosticURLPattern = regexp.MustCompile(`https?://[^\s<>"']+`)

// Config configures an archive client. Empty paths are allowed at
// construction time; operations that need persistence or downloads check them.
type Config struct {
	BaseURL       string
	Mirrors       []string
	AllowedTLDs   []string
	AutoBaseURL   bool
	SecretKey     string `json:"-"`
	AccountCookie string `json:"-"`
	DownloadDir   string
	CacheDir      string
	HTTPClient    *http.Client `json:"-"`
	UserAgent     string
	Logger        *slog.Logger `json:"-"`
	Store         *store.Store
}

// Client talks to archive mirrors using internal/httpx and internal/store.
// It is safe for concurrent use.
type Client struct {
	secretKey   string
	downloadDir string
	cacheDir    string
	autoBaseURL bool
	baseURL     string
	userAgent   string
	http        *httpx.Client
	store       *store.Store
	log         *slog.Logger

	health   *mirror.Health
	prober   *mirror.Prober
	selector *mirror.Selector

	sessionMu        sync.Mutex
	cookie           string
	configuredCookie string
	signingIn        chan struct{}
	signedInAt       time.Time
	signInErr        error
	signInTimeout    time.Duration

	fileStatsSlots   chan struct{}
	fileStatsCache   statsCache
	fileStatsTimeout time.Duration

	fastHealth *fastServerHealth
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

	cookie, err := NormalizeAccountCookie(cfg.AccountCookie)
	if err != nil {
		return nil, err
	}

	candidates, err := mirror.BuildCandidates(mirror.CandidateConfig{
		BaseURL:          cfg.BaseURL,
		Mirrors:          cfg.Mirrors,
		AllowedTLDs:      cfg.AllowedTLDs,
		AutoBaseURL:      cfg.AutoBaseURL,
		Origin:           archiveOriginOptions,
		BaseField:        "BaseURL",
		MirrorsField:     "Mirrors",
		AllowedTLDsField: "AllowedTLDs",
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

	secretKey := strings.TrimSpace(cfg.SecretKey)
	guardedHTTP := httpx.NewClient(httpx.ClientOptions{
		BaseClient: cfg.HTTPClient,
		UserAgent:  userAgent,
		Secrets:    []string{secretKey, cookie},
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

	c := &Client{
		secretKey:        secretKey,
		downloadDir:      cfg.DownloadDir,
		cacheDir:         cfg.CacheDir,
		autoBaseURL:      cfg.AutoBaseURL,
		baseURL:          baseURL,
		userAgent:        userAgent,
		http:             guardedHTTP,
		store:            fileStore,
		log:              logger,
		configuredCookie: cookie,
		cookie:           cookie,
		signInTimeout:    defaultSignInTimeout,
		fileStatsSlots:   make(chan struct{}, maxConcurrentFileStats),
		fileStatsCache:   newStatsCache(),
		fileStatsTimeout: fileStatsBudget,
		fastHealth:       newFastServerHealth(),
	}

	c.restoreSession()
	c.health = mirror.NewHealth()
	if c.cacheDir != "" {
		c.health.AttachStore(mirror.StoreConfig{
			CacheDir:    c.cacheDir,
			Filename:    mirrorStateFilename,
			TempPattern: ".mirrors-*.tmp",
			LogLabel:    "archive mirror",
			Logger:      c.log,
		})
	}
	c.prober = mirror.NewProber(mirror.ProberConfig{
		Client:       c.http,
		Path:         mirrorProbePath,
		MaxBodyBytes: maxProbeBodyBytes,
		Cookie:       c.sessionCookie,
		Validate:     validateArchiveProbe,
		Label:        "mirror",
	})
	c.selector = mirror.NewSelector(mirror.SelectorConfig{
		Hosts:              candidates,
		AutoBaseURL:        c.autoBaseURL,
		Health:             c.health,
		Prober:             c.prober,
		ClassifyProbeError: classifyArchiveProbeError,
		Label:              "archive mirror",
		Logger:             c.log,
	})
	return c, nil
}

// PrimaryOrigin returns the primary archive mirror origin selected by configuration.
func (c *Client) PrimaryOrigin() string {
	return c.baseURL
}

// Candidates returns the validated, TLD-enforced mirror hosts in preference order.
func (c *Client) Candidates() []string {
	return c.selector.Hosts()
}

// requireArchiveAccess enforces that either a session cookie or secret key is available.
func (c *Client) requireArchiveAccess(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.baseURL == "" {
		return errNoMirrorConfigured
	}
	if c.sessionCookie() == "" && c.secretKey == "" {
		return httpx.NewError(httpx.CodeConfig, "missing_credentials", "SecretKey or AccountCookie is required for archive access")
	}
	return nil
}

func buildPathURL(base, path string) string {
	base = strings.TrimSpace(base)
	if !strings.Contains(base, "://") {
		base = "https://" + strings.TrimSuffix(base, "/")
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return base + path
	}
	ref, err := url.Parse(path)
	if err != nil {
		return base + path
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/" + strings.TrimLeft(ref.Path, "/")
	u.RawPath = ""
	u.RawQuery = ref.RawQuery
	u.Fragment = ref.Fragment
	return u.String()
}

func normalizeHash(hash string) (string, error) {
	hash = strings.ToLower(strings.TrimSpace(hash))
	if len(hash) != md5.Size*2 {
		return "", httpx.NewError(httpx.CodeInvalidArgument, "invalid_hash", "book hash must be a 32-character MD5")
	}
	if _, err := hex.DecodeString(hash); err != nil {
		return "", httpx.NewError(httpx.CodeInvalidArgument, "invalid_hash", "book hash must be a hexadecimal MD5")
	}
	return hash, nil
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

func httpResponseError(resp *http.Response, rawURL string) *httpx.ProviderError {
	status := resp.StatusCode
	host := responseHost(resp, rawURL)
	message := strings.TrimSpace(fmt.Sprintf("HTTP status %d %s", status, http.StatusText(status))) + " from " + host
	code := httpx.CodeUpstream
	reason := "http_status"
	switch status {
	case http.StatusNotFound, http.StatusGone:
		code = httpx.CodeNotFound
		reason = "not_found"
		message += ": requested resource is unavailable on this server"
	case http.StatusUnauthorized, http.StatusForbidden:
		code = httpx.CodeUpstreamBlocked
		reason = "access_denied"
		message += ": access denied"
	case http.StatusTooManyRequests:
		code = httpx.CodeUpstream
		reason = "rate_limited"
		message += ": rate limited; try again later"
	default:
		if status >= http.StatusInternalServerError {
			message += ": server error; try again later"
		}
	}
	return &httpx.ProviderError{
		Code:        code,
		Reason:      reason,
		SafeMessage: message,
		StatusCode:  status,
	}
}

func (c *Client) requestFailure(err error, rawURL string) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if pe, ok := errors.AsType[*httpx.ProviderError](err); ok && pe != nil {
		return pe
	}
	message := err.Error()
	if urlErr, ok := errors.AsType[*url.Error](err); ok && urlErr != nil {
		message = urlErr.Err.Error()
		if urlErr.URL != "" {
			rawURL = urlErr.URL
		}
	}
	message = diagnosticURLPattern.ReplaceAllStringFunc(message, requestHost)
	safe := "request to " + requestHost(rawURL) + " failed: " + message
	return c.http.Redactor().WrapError(httpx.CodeUpstream, "request_failed", safe, err)
}

func apiErrorMessage(message string) string {
	if strings.Contains(message, "<") && strings.Contains(message, ">") {
		return "API returned an HTML error message"
	}
	message = strings.Join(strings.Fields(message), " ")
	message = diagnosticURLPattern.ReplaceAllStringFunc(message, requestHost)
	const maxRunes = 240
	if runes := []rune(message); len(runes) > maxRunes {
		message = string(runes[:maxRunes]) + "…"
	}
	return message
}
