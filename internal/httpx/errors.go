// Package httpx provides a shared HTTPS transport with strict redirect guards,
// credential redaction, structured provider errors, bounded reads, and retries.
package httpx

import (
	"errors"
	"time"
)

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
