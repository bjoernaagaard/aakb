// Package openalex implements the documented OpenAlex Works API over
// internal/httpx with artifact downloads through internal/store. It has no
// dependency on Anna's Archive: archive fallback lives with the caller.
package openalex

import (
	"context"
	"encoding/json"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bjoernaagaard/aakb/internal/httpx"
)

// Version is the aakb release version sent in the User-Agent header.
// Release builds override it with -X github.com/bjoernaagaard/aakb/internal/openalex.Version=....
var Version = "dev"

const (
	defaultBaseURL = "https://api.openalex.org"

	apiMaxAttempts = 3
	apiBaseDelay   = 500 * time.Millisecond
	apiMaxDelay    = 10 * time.Second

	errorBodyLimit   = 4096
	maxResponseBytes = 16 << 20
)

// Config configures an OpenAlex client. APIKey is required by every operation.
// HTTPClient, when set, supplies the base transport and timeouts; guards,
// redirect policy, and User-Agent always come from internal/httpx. BaseURL
// defaults to https://api.openalex.org and must stay an HTTPS origin.
// UserAgent overrides the default "aakb/<version>" header.
type Config struct {
	APIKey     string
	HTTPClient *http.Client
	BaseURL    string
	UserAgent  string
}

// Client queries the OpenAlex Works API. It is safe for concurrent use.
type Client struct {
	key  string
	base string
	http *httpx.Client
	err  error
}

// NewClient builds a client without network access. Invalid endpoints and a
// missing key surface as structured errors from the operation that needs them,
// except an invalid endpoint which fails every call.
func NewClient(config Config) *Client {
	base := strings.TrimRight(strings.TrimSpace(config.BaseURL), "/")
	if base == "" {
		base = defaultBaseURL
	}
	var configErr error
	endpoint, parseErr := url.Parse(base)
	if parseErr != nil || !strings.EqualFold(endpoint.Scheme, "https") ||
		endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" ||
		endpoint.Fragment != "" || endpoint.Path != "" {
		configErr = httpx.NewError(httpx.CodeConfig, "invalid_endpoint", "OpenAlex API endpoint must be an HTTPS origin")
	}
	agent := strings.TrimSpace(config.UserAgent)
	if agent == "" {
		agent = "aakb/" + Version
	}
	guarded := httpx.NewClient(httpx.ClientOptions{
		BaseClient: config.HTTPClient,
		UserAgent:  agent,
		Secrets:    []string{strings.TrimSpace(config.APIKey)},
	})
	return &Client{key: strings.TrimSpace(config.APIKey), base: base, http: guarded, err: configErr}
}

// Status reports the provider's documented rate-limit usage without assuming
// its units. Credential-shaped fields are removed and the key is redacted.
func (c *Client) Status(ctx context.Context) (Status, error) {
	var status Status
	if err := c.get(ctx, "/rate-limit", nil, &status); err != nil {
		return nil, err
	}
	redactStatus(map[string]any(status), c.key)
	return status, nil
}

func redactStatus(values map[string]any, key string) {
	for name, value := range values {
		lower := strings.ToLower(name)
		if lower == "key" || lower == "api_key" || lower == "authorization" ||
			strings.Contains(lower, "token") || strings.Contains(lower, "secret") ||
			strings.Contains(lower, "password") {
			delete(values, name)
			continue
		}
		values[name] = redactStatusValue(value, key)
	}
}

func redactStatusValue(value any, key string) any {
	switch typed := value.(type) {
	case string:
		if key == "" {
			return typed
		}
		return strings.ReplaceAll(typed, key, "[redacted]")
	case map[string]any:
		redactStatus(typed, key)
	case []any:
		for i, item := range typed {
			typed[i] = redactStatusValue(item, key)
		}
	}
	return value
}

