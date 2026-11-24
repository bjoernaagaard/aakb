// Package config reads aakb configuration from the environment once.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
}

// FromEnv builds a Config from ANNAS_* variables, for compatibility with
// the existing Anna's Archive tools, plus AAKB_CACHE_DIR.
func FromEnv() (Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, fmt.Errorf("home directory: %w", err)
	}
	cacheDir := os.Getenv("AAKB_CACHE_DIR")
	if cacheDir == "" {
		cacheDir = os.Getenv("ANNAS_MCP_CACHE_DIR")
	}
	if cacheDir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return Config{}, fmt.Errorf("cache directory: %w", err)
		}
		cacheDir = filepath.Join(base, "aakb")
	}
	downloadPath := expandHome(os.Getenv("ANNAS_DOWNLOAD_PATH"), home)
	if downloadPath == "" {
		downloadPath = filepath.Join(home, "annas-archive")
	}
	auto := true
	if v := os.Getenv("ANNAS_AUTO_BASE_URL"); v != "" {
		auto, err = strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("ANNAS_AUTO_BASE_URL: %w", err)
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
	}, nil
}

// SessionFile returns the persisted session path inside the cache dir.
func (c Config) SessionFile() string {
	return filepath.Join(c.CacheDir, "session.json")
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
