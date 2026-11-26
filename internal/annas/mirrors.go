package annas

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bjoernaagaard/aakb/internal/httpx"
)

const (
	mirrorProbePath         = "/dyn/up/"
	mirrorStateFilename     = "mirrors.json"
	maxProbeBodyBytes       = 64 << 10
	maxMirrorStateFileBytes = 64 << 10

	mirrorBaseHold      = 15 * time.Second
	mirrorMaxHold       = 5 * time.Minute
	mirrorFreshFor      = 10 * time.Minute
	mirrorSweepInterval = 30 * time.Second
	mirrorCheckTimeout  = 5 * time.Second
	mirrorSaveEvery     = time.Minute
)

var (
	officialHosts = []string{"annas-archive.gl", "annas-archive.pk", "annas-archive.gd"}
	errNotUptime  = errors.New("did not answer with Anna's Archive uptime status")
	errAllFailed  = errors.New("every mirror failed its check")
)

// HostState records what recent requests and probes observed about one mirror.
type HostState struct {
	LastOK    time.Time `json:"last_ok,omitzero"`
	LastFail  time.Time `json:"last_fail,omitzero"`
	Failures  int       `json:"failures,omitempty"`
	HeldUntil time.Time `json:"held_until,omitzero"`
	Reason    string    `json:"reason,omitempty"`
	Blocked   bool      `json:"blocked,omitempty"`
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

// MirrorFailure describes one failed request or probe against a mirror.
type MirrorFailure struct {
	Reason     string
	Blocked    bool
	RetryAfter time.Duration
}

// MirrorFailureStatus summarizes one mirror's last failure for diagnostics.
type MirrorFailureStatus struct {
	Host    string
	Reason  string
	Ago     time.Duration
	Blocked bool
}

// UnavailableError reports that no mirror is currently eligible.
type UnavailableError struct {
	Mirrors []MirrorFailureStatus
	RetryIn time.Duration
}

// Error formats the unavailable mirror diagnostic summary.
func (e *UnavailableError) Error() string {
	parts := make([]string, 0, len(e.Mirrors))
	for _, m := range e.Mirrors {
		when := "just now"
		if ago := m.Ago.Round(time.Second); ago > 0 {
			when = ago.String() + " ago"
		}
		parts = append(parts, fmt.Sprintf("%s %s %s", m.Host, m.Reason, when))
	}
	message := "no Anna's Archive mirror is working"
	if len(parts) > 0 {
		message += ": " + strings.Join(parts, ", ")
	}
	if e.RetryIn > 0 {
		message += fmt.Sprintf(". Try again in %s", roundUpSecond(e.RetryIn))
	}
	return message
}

// Blocked reports whether every recorded mirror failure was a block/refusal.
func (e *UnavailableError) Blocked() bool {
	for _, m := range e.Mirrors {
		if !m.Blocked {
			return false
		}
	}
	return len(e.Mirrors) > 0
}

func roundUpSecond(d time.Duration) time.Duration {
	return ((d + time.Second - 1) / time.Second) * time.Second
}

// ParseBaseURL validates and canonicalizes a configured archive origin,
// rejecting non-HTTPS schemes, credentials, paths, queries, fragments, and
// language subdomains such as es.annas-archive.gl.
func ParseBaseURL(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil
	}
	if !strings.Contains(value, "://") {
		value = "https://" + value
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", httpx.WrapError(httpx.CodeConfig, "invalid_base_url", "invalid archive base URL", err)
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		return "", httpx.NewError(httpx.CodeConfig, "invalid_base_url", "base URL must use HTTPS")
	}
	if parsed.User != nil || parsed.Host == "" || parsed.Hostname() == "" {
		return "", httpx.NewError(httpx.CodeConfig, "invalid_base_url", "base URL must be an HTTPS origin without credentials")
	}
	if (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", httpx.NewError(httpx.CodeConfig, "invalid_base_url", "base URL must not include a path, query or fragment")
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if host == "" || strings.ContainsAny(host, "\\/\x00\r\n") {
		return "", httpx.NewError(httpx.CodeConfig, "invalid_base_url", "base URL has an invalid hostname")
	}
	if official, ok := languageSubdomain(host); ok {
		return "", httpx.NewError(
			httpx.CodeConfig,
			"language_subdomain",
			fmt.Sprintf("%s is a translated version of %s; use %s, since aakb reads Anna's Archive in English", host, official, official),
		)
	}
	if port := parsed.Port(); port != "" {
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		host += ":" + port
	}
	return host, nil
}

func languageSubdomain(host string) (string, bool) {
	label, rest, found := strings.Cut(host, ".")
	if !found || label == "www" || len(label) < 2 || len(label) > 3 || strings.Trim(label, "abcdefghijklmnopqrstuvwxyz") != "" {
		return "", false
	}
	if slices.Contains(officialHosts, rest) {
		return rest, true
	}
	return "", false
}

func normalizeTLDs(tlds []string) []string {
	var out []string
	for _, tld := range tlds {
		cleaned := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(tld), "."))
		if cleaned != "" && !slices.Contains(out, cleaned) {
			out = append(out, cleaned)
		}
	}
	return out
}

