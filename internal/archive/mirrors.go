package archive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/bjoernaagaard/aakb/internal/httpx"
	"github.com/bjoernaagaard/aakb/internal/mirror"
)

const (
	mirrorProbePath     = "/dyn/up/"
	mirrorStateFilename = "mirrors.json"
	maxProbeBodyBytes   = 64 << 10
)

var (
	errNotUptime          = errors.New("did not answer with archive uptime status")
	errNoMirrorConfigured = httpx.NewError(httpx.CodeConfig, "no_mirror_configured", "set BaseURL or Mirrors for archive access")
)

var archiveOriginOptions = mirror.OriginOptions{
	InvalidReason:     "invalid_base_url",
	InvalidPrefix:     "base URL",
	ParseErrorMessage: "invalid archive base URL",
}

// MirrorHealth summarizes the current probe and health state of one mirror.
type MirrorHealth struct {
	Host       string    `json:"host"`
	Active     bool      `json:"active,omitempty"`
	Status     string    `json:"status"`
	HTTPStatus int       `json:"http_status,omitempty"`
	Failures   int       `json:"failures,omitempty"`
	HeldUntil  time.Time `json:"held_until,omitzero"`
	LastOK     time.Time `json:"last_ok,omitzero"`
	LastFail   time.Time `json:"last_fail,omitzero"`
	Reason     string    `json:"reason,omitempty"`
	Blocked    bool      `json:"blocked,omitempty"`
}

// HealthStatus reports the active mirror, overall status, HTTP status, and
// per-mirror health summary from probing /dyn/up/.
type HealthStatus struct {
	ActiveMirror string         `json:"active_mirror,omitempty"`
	Status       string         `json:"status"`
	HTTPStatus   int            `json:"http_status"`
	Mirrors      []MirrorHealth `json:"mirrors,omitempty"`
}

// ParseBaseURL validates and canonicalizes a configured archive origin,
// rejecting non-HTTPS schemes, credentials, paths, queries, and fragments.
func ParseBaseURL(raw string) (string, error) {
	return mirror.ParseOrigin(raw, archiveOriginOptions)
}

// isUpAnswer verifies that /dyn/up/ returned a JSON object containing "aa_logged_in".
func isUpAnswer(body []byte) bool {
	var status map[string]json.RawMessage
	if err := json.Unmarshal(body, &status); err != nil || status == nil {
		return false
	}
	_, ok := status["aa_logged_in"]
	return ok
}

func validateArchiveProbe(body []byte) error {
	if !isUpAnswer(body) {
		return errNotUptime
	}
	return nil
}

func classifyArchiveProbeError(err error) mirror.Failure {
	return mirror.ClassifyProbeFailure(err, func(e error) (mirror.Failure, bool) {
		if errors.Is(e, errNotUptime) {
			return mirror.Failure{
				Reason:  "answered its check with a page that is not the archive",
				Blocked: true,
			}, true
		}
		return mirror.Failure{}, false
	})
}

