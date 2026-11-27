package doimirror

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
	mirrorStateFilename     = "doi-mirrors.json"
	maxProbeBodyBytes       = 256 << 10
	maxMirrorStateFileBytes = 64 << 10

	mirrorBaseHold      = 15 * time.Second
	mirrorMaxHold       = 5 * time.Minute
	mirrorFreshFor      = 10 * time.Minute
	mirrorSweepInterval = 30 * time.Second
	mirrorCheckTimeout  = 5 * time.Second
	mirrorSaveEvery     = time.Minute
)

var (
	errMirrorChallenged   = errors.New("mirror returned an anti-bot or CAPTCHA challenge")
	errNotMirrorPage      = errors.New("did not answer with a DOI mirror page")
	errAllFailed          = errors.New("every DOI mirror failed its check")
	errNoMirrorConfigured = httpx.NewError(httpx.CodeConfig, "no_doi_mirror_configured", "set DOIMirrorBaseURL or DOIMirrors for DOI mirror access")
)

type hostState struct {
	LastOK    time.Time `json:"last_ok,omitzero"`
	LastFail  time.Time `json:"last_fail,omitzero"`
	Failures  int       `json:"failures,omitempty"`
	HeldUntil time.Time `json:"held_until,omitzero"`
	Reason    string    `json:"reason,omitempty"`
	Blocked   bool      `json:"blocked,omitempty"`
}

type mirrorFailure struct {
	Reason     string
	Blocked    bool
	RetryAfter time.Duration
}

type mirrorFailureStatus struct {
	Host    string
	Reason  string
	Ago     time.Duration
	Blocked bool
}

// UnavailableError reports that no configured DOI mirror is currently eligible.
type UnavailableError struct {
	Mirrors []mirrorFailureStatus
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
	message := "no DOI mirror is working"
	if len(parts) > 0 {
		message += ": " + strings.Join(parts, ", ")
	}
	if e.RetryIn > 0 {
		message += fmt.Sprintf(". Try again in %s", roundUpSecond(e.RetryIn))
	}
	return message
}

// Blocked reports whether every recorded mirror failure was a block or challenge.
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

// ParseBaseURL validates and canonicalizes a configured DOI mirror origin,
// rejecting non-HTTPS schemes, local/private addresses, credentials, paths,
// queries, and fragments.
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
		return "", httpx.WrapError(httpx.CodeConfig, "invalid_doi_base_url", "invalid DOI mirror base URL", err)
	}
	if err := httpx.ValidatePublicHTTPSURL(parsed); err != nil {
		return "", httpx.NewError(httpx.CodeConfig, "invalid_doi_base_url", "DOI mirror base URL must be a public HTTPS origin without credentials")
	}
	if (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", httpx.NewError(httpx.CodeConfig, "invalid_doi_base_url", "DOI mirror base URL must not include a path, query or fragment")
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if host == "" || strings.ContainsAny(host, "\\/\x00\r\n") {
		return "", httpx.NewError(httpx.CodeConfig, "invalid_doi_base_url", "DOI mirror base URL has an invalid hostname")
	}
	if port := parsed.Port(); port != "" {
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		host += ":" + port
	}
	return host, nil
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

func buildCandidates(baseURL string, mirrors, allowedTLDs []string, autoBaseURL bool) ([]string, error) {
	allowed := normalizeTLDs(allowedTLDs)

	baseHost, err := ParseBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	if baseHost != "" && !hostAllowedByTLDs(baseHost, allowed) {
		return nil, httpx.NewError(httpx.CodeConfig, "tld_not_allowed", "DOIMirrorBaseURL top-level domain is not in DOIMirrorAllowedTLDs")
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
			return nil, httpx.NewError(httpx.CodeConfig, "tld_not_allowed", "DOIMirrors contains a top-level domain outside DOIMirrorAllowedTLDs")
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
			return nil, nil
		}
		return []string{primary}, nil
	}

	candidates := make([]string, 0, 1+len(extraHosts))
	add := func(host string) {
		if host != "" && !slices.Contains(candidates, host) {
			candidates = append(candidates, host)
		}
	}
	add(baseHost)
	for _, host := range extraHosts {
		add(host)
	}
	return candidates, nil
}

