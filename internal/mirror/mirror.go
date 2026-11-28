// Package mirror manages HTTPS mirror origin validation, health backoff,
// persistent cache state, parallel probing, and failover selection.
package mirror

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
	// CheckTimeout bounds one parallel mirror probe round.
	CheckTimeout = 5 * time.Second

	maxStateFileBytes = 64 << 10
	privateDirMode    = os.FileMode(0o700)
	privateFileMode   = os.FileMode(0o600)

	baseHold      = 15 * time.Second
	maxHold       = 5 * time.Minute
	freshFor      = 10 * time.Minute
	sweepInterval = 30 * time.Second
	saveEvery     = time.Minute
)

// ErrAllFailed indicates every candidate mirror failed its probe check.
var ErrAllFailed = errors.New("every mirror failed its check")

// HostState records what recent requests and probes observed about one mirror.
type HostState struct {
	LastOK    time.Time `json:"last_ok,omitzero"`
	LastFail  time.Time `json:"last_fail,omitzero"`
	Failures  int       `json:"failures,omitempty"`
	HeldUntil time.Time `json:"held_until,omitzero"`
	Reason    string    `json:"reason,omitempty"`
	Blocked   bool      `json:"blocked,omitempty"`
}

// Failure describes one failed request or probe against a mirror.
type Failure struct {
	Reason     string
	Blocked    bool
	RetryAfter time.Duration
}

// FailureStatus summarizes one mirror's last failure for diagnostics.
type FailureStatus struct {
	Host    string
	Reason  string
	Ago     time.Duration
	Blocked bool
}

// UnavailableError reports that no configured mirror is currently eligible.
type UnavailableError struct {
	Label   string
	Mirrors []FailureStatus
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
	label := e.Label
	if label == "" {
		label = "mirror"
	}
	message := "no " + label + " is working"
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

// OriginOptions controls how ParseOrigin and BuildCandidates validate and report invalid origins.
type OriginOptions struct {
	PublicOnly        bool
	InvalidReason     string
	InvalidPrefix     string
	ParseErrorMessage string
	BaseField         string
	MirrorsField      string
	AllowedTLDsField  string
}

// ParseOrigin validates and canonicalizes an HTTPS mirror origin, rejecting
// credentials, paths, queries, fragments, and non-HTTPS schemes.
func ParseOrigin(raw string, opts OriginOptions) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil
	}
	if !strings.Contains(value, "://") {
		value = "https://" + value
	}
	reason := opts.InvalidReason
	if reason == "" {
		reason = "invalid_base_url"
	}
	prefix := opts.InvalidPrefix
	if prefix == "" {
		prefix = "base URL"
	}
	parseMsg := opts.ParseErrorMessage
	if parseMsg == "" {
		parseMsg = "invalid " + prefix
	}

	parsed, err := url.Parse(value)
	if err != nil {
		return "", httpx.WrapError(httpx.CodeConfig, reason, parseMsg, err)
	}
	if opts.PublicOnly {
		if err := httpx.ValidatePublicHTTPSURL(parsed); err != nil {
			return "", httpx.NewError(httpx.CodeConfig, reason, prefix+" must be a public HTTPS origin without credentials")
		}
	} else {
		if !strings.EqualFold(parsed.Scheme, "https") {
			return "", httpx.NewError(httpx.CodeConfig, reason, prefix+" must use HTTPS")
		}
		if parsed.User != nil || parsed.Host == "" || parsed.Hostname() == "" {
			return "", httpx.NewError(httpx.CodeConfig, reason, prefix+" must be an HTTPS origin without credentials")
		}
	}
	if (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", httpx.NewError(httpx.CodeConfig, reason, prefix+" must not include a path, query or fragment")
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if host == "" || strings.ContainsAny(host, "\\/\x00\r\n") {
		return "", httpx.NewError(httpx.CodeConfig, reason, prefix+" has an invalid hostname")
	}
	if port := parsed.Port(); port != "" {
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		host += ":" + port
	}
	return host, nil
}

// NormalizeTLDs lowercases, trims leading dots, and deduplicates TLD values.
func NormalizeTLDs(tlds []string) []string {
	var out []string
	for _, tld := range tlds {
		cleaned := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(tld), "."))
		if cleaned != "" && !slices.Contains(out, cleaned) {
			out = append(out, cleaned)
		}
	}
	return out
}

