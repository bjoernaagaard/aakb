// Package httpx provides a shared HTTPS transport with strict redirect guards,
// credential redaction, structured provider errors, bounded reads, and retries.
package httpx

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

var diagnosticURLPattern = regexp.MustCompile(`https?://[^\s<>"']+`)

// Code categorizes an upstream or local provider failure.
type Code string

const (
	// CodeInvalidArgument indicates invalid request parameters.
	CodeInvalidArgument Code = "INVALID_ARGUMENT"
	// CodeConfig indicates missing or invalid configuration or credentials.
	CodeConfig Code = "CONFIG"
	// CodeNotFound indicates the requested item does not exist upstream.
	CodeNotFound Code = "NOT_FOUND"
	// CodeUpstreamBlocked indicates access was denied or challenged upstream.
	CodeUpstreamBlocked Code = "UPSTREAM_BLOCKED"
	// CodeTimeout indicates an operation or upstream transfer timed out.
	CodeTimeout Code = "REQUEST_TIMEOUT"
	// CodeCanceled indicates the caller canceled the operation.
	CodeCanceled Code = "CANCELED"
	// CodeUpstream indicates an upstream service or transport failure.
	CodeUpstream Code = "UPSTREAM"
	// CodeInternal indicates a local or unexpected failure.
	CodeInternal Code = "INTERNAL"
)

var (
	// ErrBodyIdleTimeout indicates a streaming response body stopped delivering bytes.
	ErrBodyIdleTimeout = errors.New("response body idle timeout")
	// ErrRedirectRefused indicates a redirect violated the security policy.
	ErrRedirectRefused = errors.New("redirect refused by security policy")
	// ErrUnsafeURL indicates a URL violated HTTPS or credential safety rules.
	ErrUnsafeURL = errors.New("URL must use HTTPS without embedded credentials")
	// ErrResponseTooLarge indicates an upstream response exceeded its byte limit.
	ErrResponseTooLarge = errors.New("response exceeded size limit")
)

// ProviderError carries structured failure metadata with a credential-free message.
type ProviderError struct {
	Code        Code
	Reason      string
	SafeMessage string
	RetryAfter  time.Duration
	StatusCode  int
	Cause       error
}

// Error returns the safe, redacted error message without exposing raw causes.
func (e *ProviderError) Error() string {
	if e.SafeMessage != "" {
		return e.SafeMessage
	}
	if e.Reason != "" {
		return e.Reason
	}
	return string(e.Code)
}

// Unwrap returns the underlying redacted cause, if any.
func (e *ProviderError) Unwrap() error {
	return e.Cause
}

// NewError creates a ProviderError without an underlying cause.
func NewError(code Code, reason, safeMessage string) *ProviderError {
	return &ProviderError{
		Code:        code,
		Reason:      reason,
		SafeMessage: safeMessage,
	}
}

// WrapError creates a ProviderError wrapping an underlying cause.
func WrapError(code Code, reason, safeMessage string, cause error) *ProviderError {
	return &ProviderError{
		Code:        code,
		Reason:      reason,
		SafeMessage: safeMessage,
		Cause:       cause,
	}
}

// RequestHost returns the hostname from rawURL, or "upstream server" if unavailable.
func RequestHost(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Hostname() == "" {
		return "upstream server"
	}
	return parsed.Hostname()
}

// ResponseHost returns the final request hostname for resp, falling back to rawURL.
func ResponseHost(resp *http.Response, rawURL string) string {
	if resp != nil && resp.Request != nil && resp.Request.URL != nil {
		rawURL = resp.Request.URL.String()
	}
	return RequestHost(rawURL)
}

// SanitizeDiagnosticURLs replaces embedded HTTP(S) URLs in message with their hostnames.
func SanitizeDiagnosticURLs(message string) string {
	return diagnosticURLPattern.ReplaceAllStringFunc(message, RequestHost)
}

// StatusError maps a non-2xx HTTP response to a structured ProviderError without leaking paths.
func StatusError(resp *http.Response, rawURL string) *ProviderError {
	status := resp.StatusCode
	host := ResponseHost(resp, rawURL)
	message := strings.TrimSpace(fmt.Sprintf("HTTP status %d %s", status, http.StatusText(status))) + " from " + host
	code := CodeUpstream
	reason := "http_status"
	switch status {
	case http.StatusNotFound, http.StatusGone:
		code = CodeNotFound
		reason = "not_found"
		message += ": requested resource is unavailable on this server"
	case http.StatusUnauthorized, http.StatusForbidden:
		code = CodeUpstreamBlocked
		reason = "access_denied"
		message += ": access denied"
	case http.StatusTooManyRequests:
		code = CodeUpstream
		reason = "rate_limited"
		message += ": rate limited; try again later"
	default:
		if status >= http.StatusInternalServerError {
			message += ": server error; try again later"
		}
	}
	return &ProviderError{
		Code:        code,
		Reason:      reason,
		SafeMessage: message,
		StatusCode:  status,
	}
}

// RequestError wraps a transport or read failure into a credential-safe ProviderError,
// passing context errors and existing ProviderErrors through unchanged.
func (c *Client) RequestError(err error, rawURL string) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if pe, ok := errors.AsType[*ProviderError](err); ok && pe != nil {
		return pe
	}
	message := err.Error()
	if urlErr, ok := errors.AsType[*url.Error](err); ok && urlErr != nil {
		message = urlErr.Err.Error()
		if urlErr.URL != "" {
			rawURL = urlErr.URL
		}
	}
	message = SanitizeDiagnosticURLs(message)
	safe := "request to " + RequestHost(rawURL) + " failed: " + message
	return c.redactor.WrapError(CodeUpstream, "request_failed", safe, err)
}

// IsTerminalDownloadError reports download failures that must not fall back to
// another source: canceled or expired contexts, configuration errors, and local I/O errors.
func IsTerminalDownloadError(ctx context.Context, err error) bool {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if pathErr, ok := errors.AsType[*os.PathError](err); ok && pathErr != nil {
		return true
	}
	if pe, ok := errors.AsType[*ProviderError](err); ok && pe != nil {
		return pe.Code == CodeConfig || pe.Reason == "local_io"
	}
	return false
}