func hostAllowedByTLDs(host string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	u, err := url.Parse("https://" + host)
	if err != nil || u.Hostname() == "" {
		return false
	}
	hostname := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	idx := strings.LastIndex(hostname, ".")
	if idx < 0 || idx+1 >= len(hostname) {
		return false
	}
	return slices.Contains(allowed, hostname[idx+1:])
}

// buildCandidates builds the deduplicated list of archive mirror hosts from
// BaseURL, Mirrors, and official defaults, rejecting language subdomains and
// enforcing AllowedTLDs on every candidate before any host can receive credentials.
func buildCandidates(baseURL string, mirrors, allowedTLDs []string, autoBaseURL bool) ([]string, error) {
	allowed := normalizeTLDs(allowedTLDs)

	baseHost, err := ParseBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	if baseHost != "" && !hostAllowedByTLDs(baseHost, allowed) {
		return nil, httpx.NewError(httpx.CodeConfig, "tld_not_allowed", "BaseURL top-level domain is not in AllowedTLDs")
	}

	var extraHosts []string
	for _, raw := range mirrors {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		host, parseErr := ParseBaseURL(raw)
		if parseErr != nil {
			return nil, parseErr
		}
		if !hostAllowedByTLDs(host, allowed) {
			return nil, httpx.NewError(httpx.CodeConfig, "tld_not_allowed", "Mirrors contains a top-level domain outside AllowedTLDs")
		}
		if !slices.Contains(extraHosts, host) {
			extraHosts = append(extraHosts, host)
		}
	}

	if !autoBaseURL {
		primary := baseHost
		if primary == "" && len(extraHosts) > 0 {
			primary = extraHosts[0]
		}
		if primary == "" {
			primary = officialHosts[0]
			if !hostAllowedByTLDs(primary, allowed) {
				return nil, httpx.NewError(httpx.CodeConfig, "tld_not_allowed", "default archive mirror top-level domain is not in AllowedTLDs")
			}
		}
		return []string{primary}, nil
	}

	candidates := make([]string, 0, 1+len(extraHosts)+len(officialHosts))
	add := func(host string) {
		if host != "" && !slices.Contains(candidates, host) {
			candidates = append(candidates, host)
		}
	}
	add(baseHost)
	for _, host := range extraHosts {
		add(host)
	}
	for _, host := range officialHosts {
		if hostAllowedByTLDs(host, allowed) {
			add(host)
		}
	}
	if len(candidates) == 0 {
		return nil, httpx.NewError(httpx.CodeConfig, "tld_not_allowed", "no archive mirror matches AllowedTLDs")
	}
	return candidates, nil
}

type mirrorHealth struct {
	mu     sync.Mutex
	hosts  map[string]HostState
	now    func() time.Time
	jitter func(time.Duration) time.Duration
	store  *mirrorStore
	load   sync.Once
}

func newMirrorHealth() *mirrorHealth {
	return &mirrorHealth{
		hosts:  make(map[string]HostState),
		now:    time.Now,
		jitter: mirrorJitter,
	}
}

func mirrorJitter(hold time.Duration) time.Duration {
	// Spread holds by +/-10% so concurrent processes do not retry in lockstep.
	offset := (rand.Float64()*0.2 - 0.1) * float64(hold) //nolint:gosec // G404: backoff jitter is not security-sensitive
	return hold + time.Duration(offset)
}

