// Package config reads aakb configuration from the environment once.
package config

import (
	"fmt"
	"log/slog"
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
		// Homelab stores this credential as ANNAS_ARCHIVE_API_KEY.
		secretKey = os.Getenv("ANNAS_ARCHIVE_API_KEY")
	}
	return Config{
		SecretKey:     secretKey,
		AccountCookie: os.Getenv("ANNAS_ACCOUNT_COOKIE"),
		BaseURL:       os.Getenv("ANNAS_BASE_URL"),
		AutoBaseURL:   auto,
		DownloadPath:  downloadPath,
		CacheDir:      cacheDir,
		OpenAlexKey:   os.Getenv("OPENALEX_API_KEY"),
		HTTPToken:     os.Getenv("AAKB_HTTP_TOKEN"),
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