type mirrorHealth struct {
	mu     sync.Mutex
	hosts  map[string]hostState
	now    func() time.Time
	jitter func(time.Duration) time.Duration
	store  *mirrorStore
	load   sync.Once
}

func newMirrorHealth() *mirrorHealth {
	return &mirrorHealth{
		hosts:  make(map[string]hostState),
		now:    time.Now,
		jitter: mirrorJitter,
	}
}

func mirrorJitter(hold time.Duration) time.Duration {
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
	h.update(host, func(s *hostState, now time.Time) {
		s.LastOK = now
		s.Failures = 0
		s.HeldUntil = time.Time{}
		s.Reason = ""
		s.Blocked = false
	})
}

func (h *mirrorHealth) Passed(host string) {
	h.update(host, func(s *hostState, now time.Time) {
		s.LastOK = now
		s.HeldUntil = time.Time{}
	})
}

func (h *mirrorHealth) Failed(host string, failure mirrorFailure) {
	h.update(host, func(s *hostState, now time.Time) {
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

func (h *mirrorHealth) State(host string) hostState {
	h.ensureLoaded()
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hosts[host]
}

func (h *mirrorHealth) update(host string, change func(*hostState, time.Time)) {
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

func (h *mirrorHealth) merge(states map[string]hostState) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for host, state := range states {
		if latestStateTime(state).After(latestStateTime(h.hosts[host])) {
			h.hosts[host] = state
		}
	}
}

func (h *mirrorHealth) snapshot() map[string]hostState {
	h.mu.Lock()
	defer h.mu.Unlock()
	return maps.Clone(h.hosts)
}

func latestStateTime(state hostState) time.Time {
	if state.LastOK.After(state.LastFail) {
		return state.LastOK
	}
	return state.LastFail
}

type mirrorStateFile struct {
	Mirrors map[string]hostState `json:"mirrors"`
}

type mirrorStore struct {
	cacheDir string
	path     string
	health   *mirrorHealth
	log      *slog.Logger

	mu     sync.Mutex
	saved  map[string]hostState
	warned bool
}

func attachMirrorStore(cacheDir string, health *mirrorHealth, logger *slog.Logger) {
	s := &mirrorStore{
		cacheDir: cacheDir,
		path:     filepath.Join(cacheDir, mirrorStateFilename),
		health:   health,
		log:      logger,
		saved:    make(map[string]hostState),
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

func (s *mirrorStore) changed(host string, state hostState) {
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
	if err := writePrivateJSON(s.cacheDir, s.path, ".doi-mirrors-*.tmp", mirrorStateFile{Mirrors: merged}); err != nil {
		if !s.warned {
			s.warned = true
			s.log.Warn("could not save DOI mirror health", "error", err)
		}
		return
	}
	maps.Copy(s.saved, ours)
}

func (s *mirrorStore) read() map[string]hostState {
	states := make(map[string]hostState)
	data, err := readPrivateFile(s.cacheDir, mirrorStateFilename, maxMirrorStateFileBytes)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			s.log.Warn("could not read DOI mirror health", "error", err)
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
	cookie string
}

func newMirrorProber(client *httpx.Client, cookie string) *mirrorProber {
	return &mirrorProber{
		client: client.WithOnRedirect(func(next *http.Request, via []*http.Request) error {
			if len(via) > 0 && !httpx.SameOrigin(via[len(via)-1].URL, next.URL) {
				return httpx.WrapError(httpx.CodeUpstream, "probe_cross_origin_redirect", "refusing cross-origin redirect during DOI mirror probe", httpx.ErrRedirectRefused)
			}
			return nil
		}),
		cookie: cookie,
	}
}

func (p *mirrorProber) Check(ctx context.Context, host string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+"/", nil)
	if err != nil {
		return 0, err
	}
	if p.cookie != "" {
		req.Header.Set("Cookie", p.cookie)
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
	if IsChallengeHTML(body) {
		return resp.StatusCode, errMirrorChallenged
	}
	if !looksLikeMirrorLanding(body) {
		return resp.StatusCode, errNotMirrorPage
	}
	return resp.StatusCode, nil
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
			s.log.InfoContext(ctx, "using DOI mirror that worked recently", "host", host)
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
			s.log.InfoContext(ctx, "selected DOI mirror", "host", checked.host)
			return checked.host, nil
		}
		s.health.Failed(checked.host, checkFailure(checked.err))
		s.log.WarnContext(ctx, "DOI mirror failed probe", "host", checked.host, "error", checked.err)
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
		err.Mirrors = append(err.Mirrors, mirrorFailureStatus{
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

func (c *Client) baseURLFor(ctx context.Context, exclude ...string) (string, error) {
	if c.baseURL == "" {
		return "", errNoMirrorConfigured
	}
	if !c.autoBaseURL {
		if len(exclude) > 0 {
			return "", errAllFailed
		}
		return c.baseURL, nil
	}
	host, err := c.selector.Select(ctx, exclude...)
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
		return httpx.WrapError(httpx.CodeUpstreamBlocked, "doi_mirrors_blocked", unavailable.Error(), unavailable)
	}
	return httpx.WrapError(httpx.CodeUpstream, "doi_mirrors_unavailable", unavailable.Error(), unavailable)
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
			c.mirrorSucceeded(base)
			return nil
		}
		lastErr = err
		host, tracked := c.trackedHost(base)
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

func (c *Client) mirrorHost(u *url.URL) (string, bool) {
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
	return c.mirrorHost(u)
}

func (c *Client) mirrorSucceeded(rawURL string) {
	if host, ok := c.trackedHost(rawURL); ok {
		c.health.Succeeded(host)
	}
}

func (c *Client) mirrorFailed(ctx context.Context, rawURL string, failure mirrorFailure) {
	if host, ok := c.trackedHost(rawURL); ok {
		c.health.Failed(host, failure)
		c.log.WarnContext(ctx, "DOI mirror failed", "host", host, "reason", failure.Reason)
	}
}

func responseFailure(resp *http.Response) *mirrorFailure {
	switch status := resp.StatusCode; {
	case status == http.StatusForbidden:
		return &mirrorFailure{Reason: "refused access (HTTP 403)", Blocked: true}
	case status >= http.StatusInternalServerError:
		return &mirrorFailure{Reason: fmt.Sprintf("answered HTTP %d", status)}
	case status == http.StatusTooManyRequests:
		return &mirrorFailure{
			Reason:     "rate-limited requests (HTTP 429)",
			RetryAfter: retryAfterSeconds(resp.Header.Get("Retry-After")),
		}
	default:
		return nil
	}
}

func unreachableFailure(err error) mirrorFailure {
	if netErr, ok := errors.AsType[net.Error](err); ok && netErr.Timeout() {
		return mirrorFailure{Reason: "did not answer"}
	}
	return mirrorFailure{Reason: "could not be reached"}
}

func retryAfterSeconds(value string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

func checkFailure(err error) mirrorFailure {
	if statusErr, ok := errors.AsType[*probeStatusError](err); ok && statusErr != nil {
		return mirrorFailure{
			Reason:  fmt.Sprintf("answered its check with HTTP %d", statusErr.code),
			Blocked: statusErr.code == http.StatusForbidden,
		}
	}
	if errors.Is(err, errMirrorChallenged) {
		return mirrorFailure{
			Reason:  "answered its check with an anti-bot challenge",
			Blocked: true,
		}
	}
	if errors.Is(err, errNotMirrorPage) {
		return mirrorFailure{
			Reason:  "answered its check with an unexpected page",
			Blocked: true,
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return mirrorFailure{Reason: "did not answer its check"}
	}
	if netErr, ok := errors.AsType[net.Error](err); ok && netErr.Timeout() {
		return mirrorFailure{Reason: "did not answer its check"}
	}
	return mirrorFailure{Reason: "could not be reached"}
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