func holdForFailures(failures int) time.Duration {
	hold := mirrorBaseHold
	for i := 1; i < failures && hold < mirrorMaxHold; i++ {
		hold *= 2
	}
	return min(hold, mirrorMaxHold)
}

func (h *mirrorHealth) Succeeded(host string) {
	h.update(host, func(s *HostState, now time.Time) {
		s.LastOK = now
		s.Failures = 0
		s.HeldUntil = time.Time{}
		s.Reason = ""
		s.Blocked = false
	})
}

func (h *mirrorHealth) Passed(host string) {
	h.update(host, func(s *HostState, now time.Time) {
		s.LastOK = now
		s.HeldUntil = time.Time{}
	})
}

func (h *mirrorHealth) Failed(host string, failure MirrorFailure) {
	h.update(host, func(s *HostState, now time.Time) {
		s.LastFail = now
		s.Reason = failure.Reason
		s.Blocked = failure.Blocked
		if now.Before(s.HeldUntil) && failure.RetryAfter <= 0 {
			return
		}
		s.Failures++
		hold := failure.RetryAfter
		if hold <= 0 {
			hold = h.jitter(holdForFailures(s.Failures))
		}
		s.HeldUntil = now.Add(min(hold, mirrorMaxHold))
	})
}

func (h *mirrorHealth) Eligible(host string) bool {
	h.ensureLoaded()
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.now().Before(h.hosts[host].HeldUntil)
}

func (h *mirrorHealth) State(host string) HostState {
	h.ensureLoaded()
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hosts[host]
}

func (h *mirrorHealth) update(host string, change func(*HostState, time.Time)) {
	h.ensureLoaded()
	h.mu.Lock()
	state := h.hosts[host]
	change(&state, h.now())
	h.hosts[host] = state
	h.mu.Unlock()
	if h.store != nil {
		h.store.changed(host, state)
	}
}

func (h *mirrorHealth) ensureLoaded() {
	if h.store != nil {
		h.load.Do(h.store.load)
	}
}

func (h *mirrorHealth) merge(states map[string]HostState) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for host, state := range states {
		if latestStateTime(state).After(latestStateTime(h.hosts[host])) {
			h.hosts[host] = state
		}
	}
}

func (h *mirrorHealth) snapshot() map[string]HostState {
	h.mu.Lock()
	defer h.mu.Unlock()
	return maps.Clone(h.hosts)
}

func latestStateTime(state HostState) time.Time {
	if state.LastOK.After(state.LastFail) {
		return state.LastOK
	}
	return state.LastFail
}

type mirrorStateFile struct {
	Mirrors map[string]HostState `json:"mirrors"`
}

type mirrorStore struct {
	cacheDir string
	path     string
	health   *mirrorHealth
	log      *slog.Logger

	mu     sync.Mutex
	saved  map[string]HostState
	warned bool
}

func attachMirrorStore(cacheDir string, health *mirrorHealth, logger *slog.Logger) {
	s := &mirrorStore{
		cacheDir: cacheDir,
		path:     filepath.Join(cacheDir, mirrorStateFilename),
		health:   health,
		log:      logger,
		saved:    make(map[string]HostState),
	}
	health.store = s
}

func (s *mirrorStore) load() {
	states := s.read()
	s.health.merge(states)
	s.mu.Lock()
	defer s.mu.Unlock()
	maps.Copy(s.saved, states)
}

func (s *mirrorStore) changed(host string, state HostState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	saved := s.saved[host]
	if state.HeldUntil.Equal(saved.HeldUntil) && state.LastOK.Sub(saved.LastOK) < mirrorSaveEvery {
		return
	}
	ours := s.health.snapshot()
	merged := s.read()
	for h, st := range ours {
		if latestStateTime(st).After(latestStateTime(merged[h])) {
			merged[h] = st
		}
	}
	if err := writePrivateJSON(s.cacheDir, s.path, ".mirrors-*.tmp", mirrorStateFile{Mirrors: merged}); err != nil {
		if !s.warned {
			s.warned = true
			s.log.Warn("could not save mirror health", "error", err)
		}
		return
	}
	maps.Copy(s.saved, ours)
}