// HostAllowedByTLDs reports whether host's top-level domain is permitted by allowed.
func HostAllowedByTLDs(host string, allowed []string) bool {
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

// CandidateConfig configures mirror host candidate resolution.
type CandidateConfig struct {
	BaseURL     string
	Mirrors     []string
	AllowedTLDs []string
	AutoBaseURL bool
	Origin      OriginOptions
}

// BuildCandidates validates, TLD-checks, and deduplicates configured mirror origins.
func BuildCandidates(cfg CandidateConfig) ([]string, error) {
	allowed := NormalizeTLDs(cfg.AllowedTLDs)

	baseHost, err := ParseOrigin(cfg.BaseURL, cfg.Origin)
	if err != nil {
		return nil, err
	}
	if baseHost != "" && !HostAllowedByTLDs(baseHost, allowed) {
		return nil, httpx.NewError(httpx.CodeConfig, "tld_not_allowed", cfg.Origin.BaseField+" top-level domain is not in "+cfg.Origin.AllowedTLDsField)
	}

	var extraHosts []string
	for _, raw := range cfg.Mirrors {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		host, parseErr := ParseOrigin(raw, cfg.Origin)
		if parseErr != nil {
			return nil, parseErr
		}
		if !HostAllowedByTLDs(host, allowed) {
			return nil, httpx.NewError(httpx.CodeConfig, "tld_not_allowed", cfg.Origin.MirrorsField+" contains a top-level domain outside "+cfg.Origin.AllowedTLDsField)
		}
		if !slices.Contains(extraHosts, host) {
			extraHosts = append(extraHosts, host)
		}
	}

	if !cfg.AutoBaseURL {
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

// Health tracks per-host backoff holds and recent probe/request outcomes.
// It is safe for concurrent use.
type Health struct {
	mu     sync.Mutex
	hosts  map[string]HostState
	now    func() time.Time
	jitter func(time.Duration) time.Duration
	store  *stateStore
	load   sync.Once
}

// NewHealth builds an in-memory mirror Health tracker.
func NewHealth() *Health {
	return &Health{
		hosts:  make(map[string]HostState),
		now:    time.Now,
		jitter: applyJitter,
	}
}

func applyJitter(hold time.Duration) time.Duration {
	// Spread holds by +/-10% so concurrent processes do not retry in lockstep.
	offset := (rand.Float64()*0.2 - 0.1) * float64(hold) //nolint:gosec // G404: backoff jitter is not security-sensitive
	return hold + time.Duration(offset)
}

func holdForFailures(failures int) time.Duration {
	hold := baseHold
	for i := 1; i < failures && hold < maxHold; i++ {
		hold *= 2
	}
	return min(hold, maxHold)
}

// Succeeded records a successful request to host and clears any backoff hold.
func (h *Health) Succeeded(host string) {
	h.update(host, func(s *HostState, now time.Time) {
		s.LastOK = now
		s.Failures = 0
		s.HeldUntil = time.Time{}
		s.Reason = ""
		s.Blocked = false
	})
}

// Passed records a successful probe check for host and lifts any active hold.
func (h *Health) Passed(host string) {
	h.update(host, func(s *HostState, now time.Time) {
		s.LastOK = now
		s.HeldUntil = time.Time{}
	})
}

// Failed records a failed request or probe for host and updates its backoff hold.
func (h *Health) Failed(host string, failure Failure) {
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
		s.HeldUntil = now.Add(min(hold, maxHold))
	})
}

// Eligible reports whether host is not currently in a backoff hold.
func (h *Health) Eligible(host string) bool {
	h.ensureLoaded()
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.now().Before(h.hosts[host].HeldUntil)
}

// State returns the latest recorded HostState for host.
func (h *Health) State(host string) HostState {
	h.ensureLoaded()
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hosts[host]
}

func (h *Health) update(host string, change func(*HostState, time.Time)) {
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

func (h *Health) ensureLoaded() {
	if h.store != nil {
		h.load.Do(h.store.load)
	}
}

func (h *Health) merge(states map[string]HostState) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for host, state := range states {
		if latestStateTime(state).After(latestStateTime(h.hosts[host])) {
			h.hosts[host] = state
		}
	}
}

func (h *Health) snapshot() map[string]HostState {
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

type stateFile struct {
	Mirrors map[string]HostState `json:"mirrors"`
}

// StoreConfig configures persistent mirror health storage in a cache directory.
type StoreConfig struct {
	CacheDir    string
	Filename    string
	TempPattern string
	LogLabel    string
	Logger      *slog.Logger
}

type stateStore struct {
	cacheDir    string
	filename    string
	tempPattern string
	path        string
	logLabel    string
	health      *Health
	log         *slog.Logger

	mu     sync.Mutex
	saved  map[string]HostState
	warned bool
}

// AttachStore attaches a persistent cache file to h.
func (h *Health) AttachStore(cfg StoreConfig) {
	if cfg.CacheDir == "" || cfg.Filename == "" {
		return
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	logLabel := cfg.LogLabel
	if logLabel == "" {
		logLabel = "mirror"
	}
	s := &stateStore{
		cacheDir:    cfg.CacheDir,
		filename:    cfg.Filename,
		tempPattern: cfg.TempPattern,
		path:        filepath.Join(cfg.CacheDir, cfg.Filename),
		logLabel:    logLabel,
		health:      h,
		log:         logger,
		saved:       make(map[string]HostState),
	}
	h.store = s
}

func (s *stateStore) load() {
	states := s.read()
	s.health.merge(states)
	s.mu.Lock()
	defer s.mu.Unlock()
	maps.Copy(s.saved, states)
}

func (s *stateStore) changed(host string, state HostState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	saved := s.saved[host]
	if state.HeldUntil.Equal(saved.HeldUntil) && state.LastOK.Sub(saved.LastOK) < saveEvery {
		return
	}
	ours := s.health.snapshot()
	merged := s.read()
	for h, st := range ours {
		if latestStateTime(st).After(latestStateTime(merged[h])) {
			merged[h] = st
		}
	}
	if err := WritePrivateJSON(s.cacheDir, s.path, s.tempPattern, stateFile{Mirrors: merged}); err != nil {
		if !s.warned {
			s.warned = true
			s.log.Warn("could not save mirror health", "pool", s.logLabel, "error", err)
		}
		return
	}
	maps.Copy(s.saved, ours)
}

func (s *stateStore) read() map[string]HostState {
	states := make(map[string]HostState)
	data, err := ReadPrivateFile(s.cacheDir, s.filename, maxStateFileBytes)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			s.log.Warn("could not read mirror health", "pool", s.logLabel, "error", err)
		}
		return states
	}
	var saved stateFile
	if len(data) == 0 || json.Unmarshal(data, &saved) != nil {
		return states
	}
	now := s.health.now()
	for host, state := range saved.Mirrors {
		last := latestStateTime(state)
		if last.Before(now.Add(-freshFor)) || state.LastOK.After(now.Add(time.Minute)) || state.LastFail.After(now.Add(time.Minute)) {
			continue
		}
		if limit := state.LastFail.Add(maxHold); state.HeldUntil.After(limit) {
			state.HeldUntil = limit
		}
		states[host] = state
	}
	return states
}

// ProbeStatusError records an unexpected HTTP status code from a mirror probe.
type ProbeStatusError struct {
	Code int
}

// Error formats the failed HTTP probe status.
func (e *ProbeStatusError) Error() string {
	return fmt.Sprintf("probe failed with status %d", e.Code)
}

// ProberConfig configures a mirror health Prober.
type ProberConfig struct {
	Client       *httpx.Client
	Path         string
	MaxBodyBytes int64
	Cookie       func() string
	Validate     func(body []byte) error
	Label        string
}

// Prober checks candidate mirrors over HTTPS with same-origin redirect enforcement.
type Prober struct {
	client       *httpx.Client
	path         string
	maxBodyBytes int64
	cookie       func() string
	validate     func(body []byte) error
}

// NewProber builds a Prober that refuses cross-origin redirects during probes.
func NewProber(cfg ProberConfig) *Prober {
	label := cfg.Label
	if label == "" {
		label = "mirror"
	}
	path := cfg.Path
	if path == "" {
		path = "/"
	}
	maxBytes := cfg.MaxBodyBytes
	if maxBytes <= 0 {
		maxBytes = 64 << 10
	}
	return &Prober{
		client: cfg.Client.WithOnRedirect(func(next *http.Request, via []*http.Request) error {
			if len(via) > 0 && !httpx.SameOrigin(via[len(via)-1].URL, next.URL) {
				return httpx.WrapError(httpx.CodeUpstream, "probe_cross_origin_redirect", "refusing cross-origin redirect during "+label+" probe", httpx.ErrRedirectRefused)
			}
			return nil
		}),
		path:         path,
		maxBodyBytes: maxBytes,
		cookie:       cfg.Cookie,
		validate:     cfg.Validate,
	}
}

// Check probes host and returns its HTTP status and any validation or transport error.
func (p *Prober) Check(ctx context.Context, host string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+p.path, nil)
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
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, p.maxBodyBytes))
	closeErr := resp.Body.Close()
	if readErr != nil {
		return resp.StatusCode, readErr
	}
	if closeErr != nil {
		return resp.StatusCode, closeErr
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return resp.StatusCode, &ProbeStatusError{Code: resp.StatusCode}
	}
	if p.validate != nil {
		if err := p.validate(body); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

// SelectorConfig configures a concurrent mirror Selector.
type SelectorConfig struct {
	Hosts              []string
	AutoBaseURL        bool
	Health             *Health
	Prober             *Prober
	ClassifyProbeError func(error) Failure
	Label              string
	Logger             *slog.Logger
}

// Selector chooses a healthy mirror using recent-success preference and
// singleflight parallel probes. It is safe for concurrent use.
type Selector struct {
	log              *slog.Logger
	hosts            []string
	autoBaseURL      bool
	health           *Health
	prober           *Prober
	classifyProbeErr func(error) Failure
	label            string

	mu        sync.Mutex
	current   string
	selecting chan struct{}
	lastRound time.Time
}

// NewSelector builds a Selector for the configured candidate hosts.
func NewSelector(cfg SelectorConfig) *Selector {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	label := cfg.Label
	if label == "" {
		label = "mirror"
	}
	classify := cfg.ClassifyProbeError
	if classify == nil {
		classify = func(err error) Failure {
			return ClassifyProbeFailure(err, nil)
		}
	}
	return &Selector{
		log:              logger,
		hosts:            slices.Clone(cfg.Hosts),
		autoBaseURL:      cfg.AutoBaseURL,
		health:           cfg.Health,
		prober:           cfg.Prober,
		classifyProbeErr: classify,
		label:            label,
	}
}

// Has reports whether host is one of the configured candidate mirrors.
func (s *Selector) Has(host string) bool {
	return slices.Contains(s.hosts, host)
}

// Hosts returns a copy of the configured candidate mirror hosts.
func (s *Selector) Hosts() []string {
	return slices.Clone(s.hosts)
}

// Current returns the currently active mirror host, or "" if none is selected.
func (s *Selector) Current() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current
}

