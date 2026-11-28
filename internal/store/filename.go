// Package store provides a unified local download store with an atomic JSON
// manifest, O(1) stat-verified reuse, cross-process lease locks, streaming
// dual-digest transfers, HTTP Range resume, and magic-byte validation.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"iter"
	"mime"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/bjoernaagaard/aakb/internal/httpx"
)

const maxFilenameBytes = 180

var (
	unsafeFilenameChars = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)
	formatPattern       = regexp.MustCompile(`^[A-Za-z0-9]{1,16}$`)
)

// SanitizeFilename preserves readable Unicode while avoiding reserved names,
// path traversal, and platform filename length limits.
func SanitizeFilename(filename string) string {
	safe := unsafeFilenameChars.ReplaceAllString(filename, "_")
	safe = strings.ReplaceAll(safe, "..", "_")
	safe = strings.TrimRight(filepath.Base(safe), " .")
	if safe == "" {
		return ""
	}
	stem := strings.ToUpper(strings.SplitN(safe, ".", 2)[0])
	stem = strings.TrimRight(stem, " .")
	switch stem {
	case "CON", "PRN", "AUX", "NUL",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		safe = "_" + safe
	}
	if len(safe) <= maxFilenameBytes {
		return safe
	}
	var result strings.Builder
	for _, r := range safe {
		if result.Len()+utf8.RuneLen(r) > maxFilenameBytes {
			break
		}
		result.WriteRune(r)
	}
	return strings.TrimRight(result.String(), " .")
}

// ValidFilename reports whether name is a single local, non-hidden, finalized
// filename safe to open inside the download root directory.
func ValidFilename(name string) bool {
	if !filepath.IsLocal(name) || strings.ContainsAny(name, "/\\\x00") || strings.HasPrefix(name, ".") {
		return false
	}
	ext := filepath.Ext(name)
	return !strings.EqualFold(ext, ".part") && !strings.EqualFold(ext, ".tmp")
}

// NormalizeFormat validates and normalizes a file extension, defaulting empty
// values to "bin".
func NormalizeFormat(format string) (string, error) {
	format = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(format), "."))
	if format == "" {
		return "bin", nil
	}
	if !formatPattern.MatchString(format) {
		return "", httpx.NewError(httpx.CodeInvalidArgument, "invalid_format", "file format must be a simple extension")
	}
	return format, nil
}

// RequireFormat normalizes raw and verifies it equals want, defaulting an empty
// value to want and returning mismatchMessage on any other format.
func RequireFormat(raw, want, mismatchMessage string) (string, error) {
	format := strings.TrimSpace(raw)
	if format == "" {
		return want, nil
	}
	normalized, err := NormalizeFormat(format)
	if err != nil {
		return "", err
	}
	if normalized != want {
		return "", httpx.NewError(httpx.CodeInvalidArgument, "invalid_format", mismatchMessage)
	}
	return normalized, nil
}

// DownloadFormat infers a file extension from response headers or the final URL,
// falling back to "bin".
func DownloadFormat(header http.Header, requestURL *url.URL) string {
	if cd := header.Get("Content-Disposition"); cd != "" {
		if _, params, err := mime.ParseMediaType(cd); err == nil {
			if filename, ok := params["filename"]; ok {
				if ext := strings.TrimPrefix(filepath.Ext(filename), "."); formatPattern.MatchString(ext) {
					return strings.ToLower(ext)
				}
			}
		}
	}
	if ext := extensionFromMediaType(header.Get("Content-Type")); ext != "" {
		return ext
	}
	if requestURL != nil {
		if ext := strings.TrimPrefix(path.Ext(requestURL.Path), "."); formatPattern.MatchString(ext) {
			return strings.ToLower(ext)
		}
	}
	return "bin"
}

func extensionFromMediaType(contentType string) string {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType == "" || mediaType == "application/octet-stream" || mediaType == "binary/octet-stream" {
		return ""
	}
	extensions, err := mime.ExtensionsByType(mediaType)
	if err != nil || len(extensions) == 0 {
		return ""
	}
	ext := strings.ToLower(strings.TrimPrefix(extensions[0], "."))
	if !formatPattern.MatchString(ext) {
		return ""
	}
	return ext
}

// CandidateNames yields non-overwriting candidate basenames for a download.
// If digestPrefix is non-empty, it is used directly (truncated to 8 safe characters);
// otherwise a short SHA-256 suffix is derived from fallbackKey.
func CandidateNames(title, format, digestPrefix, fallbackKey string) iter.Seq[string] {
	base := SanitizeFilename(title)
	if base == "" {
		base = "untitled"
	}
	suffix := sanitizeSuffix(digestPrefix)
	if suffix == "" && fallbackKey != "" {
		sum := sha256.Sum256([]byte(fallbackKey))
		suffix = hex.EncodeToString(sum[:4])
	}
	return func(yield func(string) bool) {
		if !yield(base + "." + format) {
			return
		}
		if suffix != "" && !yield(base+"-"+suffix+"."+format) {
			return
		}
		for i := 2; ; i++ {
			var name string
			if suffix != "" {
				name = fmt.Sprintf("%s-%s-%d.%s", base, suffix, i, format)
			} else {
				name = fmt.Sprintf("%s-%d.%s", base, i, format)
			}
			if !yield(name) {
				return
			}
		}
	}
}

func sanitizeSuffix(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if len(raw) > 8 {
		raw = raw[:8]
	}
	var b strings.Builder
	for _, r := range raw {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}
