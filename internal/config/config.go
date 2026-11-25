// Package config reads aakb configuration from the environment once.
package config

import (
	"fmt"
	"log/slog"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/bjoernaagaard/aakb"
)

// Config holds the settings shared by the MCP server and the CLI.
// The secret key is never logged and never accepted as a tool argument.
type Config struct {
	SecretKey     string
	AccountCookie string
	BaseURL       string
	AutoBaseURL   bool
	DownloadPath  string
	CacheDir      string
	OpenAlexKey   string
	HTTPToken     string
	// TrustedProxies holds the proxy networks whose X-Forwarded-For
	// header aakb serve believes when it counts requests per client.
	TrustedProxies []netip.Prefix
}

// LogValue redacts credentials so a Config can never leak through a log line.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Bool("base_url_set", c.BaseURL != ""),
		slog.Bool("auto_base_url", c.AutoBaseURL),
		slog.String("download_path", c.DownloadPath),
		slog.String("cache_dir", c.CacheDir),
		slog.Bool("secret_key_set", c.SecretKey != ""),
		slog.Bool("openalex_key_set", c.OpenAlexKey != ""),
	)
}

// FromEnv builds a Config from ANNAS_* variables, for compatibility with
// the existing Anna's Archive tools, plus AAKB_CACHE_DIR.
func FromEnv() (Config, error) {
	loadDotEnv(".env.local")
	loadDotEnv(".env")
	cacheDir := os.Getenv("AAKB_CACHE_DIR")
	if cacheDir == "" {
		cacheDir = os.Getenv("ANNAS_MCP_CACHE_DIR")
	}
	downloadPath := os.Getenv("ANNAS_DOWNLOAD_PATH")
	if downloadPath == "~" || strings.HasPrefix(downloadPath, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return Config{}, fmt.Errorf("home directory: %w", err)
		}
		downloadPath = expandHome(downloadPath, home)
	}
	auto := true
	if v := os.Getenv("ANNAS_AUTO_BASE_URL"); v != "" {
		var err error
		auto, err = strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("ANNAS_AUTO_BASE_URL must be a boolean")
		}
	}
	secretKey := os.Getenv("ANNAS_SECRET_KEY")
	if secretKey == "" {
		// Some credential stores name the same key ANNAS_ARCHIVE_API_KEY.
		secretKey = os.Getenv("ANNAS_ARCHIVE_API_KEY")
	}
	baseURL := os.Getenv("ANNAS_BASE_URL")
	if err := checkTLD(baseURL, os.Getenv("ANNAS_ALLOWED_TLDS")); err != nil {
		return Config{}, err
	}
	proxies, err := parsePrefixes(os.Getenv("AAKB_TRUSTED_PROXIES"))
	if err != nil {
		return Config{}, err
	}
	return Config{
		TrustedProxies: proxies,
		SecretKey:      secretKey,
		AccountCookie:  os.Getenv("ANNAS_ACCOUNT_COOKIE"),
		BaseURL:        baseURL,
		AutoBaseURL:    auto,
		DownloadPath:   downloadPath,
		CacheDir:       cacheDir,
		OpenAlexKey:    os.Getenv("OPENALEX_API_KEY"),
		HTTPToken:      os.Getenv("AAKB_HTTP_TOKEN"),
	}, nil
}

// Client returns the provider configuration for the public library.
func (c Config) Client() aakb.Config {
	mirror := aakb.MirrorFailover
	if !c.AutoBaseURL {
		mirror = aakb.MirrorFixed
	}
	return aakb.Config{
		SecretKey: c.SecretKey, AccountCookie: c.AccountCookie, BaseURL: c.BaseURL, Mirror: mirror,
		DownloadDir: c.DownloadPath, CacheDir: c.CacheDir, OpenAlexKey: c.OpenAlexKey,
	}
}

// checkTLD rejects a mirror whose top-level domain is missing from a
// non-empty comma-separated allowlist such as "gl,gd,pk".
func checkTLD(baseURL, allowed string) error {
	if baseURL == "" || strings.TrimSpace(allowed) == "" {
		return nil
	}
	host := baseURL
	if u, err := url.Parse(baseURL); err == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	tld := strings.ToLower(host[strings.LastIndex(host, ".")+1:])
	for part := range strings.SplitSeq(allowed, ",") {
		if strings.ToLower(strings.TrimPrefix(strings.TrimSpace(part), ".")) == tld {
			return nil
		}
	}
	return fmt.Errorf("ANNAS_BASE_URL has a top-level domain outside ANNAS_ALLOWED_TLDS")
}

// parsePrefixes reads comma-separated addresses or CIDR networks.
func parsePrefixes(list string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for part := range strings.SplitSeq(list, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(part)
		if err != nil {
			addr, addrErr := netip.ParseAddr(part)
			if addrErr != nil {
				return nil, fmt.Errorf("AAKB_TRUSTED_PROXIES must list IP addresses or CIDR networks")
			}
			prefix = netip.PrefixFrom(addr, addr.BitLen())
		}
		out = append(out, prefix.Masked())
	}
	return out, nil
}

func expandHome(path, home string) string {
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
}

// loadDotEnv sets missing variables from a simple KEY=value file.
// Variables already in the environment always win.
func loadDotEnv(path string) {
	data, err := os.ReadFile(path) //nolint:gosec // path is the fixed .env in the working directory
	if err != nil {
		return
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, value) //nolint:errcheck // best-effort .env load
		}
	}
}