// SetCurrent sets the active mirror host.
func (s *Selector) SetCurrent(host string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current = host
}

// MatchHost returns the normalized host when u targets a configured HTTPS mirror.
func (s *Selector) MatchHost(u *url.URL) (string, bool) {
	if u == nil || !strings.EqualFold(u.Scheme, "https") || u.User != nil {
		return "", false
	}
	host := strings.ToLower(u.Host)
	return host, s.Has(host)
}

// TrackedHost returns the normalized mirror host when failover tracking is enabled
// and rawURL belongs to a configured mirror.
func (s *Selector) TrackedHost(rawURL string) (string, bool) {
	if !s.autoBaseURL {
		return "", false
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", false
	}
	return s.MatchHost(u)
}

// RecordSuccess marks rawURL's mirror host as healthy when tracked.
func (s *Selector) RecordSuccess(rawURL string) {
	if host, ok := s.TrackedHost(rawURL); ok {
		s.health.Succeeded(host)
	}
}

// RecordFailure records a failure against rawURL's mirror host when tracked.
func (s *Selector) RecordFailure(ctx context.Context, rawURL string, failure Failure) {
	if host, ok := s.TrackedHost(rawURL); ok {
		s.health.Failed(host, failure)
		s.log.WarnContext(ctx, "mirror failed", "pool", s.label, "host", host, "reason", failure.Reason)
	}
}

