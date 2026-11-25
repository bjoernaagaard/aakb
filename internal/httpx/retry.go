package httpx

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	defaultMaxAttempts = 3
	defaultBaseDelay   = 500 * time.Millisecond
	defaultMaxDelay    = 10 * time.Second
	drainLimitBytes    = 64 << 10
)

// RetryPolicy configures idempotent GET/HEAD retries for HTTP 429 and 5xx responses.
type RetryPolicy struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	ShouldRetry func(resp *http.Response, err error) bool
}

// DefaultRetryPolicy returns the standard retry configuration.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts: defaultMaxAttempts,
		BaseDelay:   defaultBaseDelay,
		MaxDelay:    defaultMaxDelay,
	}
}

func (p RetryPolicy) normalized() RetryPolicy {
	if p.MaxAttempts <= 0 {
		p.MaxAttempts = defaultMaxAttempts
	}
	if p.BaseDelay <= 0 {
		p.BaseDelay = defaultBaseDelay
	}
	if p.MaxDelay <= 0 {
		p.MaxDelay = defaultMaxDelay
	}
	return p
}

// ParseRetryAfter parses an HTTP Retry-After header as either delta seconds or an HTTP date.
func ParseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds < 0 {
			return 0, true
		}
		return time.Duration(seconds) * time.Second, true
	}
	for _, layout := range []string{http.TimeFormat, time.RFC850, time.ANSIC} {
		if t, err := time.Parse(layout, value); err == nil {
			delay := t.Sub(now)
			if delay < 0 {
				return 0, true
			}
			return delay, true
		}
	}
	return 0, false
}

// DoWithRetry executes an idempotent request (GET or HEAD) with Retry-After
// and jittered exponential backoff for 429/5xx responses and transient errors.
func (c *Client) DoWithRetry(ctx context.Context, req *http.Request, policy RetryPolicy) (*http.Response, error) {
	if req == nil {
		return c.Do(nil)
	}
	policy = policy.normalized()
	idempotent := (req.Method == http.MethodGet || req.Method == http.MethodHead) && (req.Body == nil || req.GetBody != nil)
	if !idempotent {
		return c.Do(req.WithContext(ctx))
	}

	var lastErr error
	for attempt := 1; attempt <= policy.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		attemptReq := req.Clone(ctx)
		if req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, c.redactor.RedactError(err)
			}
			attemptReq.Body = body
		}

		resp, err := c.Do(attemptReq)
		if err == nil {
			if !shouldRetryResponse(resp, policy) || attempt == policy.MaxAttempts {
				return resp, nil
			}
			delay, ok := retryDelay(resp, attempt, policy)
			if !ok {
				// Retry-After exceeds MaxDelay; return the response so the caller can report the window.
				return resp, nil
			}
			DrainAndClose(resp.Body)
			if sleepErr := sleepContext(ctx, delay); sleepErr != nil {
				return nil, sleepErr
			}
			continue
		}

		lastErr = err
		if !shouldRetryError(ctx, err, policy) || attempt == policy.MaxAttempts {
			return nil, lastErr
		}
		delay := jitteredBackoff(attempt, policy.BaseDelay, policy.MaxDelay)
		if sleepErr := sleepContext(ctx, delay); sleepErr != nil {
			return nil, sleepErr
		}
	}
	return nil, lastErr
}

func shouldRetryResponse(resp *http.Response, policy RetryPolicy) bool {
	if policy.ShouldRetry != nil {
		return policy.ShouldRetry(resp, nil)
	}
	return resp.StatusCode == http.StatusTooManyRequests ||
		(resp.StatusCode >= http.StatusInternalServerError && resp.StatusCode <= 599)
}

func shouldRetryError(ctx context.Context, err error, policy RetryPolicy) bool {
	if ctx.Err() != nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, ErrRedirectRefused) || errors.Is(err, ErrUnsafeURL) {
		return false
	}
	if policy.ShouldRetry != nil {
		return policy.ShouldRetry(nil, err)
	}
	return true
}

func retryDelay(resp *http.Response, attempt int, policy RetryPolicy) (time.Duration, bool) {
	if retryAfter, ok := ParseRetryAfter(resp.Header.Get("Retry-After"), time.Now()); ok {
		if retryAfter > policy.MaxDelay {
			return 0, false
		}
		return retryAfter, true
	}
	return jitteredBackoff(attempt, policy.BaseDelay, policy.MaxDelay), true
}

func jitteredBackoff(attempt int, base, maxDelay time.Duration) time.Duration {
	shift := min(max(attempt-1, 0), 10)
	delay := base << shift
	if delay > maxDelay || delay <= 0 {
		delay = maxDelay
	}
	// Apply +/-20% jitter so concurrent callers do not synchronize retries.
	jitterRange := delay / 5
	if jitterRange <= 0 {
		return delay
	}
	offset := time.Duration(rand.Int64N(int64(2*jitterRange)+1)) - jitterRange //nolint:gosec // G404: retry backoff jitter is not security-sensitive
	return max(delay+offset, 0)
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// ReadBounded reads at most limit bytes from r. If more than limit bytes are
// available, it returns a *ProviderError wrapping ErrResponseTooLarge.
func ReadBounded(r io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		limit = DefaultMaxBodyBytes
	}
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, WrapError(CodeUpstream, "response_too_large", "upstream response exceeded the size limit", ErrResponseTooLarge)
	}
	return data, nil
}

// DrainAndClose discards up to 64 KiB of body so HTTP keep-alive connections
// can be reused, then closes body.
func DrainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, drainLimitBytes)) //nolint:errcheck // best-effort drain before close
	_ = body.Close()                                                  //nolint:errcheck // closing a drained response body
}
