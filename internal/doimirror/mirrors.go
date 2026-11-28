package doimirror

import (
	"context"
	"errors"
	"net/http"

	"github.com/bjoernaagaard/aakb/internal/httpx"
	"github.com/bjoernaagaard/aakb/internal/mirror"
)

const (
	mirrorStateFilename = "doi-mirrors.json"
	maxProbeBodyBytes   = 256 << 10
)

var (
	errMirrorChallenged   = errors.New("mirror returned an anti-bot or CAPTCHA challenge")
	errNotMirrorPage      = errors.New("did not answer with a DOI mirror page")
	errNoMirrorConfigured = httpx.NewError(httpx.CodeConfig, "no_doi_mirror_configured", "set DOIMirrorBaseURL or DOIMirrors for DOI mirror access")
)

var doiOriginOptions = mirror.OriginOptions{
	PublicOnly:        true,
	InvalidReason:     "invalid_doi_base_url",
	InvalidPrefix:     "DOI mirror base URL",
	ParseErrorMessage: "invalid DOI mirror base URL",
}

// ParseBaseURL validates and canonicalizes a configured DOI mirror origin,
// rejecting non-HTTPS schemes, local/private addresses, credentials, paths,
// queries, and fragments.
func ParseBaseURL(raw string) (string, error) {
	return mirror.ParseOrigin(raw, doiOriginOptions)
}

func validateDOIMirrorProbe(body []byte) error {
	if IsChallengeHTML(body) {
		return errMirrorChallenged
	}
	if !looksLikeMirrorLanding(body) {
		return errNotMirrorPage
	}
	return nil
}

func classifyDOIMirrorProbeError(err error) mirror.Failure {
	return mirror.ClassifyProbeFailure(err, func(e error) (mirror.Failure, bool) {
		switch {
		case errors.Is(e, errMirrorChallenged):
			return mirror.Failure{
				Reason:  "answered its check with an anti-bot challenge",
				Blocked: true,
			}, true
		case errors.Is(e, errNotMirrorPage):
			return mirror.Failure{
				Reason:  "answered its check with an unexpected page",
				Blocked: true,
			}, true
		default:
			return mirror.Failure{}, false
		}
	})
}

func (c *Client) baseURLFor(ctx context.Context, exclude ...string) (string, error) {
	if c.baseURL == "" {
		return "", errNoMirrorConfigured
	}
	if !c.autoBaseURL {
		if len(exclude) > 0 {
			return "", mirror.ErrAllFailed
		}
		return c.baseURL, nil
	}
	host, err := c.selector.Select(ctx, exclude...)
	if err != nil {
		return "", mirror.UnavailableProviderError(err, "doi_mirrors_blocked", "doi_mirrors_unavailable")
	}
	return "https://" + host, nil
}

func (c *Client) onMirror(ctx context.Context, fn func(base string) error) error {
	var excluded []string
	var lastErr error
	maxAttempts := max(len(c.selector.Hosts()), 1)
	if !c.autoBaseURL {
		maxAttempts = 1
	}
	for range maxAttempts {
		base, err := c.baseURLFor(ctx, excluded...)
		if err != nil {
			if lastErr != nil {
				return lastErr
			}
			return err
		}
		err = fn(base)
		if err == nil {
			c.selector.RecordSuccess(base)
			return nil
		}
		lastErr = err
		host, tracked := c.selector.TrackedHost(base)
		if !tracked || ctx.Err() != nil {
			return err
		}
		// Only fail over when the mirror itself was marked ineligible (blocked,
		// rate-limited, 5xx, or unreachable); a clean NOT_FOUND stops failover.
		if c.health.Eligible(host) {
			return err
		}
		excluded = append(excluded, host)
		c.log.InfoContext(ctx, "failing over to another DOI mirror", "failed_host", host)
	}
	return lastErr
}

func responseFailure(resp *http.Response) *mirror.Failure {
	if resp.StatusCode == http.StatusForbidden {
		return &mirror.Failure{Reason: "refused access (HTTP 403)", Blocked: true}
	}
	return mirror.ResponseFailure(resp)
}