// Select returns an eligible mirror host, probing candidates in parallel when needed.
func (s *Selector) Select(ctx context.Context, exclude ...string) (string, error) {
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
			s.log.InfoContext(ctx, "using mirror that worked recently", "pool", s.label, "host", host)
			return host, nil
		}
		candidates := s.eligible(exclude)
		if len(candidates) == 0 {
			if s.health.now().Sub(s.lastRound) < sweepInterval {
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
		if errors.Is(err, ErrAllFailed) {
			err = s.unavailable()
		}
		s.selecting = nil
		close(pending)
		s.mu.Unlock()
		return host, err
	}
}

func (s *Selector) checkAll(ctx context.Context, candidates []string) (string, error) {
	checkCtx, cancel := context.WithTimeout(ctx, CheckTimeout)
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
			s.log.InfoContext(ctx, "selected mirror", "pool", s.label, "host", checked.host)
			return checked.host, nil
		}
		s.health.Failed(checked.host, s.classifyProbeErr(checked.err))
		s.log.WarnContext(ctx, "mirror failed probe", "pool", s.label, "host", checked.host, "error", checked.err)
	}
	return "", ErrAllFailed
}

func (s *Selector) usable(host string, exclude []string) bool {
	return !slices.Contains(exclude, host) && s.health.Eligible(host)
}