func (s *mirrorStore) read() map[string]HostState {
	states := make(map[string]HostState)
	data, err := readPrivateFile(s.cacheDir, mirrorStateFilename, maxMirrorStateFileBytes)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			s.log.Warn("could not read mirror health", "error", err)
		}
		return states
	}
	var saved mirrorStateFile
	if len(data) == 0 || json.Unmarshal(data, &saved) != nil {
		return states
	}
	now := s.health.now()
	for host, state := range saved.Mirrors {
		last := latestStateTime(state)
		if last.Before(now.Add(-mirrorFreshFor)) || state.LastOK.After(now.Add(time.Minute)) || state.LastFail.After(now.Add(time.Minute)) {
			continue
		}
		if limit := state.LastFail.Add(mirrorMaxHold); state.HeldUntil.After(limit) {
			state.HeldUntil = limit
		}
		states[host] = state
	}
	return states
}

type probeStatusError struct {
	code int
}

func (e *probeStatusError) Error() string {
	return fmt.Sprintf("probe failed with status %d", e.code)
}

type mirrorProber struct {
	client *httpx.Client
	cookie func() string
}

func newMirrorProber(client *httpx.Client, cookie func() string) *mirrorProber {
	return &mirrorProber{
		client: client.WithOnRedirect(func(next *http.Request, via []*http.Request) error {
			if len(via) > 0 && !httpx.SameOrigin(via[len(via)-1].URL, next.URL) {
				return httpx.WrapError(httpx.CodeUpstream, "probe_cross_origin_redirect", "refusing cross-origin redirect during mirror probe", httpx.ErrRedirectRefused)
			}
			return nil
		}),
		cookie: cookie,
	}
}

func (p *mirrorProber) Check(ctx context.Context, host string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+mirrorProbePath, nil)
	if err != nil {
		return 0, err
	}
	if p.cookie != nil {
		if cookie := strings.TrimSpace(p.cookie()); cookie != "" {
			req.Header.Set("Cookie", cookie)
		}
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, err
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxProbeBodyBytes))
	closeErr := resp.Body.Close()
	if readErr != nil {
		return resp.StatusCode, readErr
	}
	if closeErr != nil {
		return resp.StatusCode, closeErr
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return resp.StatusCode, &probeStatusError{code: resp.StatusCode}
	}
	if !isUpAnswer(body) {
		return resp.StatusCode, errNotUptime
	}
	return resp.StatusCode, nil
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

type mirrorSelector struct {
	log    *slog.Logger
	hosts  []string
	health *mirrorHealth
	prober *mirrorProber

	mu        sync.Mutex
	current   string
	selecting chan struct{}
	lastRound time.Time
}

func newMirrorSelector(hosts []string, health *mirrorHealth, prober *mirrorProber, logger *slog.Logger) *mirrorSelector {
	return &mirrorSelector{
		log:    logger,
		hosts:  slices.Clone(hosts),
		health: health,
		prober: prober,
	}
}

func (s *mirrorSelector) Has(host string) bool {
	return slices.Contains(s.hosts, host)
}

func (s *mirrorSelector) Hosts() []string {
	return slices.Clone(s.hosts)
}

func (s *mirrorSelector) Current() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current
}

func (s *mirrorSelector) setCurrent(host string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current = host
}

func (s *mirrorSelector) Select(ctx context.Context, exclude ...string) (string, error) {
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		s.mu.Lock()
		if s.current != "" && s.usable(s.current, exclude) {
			host := s.current
			s.mu.Unlock()
			return host, nil
		}
		if pending := s.selecting; pending != nil {
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-pending:
				continue
			}
		}
		if host := s.freshest(exclude); host != "" {
			s.current = host
			s.mu.Unlock()
			s.log.InfoContext(ctx, "using mirror that worked recently", "host", host)
			return host, nil
		}
		candidates := s.eligible(exclude)
		if len(candidates) == 0 {
			if s.health.now().Sub(s.lastRound) < mirrorSweepInterval {
				err := s.unavailable()
				s.mu.Unlock()
				return "", err
			}
			candidates = slices.DeleteFunc(slices.Clone(s.hosts), func(host string) bool {
				return slices.Contains(exclude, host)
			})
			if len(candidates) == 0 {
				err := s.unavailable()
				s.mu.Unlock()
				return "", err
			}
		}
		pending := make(chan struct{})
		s.selecting = pending
		s.mu.Unlock()

		host, err := s.checkAll(ctx, candidates)

		s.mu.Lock()
		if ctx.Err() == nil {
			s.lastRound = s.health.now()
		}
		if host != "" {
			s.current = host
		}
		if errors.Is(err, errAllFailed) {
			err = s.unavailable()
		}
		s.selecting = nil
		close(pending)
		s.mu.Unlock()
		return host, err
	}
}

