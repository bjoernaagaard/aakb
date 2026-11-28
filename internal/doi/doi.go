// Package doi normalizes, validates, and detects Digital Object Identifiers
// (DOIs) and defines the shared DOI PDF download request shape.
package doi

import (
	"net/url"
	"regexp"
	"strings"
)

var validPattern = regexp.MustCompile(`(?i)^10\.\d{4,9}/\S+$`)

// DownloadRequest describes a DOI PDF download passed to a DOI download client.
type DownloadRequest struct {
	DOI      string
	Title    string
	Format   string
	Progress func(done, total int64)
}

// Normalize removes common DOI URL/label prefixes and enclosing wrappers,
// as often as they occur, as in "https://doi.org/doi:10.1000/x". Suffix
// punctuation is significant and is not guessed away as prose.
func Normalize(raw string) string {
	value := raw
	for {
		// Each round only shortens the text, so this terminates.
		next := normalizeOnce(value)
		if next == value {
			return next
		}
		value = next
	}
}

func normalizeOnce(value string) string {
	value = unwrap(value)
	if strings.HasPrefix(strings.ToLower(value), "doi:") {
		value = unwrap(value[len("doi:"):])
	}
	lower := strings.ToLower(value)
	for _, prefix := range []string{"https://doi.org/", "http://doi.org/", "https://dx.doi.org/", "http://dx.doi.org/"} {
		if strings.HasPrefix(lower, prefix) {
			if parsed, err := url.Parse(value); err == nil && parsed.Host != "" {
				// URL.Path is decoded by net/url, so an escaped slash in a DOI
				// becomes its normal slash separator, and tracking query/fragment
				// parameters are dropped.
				value = strings.TrimPrefix(parsed.Path, "/")
			} else {
				value = value[len(prefix):]
			}
			break
		}
	}
	return strings.TrimSpace(value)
}

func unwrap(value string) string {
	value = strings.TrimSpace(value)
	for len(value) >= 2 {
		first, last := value[0], value[len(value)-1]
		wrapped := first == '<' && last == '>' || first == '(' && last == ')' ||
			first == '[' && last == ']' || first == '{' && last == '}' ||
			first == '"' && last == '"' || first == '\'' && last == '\''
		if !wrapped {
			break
		}
		value = strings.TrimSpace(value[1 : len(value)-1])
	}
	return value
}

// IsQuery reports whether query explicitly addresses a DOI. It intentionally
// returns true for malformed DOI-shaped input so callers can report an invalid
// DOI instead of silently treating it as a keyword search.
func IsQuery(query string) bool {
	query = strings.ToLower(unwrap(query))
	return strings.HasPrefix(query, "10.") ||
		strings.HasPrefix(query, "doi:") ||
		strings.HasPrefix(query, "https://doi.org/") ||
		strings.HasPrefix(query, "http://doi.org/") ||
		strings.HasPrefix(query, "https://dx.doi.org/") ||
		strings.HasPrefix(query, "http://dx.doi.org/")
}

// Valid reports whether value has a DOI's shape: 10.<registrant>/<suffix>.
func Valid(value string) bool {
	return validPattern.MatchString(value)
}

// Parse normalizes raw and reports whether the result is a valid DOI.
func Parse(raw string) (string, bool) {
	normalized := Normalize(raw)
	return normalized, validPattern.MatchString(normalized)
}