func (s *Selector) eligible(exclude []string) []string {
	var hosts []string
	for _, host := range s.hosts {
		if s.usable(host, exclude) {
			hosts = append(hosts, host)
		}
	}
	return hosts
}

func (s *Selector) freshest(exclude []string) string {
	best, bestOK := "", s.health.now().Add(-freshFor)
	for _, host := range s.eligible(exclude) {
		if ok := s.health.State(host).LastOK; ok.After(bestOK) {
			best, bestOK = host, ok
		}
	}
	return best
}

func (s *Selector) unavailable() *UnavailableError {
	now := s.health.now()
	next := s.lastRound.Add(sweepInterval)
	if !next.After(now) {
		next = time.Time{}
	}
	err := &UnavailableError{Label: s.label}
	for _, host := range s.hosts {
		state := s.health.State(host)
		if state.HeldUntil.After(now) && (next.IsZero() || state.HeldUntil.Before(next)) {
			next = state.HeldUntil
		}
		if state.LastFail.IsZero() {
			continue
		}
		err.Mirrors = append(err.Mirrors, FailureStatus{
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

// UnavailableProviderError maps an UnavailableError to a structured ProviderError.
func UnavailableProviderError(err error, blockedReason, unavailableReason string) error {
	unavailable, ok := errors.AsType[*UnavailableError](err)
	if !ok || unavailable == nil {
		return err
	}
	if unavailable.Blocked() {
		return httpx.WrapError(httpx.CodeUpstreamBlocked, blockedReason, unavailable.Error(), unavailable)
	}
	return httpx.WrapError(httpx.CodeUpstream, unavailableReason, unavailable.Error(), unavailable)
}

// ResponseFailure classifies HTTP 5xx and 429 responses as mirror failures.
func ResponseFailure(resp *http.Response) *Failure {
	switch status := resp.StatusCode; {
	case status >= http.StatusInternalServerError:
		return &Failure{Reason: fmt.Sprintf("answered HTTP %d", status)}
	case status == http.StatusTooManyRequests:
		return &Failure{
			Reason:     "rate-limited requests (HTTP 429)",
			RetryAfter: RetryAfterSeconds(resp.Header.Get("Retry-After")),
		}
	default:
		return nil
	}
}

// UnreachableFailure classifies a transport error as a timeout or reachability failure.
func UnreachableFailure(err error) Failure {
	if netErr, ok := errors.AsType[net.Error](err); ok && netErr.Timeout() {
		return Failure{Reason: "did not answer"}
	}
	return Failure{Reason: "could not be reached"}
}

// RetryAfterSeconds parses a positive integer Retry-After header value in seconds.
func RetryAfterSeconds(value string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// ClassifyProbeFailure converts a probe error into a mirror Failure, consulting
// custom before applying standard status, timeout, and reachability classifications.
func ClassifyProbeFailure(err error, custom func(error) (Failure, bool)) Failure {
	if statusErr, ok := errors.AsType[*ProbeStatusError](err); ok && statusErr != nil {
		return Failure{
			Reason:  fmt.Sprintf("answered its check with HTTP %d", statusErr.Code),
			Blocked: statusErr.Code == http.StatusForbidden,
		}
	}
	if custom != nil {
		if failure, ok := custom(err); ok {
			return failure
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return Failure{Reason: "did not answer its check"}
	}
	if netErr, ok := errors.AsType[net.Error](err); ok && netErr.Timeout() {
		return Failure{Reason: "did not answer its check"}
	}
	return Failure{Reason: "could not be reached"}
}

// ReadPrivateFile reads up to limit bytes from a regular file inside dir using
// os.OpenRoot and O_NONBLOCK to avoid symlink traversal and FIFO hangs.
func ReadPrivateFile(dir, name string, limit int64) ([]byte, error) {
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

// WritePrivateJSON atomically writes payload as JSON with owner-only permissions.
func WritePrivateJSON(dir, targetPath, tempPattern string, payload any) error {
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
