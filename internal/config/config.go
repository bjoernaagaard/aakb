// Package config reads aakb configuration from the environment once.
package config

import (
	"fmt"
	"log/slog"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
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
	Mirrors       []string
	AllowedTLDs   []string
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
		slog.Int("mirrors_count", len(c.Mirrors)),
		slog.Int("allowed_tlds_count", len(c.AllowedTLDs)),
		slog.Bool("auto_base_url", c.AutoBaseURL),
		slog.String("download_path", c.DownloadPath),
		slog.String("cache_dir", c.CacheDir),
		slog.Bool("secret_key_set", c.SecretKey != ""),
		slog.Bool("openalex_key_set", c.OpenAlexKey != ""),
	)
}

// FromEnv reads the CLI and server settings from the environment.
func FromEnv() (Config, error) {
	loadDotEnv(".env.local")
	loadDotEnv(".env")
	cacheDir := os.Getenv("AAKB_CACHE_DIR")
	downloadPath := os.Getenv("AAKB_ARCHIVE_DOWNLOAD_PATH")
	if downloadPath == "~" || strings.HasPrefix(downloadPath, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return Config{}, fmt.Errorf("home directory: %w", err)
		}
		downloadPath = expandHome(downloadPath, home)
	}
	auto := true
	if v := os.Getenv("AAKB_ARCHIVE_AUTO_BASE_URL"); v != "" {
		var err error
		auto, err = strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("AAKB_ARCHIVE_AUTO_BASE_URL must be a boolean")
		}
	}
	secretKey := os.Getenv("AAKB_ARCHIVE_SECRET_KEY")
	baseURL := os.Getenv("AAKB_ARCHIVE_BASE_URL")
	mirrors := parseCSV(os.Getenv("AAKB_ARCHIVE_MIRRORS"))
	allowedTLDs := parseTLDs(os.Getenv("AAKB_ARCHIVE_ALLOWED_TLDS"))
	if err := checkTLD("AAKB_ARCHIVE_BASE_URL", baseURL, allowedTLDs); err != nil {
		return Config{}, err
	}
	for _, mirror := range mirrors {
		if err := checkTLD("AAKB_ARCHIVE_MIRRORS", mirror, allowedTLDs); err != nil {
			return Config{}, err
		}
	}
	proxies, err := parsePrefixes(os.Getenv("AAKB_TRUSTED_PROXIES"))
	if err != nil {
		return Config{}, err
	}
	return Config{
		TrustedProxies: proxies,
		SecretKey:      secretKey,
		AccountCookie:  os.Getenv("AAKB_ARCHIVE_ACCOUNT_COOKIE"),
		BaseURL:        baseURL,
		Mirrors:        mirrors,
		AllowedTLDs:    allowedTLDs,
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
		SecretKey:     c.SecretKey,
		AccountCookie: c.AccountCookie,
		BaseURL:       c.BaseURL,
		Mirrors:       c.Mirrors,
		AllowedTLDs:   c.AllowedTLDs,
		Mirror:        mirror,
		DownloadDir:   c.DownloadPath,
		CacheDir:      c.CacheDir,
		OpenAlexKey:   c.OpenAlexKey,
	}
}

func parseCSV(raw string) []string {
	var out []string
	for part := range strings.SplitSeq(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func parseTLDs(raw string) []string {
	var out []string
	for part := range strings.SplitSeq(raw, ",") {
		if tld := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(part), ".")); tld != "" {
			out = append(out, tld)
		}
	}
	return out
}

// checkTLD rejects a mirror whose top-level domain is missing from a
// non-empty allowlist such as ["gl", "gd", "pk"].
func checkTLD(envName, rawHost string, allowed []string) error {
	if rawHost == "" || len(allowed) == 0 {
		return nil
	}
	host := strings.TrimSpace(rawHost)
	if u, err := url.Parse(host); err == nil && u.Hostname() != "" {
		host = u.Hostname()
	} else if u, err := url.Parse("https://" + host); err == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	idx := strings.LastIndex(host, ".")
	if idx < 0 || idx+1 >= len(host) {
		return fmt.Errorf("%s has a top-level domain outside AAKB_ARCHIVE_ALLOWED_TLDS", envName)
	}
	tld := strings.ToLower(host[idx+1:])
	if slices.Contains(allowed, tld) {
		return nil
	}
	return fmt.Errorf("%s has a top-level domain outside AAKB_ARCHIVE_ALLOWED_TLDS", envName)
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