func (s *mirrorSelector) checkAll(ctx context.Context, candidates []string) (string, error) {
	checkCtx, cancel := context.WithTimeout(ctx, mirrorCheckTimeout)
	defer cancel()

	type probeOutcome struct {
		host string
		err  error
	}
	results := make(chan probeOutcome, len(candidates))
	for _, host := range candidates {
		go func(target string) {
			if err := checkCtx.Err(); err != nil {
				results <- probeOutcome{host: target, err: err}
				return
			}
			_, err := s.prober.Check(checkCtx, target)
			results <- probeOutcome{host: target, err: err}
		}(host)
	}

	for range candidates {
		checked := <-results
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if checked.err == nil {
			s.health.Passed(checked.host)
			s.log.InfoContext(ctx, "selected archive mirror", "host", checked.host)
			return checked.host, nil
		}
		s.health.Failed(checked.host, checkFailure(checked.err))
		s.log.WarnContext(ctx, "archive mirror failed probe", "host", checked.host, "error", checked.err)
	}
	return "", errAllFailed
}

func (s *mirrorSelector) usable(host string, exclude []string) bool {
	return !slices.Contains(exclude, host) && s.health.Eligible(host)
}

func (s *mirrorSelector) eligible(exclude []string) []string {
	var hosts []string
	for _, host := range s.hosts {
		if s.usable(host, exclude) {
			hosts = append(hosts, host)
		}
	}
	return hosts
}

func (s *mirrorSelector) freshest(exclude []string) string {
	best, bestOK := "", s.health.now().Add(-mirrorFreshFor)
	for _, host := range s.eligible(exclude) {
		if ok := s.health.State(host).LastOK; ok.After(bestOK) {
			best, bestOK = host, ok
		}
	}
	return best
}

func (s *mirrorSelector) unavailable() *UnavailableError {
	now := s.health.now()
	next := s.lastRound.Add(mirrorSweepInterval)
	if !next.After(now) {
		next = time.Time{}
	}
	err := &UnavailableError{}
	for _, host := range s.hosts {
		state := s.health.State(host)
		if state.HeldUntil.After(now) && (next.IsZero() || state.HeldUntil.Before(next)) {
			next = state.HeldUntil
		}
		if state.LastFail.IsZero() {
			continue
		}
		err.Mirrors = append(err.Mirrors, MirrorFailureStatus{
			Host:    host,
			Reason:  state.Reason,
			Ago:     now.Sub(state.LastFail),
			Blocked: state.Blocked,
		})
	}
	if !next.IsZero() {
		err.RetryIn = next.Sub(now)
	}
	return err
}

