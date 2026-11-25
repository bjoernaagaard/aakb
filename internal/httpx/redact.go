package httpx

import (
	"errors"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

const redactedToken = "REDACTED"

var (
	querySecretPattern  = regexp.MustCompile(`(?i)([?&](?:key|api_key|token)=)([^&\s"'<>]+)`)
	cookieSecretPattern = regexp.MustCompile(`(?i)(aa_account_id2=)([^;\s"'<>]+)`)
	bearerSecretPattern = regexp.MustCompile(`(?i)(Bearer\s+)([^\s"'<>]+)`)
)

// Redactor removes configured secrets and known credential patterns from strings
// and error chains while preserving errors.Is and errors.AsType matching.
type Redactor struct {
	needles []string
}

// NewRedactor builds a Redactor for the supplied secret values.
func NewRedactor(secrets ...string) *Redactor {
	seen := make(map[string]struct{})
	var needles []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		if _, ok := seen[s]; ok {
			return
		}
		seen[s] = struct{}{}
		needles = append(needles, s)
	}
	for _, raw := range secrets {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		add(raw)
		add(url.QueryEscape(raw))
		add(url.PathEscape(raw))
		// If a Cookie header or name=value pair was supplied, redact individual cookie values too.
		for part := range strings.SplitSeq(raw, ";") {
			if _, val, ok := strings.Cut(strings.TrimSpace(part), "="); ok {
				val = strings.TrimSpace(val)
				if val != "" {
					add(val)
					add(url.QueryEscape(val))
					add(url.PathEscape(val))
				}
			}
		}
	}
	// Redact longer needles first so substrings do not leave fragments behind.
	slices.SortFunc(needles, func(a, b string) int {
		return len(b) - len(a)
	})
	return &Redactor{needles: needles}
}

// RedactString replaces all configured secrets and credential patterns in s.
func (r *Redactor) RedactString(s string) string {
	if s == "" {
		return ""
	}
	if r != nil {
		for _, needle := range r.needles {
			s = strings.ReplaceAll(s, needle, redactedToken)
		}
	}
	s = querySecretPattern.ReplaceAllString(s, "${1}"+redactedToken)
	s = cookieSecretPattern.ReplaceAllString(s, "${1}"+redactedToken)
	s = bearerSecretPattern.ReplaceAllString(s, "${1}"+redactedToken)
	return s
}

type redactedError struct {
	message string
	cause   error
}

func (e *redactedError) Error() string { return e.message }
func (e *redactedError) Unwrap() error { return e.cause }

// RedactError ensures err.Error() never exposes configured secrets or URL keys.
// The returned error preserves err in its Unwrap chain for errors.Is/AsType.
func (r *Redactor) RedactError(err error) error {
	_, redacted := r.redactError(err)
	return redacted
}

func (r *Redactor) redactError(err error) (bool, error) {
	if err == nil {
		return false, nil
	}
	if pe, ok := errors.AsType[*ProviderError](err); ok && pe != nil {
		safe := r.RedactString(pe.SafeMessage)
		causeChanged, cause := r.redactError(pe.Cause)
		if safe == pe.SafeMessage && !causeChanged {
			return false, pe
		}
		copied := *pe
		copied.SafeMessage = safe
		copied.Cause = cause
		return true, &copied
	}
	raw := err.Error()
	redacted := r.RedactString(raw)
	if redacted == raw {
		return false, err
	}
	return true, &redactedError{message: redacted, cause: err}
}

// WrapError creates a ProviderError with its SafeMessage and Cause redacted.
func (r *Redactor) WrapError(code Code, reason, safeMessage string, cause error) *ProviderError {
	return &ProviderError{
		Code:        code,
		Reason:      reason,
		SafeMessage: r.RedactString(safeMessage),
		Cause:       r.RedactError(cause),
	}
}