// ProbeHealth probes candidate archive mirrors in parallel via /dyn/up/ and
// returns the active mirror, overall status, HTTP status, and per-mirror summary.
func (c *Client) ProbeHealth(ctx context.Context) HealthStatus {
	hosts := c.selector.Hosts()
	if len(hosts) == 0 || ctx.Err() != nil {
		return HealthStatus{Status: "unreachable"}
	}

	probeCtx, cancel := context.WithTimeout(ctx, mirror.CheckTimeout)
	defer cancel()

	type probeResult struct {
		httpStatus int
		err        error
	}
	outcomes := make([]probeResult, len(hosts))
	var wg sync.WaitGroup
	for i, host := range hosts {
		wg.Go(func() {
			status, err := c.prober.Check(probeCtx, host)
			outcomes[i] = probeResult{httpStatus: status, err: err}
		})
	}
	wg.Wait()

	current := c.selector.Current()
	activeMirror := ""
	activeHTTPStatus := 0
	firstChallengedStatus := 0
	anyChallenged := false

	for i, host := range hosts {
		outcome := outcomes[i]
		if ctx.Err() == nil {
			if outcome.err == nil {
				c.health.Passed(host)
			} else {
				c.health.Failed(host, classifyArchiveProbeError(outcome.err))
			}
		}
		if outcome.err == nil {
			if activeMirror == "" || host == current {
				activeMirror = host
				activeHTTPStatus = outcome.httpStatus
			}
		} else if outcome.httpStatus > 0 {
			anyChallenged = true
			if firstChallengedStatus == 0 {
				firstChallengedStatus = outcome.httpStatus
			}
		}
	}

	if activeMirror != "" && ctx.Err() == nil {
		c.selector.SetCurrent(activeMirror)
	}

	summary := make([]MirrorHealth, len(hosts))
	for i, host := range hosts {
		outcome := outcomes[i]
		state := c.health.State(host)
		mirrorStatus := "unreachable"
		switch {
		case outcome.err == nil:
			mirrorStatus = "ok"
		case outcome.httpStatus > 0:
			mirrorStatus = "challenged"
		}
		summary[i] = MirrorHealth{
			Host:       host,
			Active:     host == activeMirror,
			Status:     mirrorStatus,
			HTTPStatus: outcome.httpStatus,
			Failures:   state.Failures,
			HeldUntil:  state.HeldUntil,
			LastOK:     state.LastOK,
			LastFail:   state.LastFail,
			Reason:     state.Reason,
			Blocked:    state.Blocked,
		}
	}

	if activeMirror != "" {
		return HealthStatus{
			ActiveMirror: activeMirror,
			Status:       "ok",
			HTTPStatus:   activeHTTPStatus,
			Mirrors:      summary,
		}
	}
	if anyChallenged {
		return HealthStatus{
			Status:     "challenged",
			HTTPStatus: firstChallengedStatus,
			Mirrors:    summary,
		}
	}
	return HealthStatus{
		Status:  "unreachable",
		Mirrors: summary,
	}
}

func (c *Client) baseURLFor(ctx context.Context) (string, error) {
	if c.baseURL == "" {
		return "", errNoMirrorConfigured
	}
	if err := c.ensureSession(ctx); err != nil {
		return "", err
	}
	if !c.autoBaseURL {
		return c.baseURL, nil
	}
	host, err := c.selector.Select(ctx)
	if err != nil {
		return "", mirror.UnavailableProviderError(err, "mirrors_blocked", "mirrors_unavailable")
	}
	return "https://" + host, nil
}

func (c *Client) onMirror(ctx context.Context, fn func(base string) error) error {
	base, err := c.baseURLFor(ctx)
	if err != nil {
		return err
	}
	return c.onMirrorFrom(ctx, base, fn)
}

func (c *Client) onMirrorFrom(ctx context.Context, base string, fn func(base string) error) error {
	err := fn(base)
	if err == nil {
		c.selector.RecordSuccess(base)
		return nil
	}
	host, tracked := c.selector.TrackedHost(base)
	if !tracked || ctx.Err() != nil {
		return err
	}
	_, refused := errors.AsType[*forbiddenError](err)
	if !refused && c.health.Eligible(host) {
		return err
	}
	next, selectErr := c.selector.Select(ctx, host)
	if selectErr != nil {
		return err
	}
	c.log.InfoContext(ctx, "retrying on another archive mirror", "failed_host", host, "next_host", next)
	if retryErr := fn("https://" + next); retryErr != nil {
		if !refused && !c.health.Eligible(next) {
			return fmt.Errorf("%w (before that, %w)", retryErr, err)
		}
		return retryErr
	}
	if refused {
		c.selector.RecordFailure(ctx, base, mirror.Failure{Reason: "refused access (HTTP 403)", Blocked: true})
	}
	c.selector.RecordSuccess("https://" + next)
	return nil
}