// ProbeHealth probes candidate archive mirrors in parallel via /dyn/up/ and
// returns the active mirror, overall status, HTTP status, and per-mirror summary.
func (c *Client) ProbeHealth(ctx context.Context) HealthStatus {
	hosts := c.selector.Hosts()
	if len(hosts) == 0 {
		return HealthStatus{Status: "unreachable"}
	}
	if ctx.Err() != nil {
		return HealthStatus{Status: "unreachable"}
	}

	probeCtx, cancel := context.WithTimeout(ctx, mirrorCheckTimeout)
	defer cancel()

	type probeResult struct {
		httpStatus int
		err        error
	}
	outcomes := make([]probeResult, len(hosts))
	var wg sync.WaitGroup
	for i, host := range hosts {
		wg.Go(func() {
			status, err := c.selector.prober.Check(probeCtx, host)
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
				c.health.Failed(host, checkFailure(outcome.err))
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
		c.selector.setCurrent(activeMirror)
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
	if err := c.ensureSession(ctx); err != nil {
		return "", err
	}
	if !c.autoBaseURL {
		return c.baseURL, nil
	}
	host, err := c.selector.Select(ctx)
	if err != nil {
		return "", mirrorUnavailableError(err)
	}
	return "https://" + host, nil
}

func mirrorUnavailableError(err error) error {
	unavailable, ok := errors.AsType[*UnavailableError](err)
	if !ok || unavailable == nil {
		return err
	}
	if unavailable.Blocked() {
		return httpx.WrapError(httpx.CodeUpstreamBlocked, "mirrors_blocked", unavailable.Error(), unavailable)
	}
	return httpx.WrapError(httpx.CodeUpstream, "mirrors_unavailable", unavailable.Error(), unavailable)
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
		c.mirrorSucceeded(base)
		return nil
	}
	host, tracked := c.trackedHost(base)
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
		c.mirrorFailed(ctx, base, MirrorFailure{Reason: "refused access (HTTP 403)", Blocked: true})
	}
	c.mirrorSucceeded("https://" + next)
	return nil
}

// archiveHost returns the normalized host when u belongs to a validated archive mirror.
// Only archive mirrors may receive the aa_account_id2 session cookie.
func (c *Client) archiveHost(u *url.URL) (string, bool) {
	if u == nil || !strings.EqualFold(u.Scheme, "https") || u.User != nil {
		return "", false
	}
	host := strings.ToLower(u.Host)
	return host, c.selector.Has(host)
}

func (c *Client) trackedHost(rawURL string) (string, bool) {
	if !c.autoBaseURL {
		return "", false
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", false
	}
	return c.archiveHost(u)
}

func (c *Client) mirrorSucceeded(rawURL string) {
	if host, ok := c.trackedHost(rawURL); ok {
		c.health.Succeeded(host)
	}
}

func (c *Client) mirrorFailed(ctx context.Context, rawURL string, failure MirrorFailure) {
	if host, ok := c.trackedHost(rawURL); ok {
		c.health.Failed(host, failure)
		c.log.WarnContext(ctx, "archive mirror failed", "host", host, "reason", failure.Reason)
	}
}

func responseFailure(resp *http.Response) *MirrorFailure {
	switch status := resp.StatusCode; {
	case status >= http.StatusInternalServerError:
		return &MirrorFailure{Reason: fmt.Sprintf("answered HTTP %d", status)}
	case status == http.StatusTooManyRequests:
		return &MirrorFailure{
			Reason:     "rate-limited requests (HTTP 429)",
			RetryAfter: retryAfterSeconds(resp.Header.Get("Retry-After")),
		}
	default:
		return nil
	}
}

func unreachableFailure(err error) MirrorFailure {
	if netErr, ok := errors.AsType[net.Error](err); ok && netErr.Timeout() {
		return MirrorFailure{Reason: "did not answer"}
	}
	return MirrorFailure{Reason: "could not be reached"}
}

func retryAfterSeconds(value string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

func checkFailure(err error) MirrorFailure {
	if statusErr, ok := errors.AsType[*probeStatusError](err); ok && statusErr != nil {
		return MirrorFailure{
			Reason:  fmt.Sprintf("answered its check with HTTP %d", statusErr.code),
			Blocked: statusErr.code == http.StatusForbidden,
		}
	}
	if errors.Is(err, errNotUptime) {
		return MirrorFailure{
			Reason:  "answered its check with a page that is not Anna's Archive",
			Blocked: true,
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return MirrorFailure{Reason: "did not answer its check"}
	}
	if netErr, ok := errors.AsType[net.Error](err); ok && netErr.Timeout() {
		return MirrorFailure{Reason: "did not answer its check"}
	}
	return MirrorFailure{Reason: "could not be reached"}
}

func readPrivateFile(dir, name string, limit int64) ([]byte, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }() //nolint:errcheck // closing read-only cache root

	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }() //nolint:errcheck // closing read-only private cache file

	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fs.ErrInvalid
	}
	return io.ReadAll(io.LimitReader(file, limit))
}

func writePrivateJSON(dir, targetPath, tempPattern string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, privateDirMode); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, tempPattern)
	if err != nil {
		return err
	}
	tempName := temp.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tempName) //nolint:errcheck // cleanup failed temporary file
		}
	}()
	if err := temp.Chmod(privateFileMode); err != nil {
		_ = temp.Close() //nolint:errcheck // already returning chmod error
		return err
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close() //nolint:errcheck // already returning write error
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close() //nolint:errcheck // already returning sync error
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempName, targetPath); err != nil {
		return err
	}
	renamed = true
	return nil
}