func (c *Client) get(ctx context.Context, route string, values url.Values, target any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.err != nil {
		return c.err
	}
	if c.key == "" {
		return httpx.NewError(httpx.CodeConfig, "missing_key", "set OPENALEX_API_KEY to use OpenAlex")
	}
	parsed, err := url.Parse(c.base)
	if err != nil {
		return httpx.NewError(httpx.CodeConfig, "invalid_endpoint", "invalid OpenAlex API endpoint")
	}
	parsed.Path = route
	parsed.RawQuery = values.Encode()
	for attempt := 1; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
		if err != nil {
			return httpx.NewError(httpx.CodeUpstream, "request_failed", "could not create OpenAlex request")
		}
		req.Header.Set("Authorization", "Bearer "+c.key)
		req.Header.Set("Accept", "application/json")
		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if attempt < apiMaxAttempts {
				if sleepErr := sleepContext(ctx, jitteredBackoff(attempt)); sleepErr != nil {
					return sleepErr
				}
				continue
			}
			return httpx.WrapError(httpx.CodeUpstream, "request_failed", "OpenAlex request failed", err)
		}
		if resp.StatusCode == http.StatusOK {
			raw, readErr := httpx.ReadBounded(resp.Body, maxResponseBytes)
			if closeErr := resp.Body.Close(); closeErr != nil && readErr == nil {
				readErr = closeErr
			}
			if readErr != nil {
				return readErr
			}
			if err := json.Unmarshal(raw, target); err != nil {
				return httpx.WrapError(httpx.CodeUpstream, "invalid_response", "OpenAlex returned invalid JSON", err)
			}
			return nil
		}
		snippet, snippetErr := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
		closeErr := resp.Body.Close()
		if snippetErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return httpx.WrapError(httpx.CodeUpstream, "request_failed", "could not read OpenAlex error response", snippetErr)
		}
		if closeErr != nil {
			return httpx.WrapError(httpx.CodeUpstream, "request_failed", "could not complete OpenAlex request", closeErr)
		}
		retryAfter := resp.Header.Get("Retry-After")
		failure, retryable := responseError(resp.StatusCode, retryAfter, snippet, c.key)
		if !retryable || attempt >= apiMaxAttempts {
			return failure
		}
		delay, ok := retryDelay(retryAfter, attempt)
		if !ok {
			return failure
		}
		if sleepErr := sleepContext(ctx, delay); sleepErr != nil {
			return sleepErr
		}
	}
}

// responseError classifies a failed OpenAlex status without leaking the key.
// Daily budget exhaustion and plan errors are terminal; transient rate limits
// and 5xx responses carry a retry window and may be retried by the caller.
func responseError(status int, retryAfterHeader string, body []byte, key string) (*httpx.ProviderError, bool) {
	lowered := strings.ToLower(string(body))
	if key != "" {
		lowered = strings.ToLower(strings.ReplaceAll(string(body), key, "[redacted]"))
	}
	result := &httpx.ProviderError{
		Code:       httpx.CodeUpstream,
		Reason:     "provider_error",
		StatusCode: status,
	}
	retryable := false
	switch status {
	case http.StatusBadRequest:
		result.Code = httpx.CodeInvalidArgument
		result.Reason = "invalid_request"
		result.SafeMessage = "OpenAlex rejected the query or filters"
	case http.StatusNotFound:
		result.Code = httpx.CodeNotFound
		result.Reason = "work_not_found"
		result.SafeMessage = "OpenAlex could not find this work"
	case http.StatusUnauthorized, http.StatusForbidden:
		result.Code = httpx.CodeConfig
		result.Reason = "invalid_key"
		result.SafeMessage = "OpenAlex rejected OPENALEX_API_KEY"
	case http.StatusTooManyRequests:
		result.Reason = "rate_limited"
		result.SafeMessage = "OpenAlex temporarily limited requests"
		switch {
		case containsAny(lowered, "budget", "daily", "credits", "allowance"):
			result.Reason = "budget_exhausted"
			result.SafeMessage = "OpenAlex daily allowance is exhausted"
		case containsAny(lowered, "upgrade", "paid plan", "premium plan"):
			result.Reason = "plan_required"
			result.SafeMessage = "OpenAlex requires a different plan for this operation"
		default:
			retryable = true
		}
	default:
		result.SafeMessage = "OpenAlex could not complete the request"
		if status >= 500 {
			retryable = true
		}
	}
	if duration, ok := httpx.ParseRetryAfter(retryAfterHeader, time.Now()); ok {
		result.RetryAfter = duration
	}
	return result, retryable
}

func containsAny(haystack string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(haystack, needle) {
			return true
		}
	}
	return false
}

// retryDelay prefers the server's Retry-After window and falls back to
// jittered exponential backoff. It reports false when the server asks to wait
// longer than the retry budget so the caller can surface the window instead.
func retryDelay(retryAfterHeader string, attempt int) (time.Duration, bool) {
	if duration, ok := httpx.ParseRetryAfter(retryAfterHeader, time.Now()); ok {
		if duration > apiMaxDelay {
			return 0, false
		}
		return duration, true
	}
	return jitteredBackoff(attempt), true
}

func jitteredBackoff(attempt int) time.Duration {
	shift := min(max(attempt-1, 0), 10)
	delay := apiBaseDelay << shift
	if delay > apiMaxDelay || delay <= 0 {
		delay = apiMaxDelay
	}
	jitterRange := delay / 5
	if jitterRange <= 0 {
		return delay
	}
	offset := time.Duration(rand.Int64N(int64(2*jitterRange)+1)) - jitterRange //nolint:gosec // G404: retry backoff jitter is not security-sensitive
	return max(delay+offset, 0)
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	if duration <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
