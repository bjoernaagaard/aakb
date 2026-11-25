package httpx

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// DefaultDialTimeout bounds TCP connection establishment.
	DefaultDialTimeout = 10 * time.Second
	// DefaultTLSHandshakeTimeout bounds TLS negotiation.
	DefaultTLSHandshakeTimeout = 10 * time.Second
	// DefaultResponseHeaderTimeout bounds waiting for response headers.
	DefaultResponseHeaderTimeout = 30 * time.Second
	// DefaultBodyIdleTimeout bounds inactivity while streaming a response body.
	DefaultBodyIdleTimeout = 60 * time.Second
	// DefaultMaxRedirects is the maximum number of HTTP redirects followed.
	DefaultMaxRedirects = 10
	// DefaultMaxBodyBytes caps HTML and standard JSON responses at 8 MiB.
	DefaultMaxBodyBytes int64 = 8 << 20
	// OpenAlexMaxBodyBytes caps OpenAlex JSON responses at 16 MiB.
	OpenAlexMaxBodyBytes int64 = 16 << 20
)

// TransportOptions configures the shared HTTPS http.Transport.
type TransportOptions struct {
	DialTimeout           time.Duration
	TLSHandshakeTimeout   time.Duration
	ResponseHeaderTimeout time.Duration
	PublicOnly            bool
}

// NewTransport constructs an HTTPS transport with bounded dial, TLS, and header timeouts.
func NewTransport(opts TransportOptions) *http.Transport {
	dialTimeout := opts.DialTimeout
	if dialTimeout <= 0 {
		dialTimeout = DefaultDialTimeout
	}
	tlsTimeout := opts.TLSHandshakeTimeout
	if tlsTimeout <= 0 {
		tlsTimeout = DefaultTLSHandshakeTimeout
	}
	headerTimeout := opts.ResponseHeaderTimeout
	if headerTimeout <= 0 {
		headerTimeout = DefaultResponseHeaderTimeout
	}

	dialer := &net.Dialer{
		Timeout:   dialTimeout,
		KeepAlive: 30 * time.Second,
	}
	dialContext := dialer.DialContext
	if opts.PublicOnly {
		dialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			return publicDial(ctx, dialer, network, address)
		}
	}

	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialContext,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   tlsTimeout,
		ResponseHeaderTimeout: headerTimeout,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   16,
	}
}

// ValidateHTTPSURL ensures u is an HTTPS URL with a host and no embedded credentials.
func ValidateHTTPSURL(u *url.URL) error {
	if u == nil || !strings.EqualFold(u.Scheme, "https") || u.Host == "" || u.User != nil {
		return ErrUnsafeURL
	}
	return nil
}

// ValidatePublicHTTPSURL ensures u is an HTTPS URL that does not target a local or private host.
func ValidatePublicHTTPSURL(u *url.URL) error {
	if err := ValidateHTTPSURL(u); err != nil {
		return err
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return ErrUnsafeURL
	}
	if ip, err := netip.ParseAddr(host); err == nil && !isPublicIP(ip) {
		return ErrUnsafeURL
	}
	return nil
}

// SameOrigin reports whether two URLs share the same scheme and normalized host:port.
func SameOrigin(a, b *url.URL) bool {
	if a == nil || b == nil {
		return false
	}
	if !strings.EqualFold(a.Scheme, b.Scheme) {
		return false
	}
	return strings.EqualFold(normalizedHostPort(a), normalizedHostPort(b))
}

func normalizedHostPort(u *url.URL) string {
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		if strings.EqualFold(u.Scheme, "https") {
			port = "443"
		} else if strings.EqualFold(u.Scheme, "http") {
			port = "80"
		}
	}
	if port == "" {
		return host
	}
	return net.JoinHostPort(host, port)
}

// ClientOptions configures a guarded HTTPS Client.
type ClientOptions struct {
	BaseClient      *http.Client
	UserAgent       string
	Secrets         []string
	BodyIdleTimeout time.Duration
	MaxRedirects    int
	PublicOnly      bool
	OnRedirect      func(req *http.Request, via []*http.Request) error
}

// Client executes HTTPS requests with redirect guards, body-idle timeouts,
// credential redaction, and idempotent retries.
type Client struct {
	http       *http.Client
	userAgent  string
	redactor   *Redactor
	publicOnly bool
	onRedirect func(req *http.Request, via []*http.Request) error
	maxRedirs  int
	baseCheck  func(req *http.Request, via []*http.Request) error
}

// NewClient creates a guarded HTTPS Client.
func NewClient(opts ClientOptions) *Client {
	idleTimeout := opts.BodyIdleTimeout
	if idleTimeout <= 0 {
		idleTimeout = DefaultBodyIdleTimeout
	}
	maxRedirects := opts.MaxRedirects
	if maxRedirects <= 0 {
		maxRedirects = DefaultMaxRedirects
	}

	var base http.Client
	var baseCheck func(req *http.Request, via []*http.Request) error
	if opts.BaseClient != nil {
		base = *opts.BaseClient
		baseCheck = opts.BaseClient.CheckRedirect
	}
	// Never inherit a CookieJar that could bypass our origin-scoped cookie policy.
	base.Jar = nil

	transport := base.Transport
	if transport == nil {
		transport = NewTransport(TransportOptions{PublicOnly: opts.PublicOnly})
	}
	base.Transport = &idleTimeoutRoundTripper{
		base:        transport,
		idleTimeout: idleTimeout,
	}

	c := &Client{
		http:       &base,
		userAgent:  strings.TrimSpace(opts.UserAgent),
		redactor:   NewRedactor(opts.Secrets...),
		publicOnly: opts.PublicOnly,
		onRedirect: opts.OnRedirect,
		maxRedirs:  maxRedirects,
		baseCheck:  baseCheck,
	}
	c.http.CheckRedirect = c.checkRedirect
	return c
}

// WithOnRedirect returns a shallow copy of Client with an additional redirect hook.
func (c *Client) WithOnRedirect(fn func(req *http.Request, via []*http.Request) error) *Client {
	copied := *c
	httpCopy := *c.http
	copied.http = &httpCopy
	copied.onRedirect = fn
	copied.http.CheckRedirect = copied.checkRedirect
	return &copied
}

// Redactor returns the Client's secret redactor.
func (c *Client) Redactor() *Redactor {
	return c.redactor
}

// HTTPClient returns a copy of the underlying *http.Client with all guards active.
func (c *Client) HTTPClient() *http.Client {
	copied := *c.http
	return &copied
}

// Do validates the request URL, sets default headers, runs the request, and redacts any error.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil {
		return nil, NewError(CodeInvalidArgument, "nil_request", "HTTP request is nil")
	}
	if err := c.validateRequestURL(req.URL); err != nil {
		return nil, c.redactor.WrapError(CodeInvalidArgument, "unsafe_url", "upstream URL must use HTTPS without credentials", err)
	}
	if c.userAgent != "" && req.Header.Get("User-Agent") == "" {
		req = req.Clone(req.Context())
		req.Header.Set("User-Agent", c.userAgent)
	}
	resp, err := c.http.Do(req) //nolint:gosec // G704: request URL is validated by validateRequestURL and CheckRedirect
	if err != nil {
		return nil, c.redactor.RedactError(err)
	}
	return resp, nil
}

func (c *Client) validateRequestURL(u *url.URL) error {
	if c.publicOnly {
		return ValidatePublicHTTPSURL(u)
	}
	return ValidateHTTPSURL(u)
}

func (c *Client) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= c.maxRedirs {
		return WrapError(CodeUpstream, "too_many_redirects", "upstream exceeded the redirect limit", ErrRedirectRefused)
	}
	if err := c.validateRequestURL(req.URL); err != nil {
		return WrapError(CodeUpstream, "unsafe_redirect", "refused an unsafe upstream redirect", errors.Join(ErrRedirectRefused, err))
	}
	if len(via) > 0 {
		first := via[0]
		prev := via[len(via)-1]
		crossOrigin := !SameOrigin(req.URL, first.URL) || !SameOrigin(req.URL, prev.URL)
		if crossOrigin {
			req.Header.Del("Cookie")
			req.Header.Del("Authorization")
			for _, prior := range via {
				if prior.URL != nil && prior.URL.Query().Has("key") {
					return WrapError(CodeUpstream, "key_redirect_refused", "refused to redirect an authenticated request to another origin", ErrRedirectRefused)
				}
			}
		}
	}
	if c.onRedirect != nil {
		if err := c.onRedirect(req, via); err != nil {
			return err
		}
	}
	if c.baseCheck != nil {
		return c.baseCheck(req, via)
	}
	return nil
}

type idleTimeoutRoundTripper struct {
	base        http.RoundTripper
	idleTimeout time.Duration
}

func (rt *idleTimeoutRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if rt.idleTimeout <= 0 {
		return rt.base.RoundTrip(req)
	}
	parentCtx := req.Context()
	reqCtx, cancel := context.WithCancelCause(parentCtx)
	cloned := req.Clone(reqCtx)

	resp, err := rt.base.RoundTrip(cloned)
	if err != nil {
		cancel(nil)
		return nil, err
	}
	if resp.Body == nil {
		cancel(nil)
		return resp, nil
	}

	body := &idleTimeoutBody{
		body:        resp.Body,
		cancel:      cancel,
		idleTimeout: rt.idleTimeout,
	}
	body.timer = time.AfterFunc(rt.idleTimeout, func() {
		body.timedOut.Store(true)
		cancel(ErrBodyIdleTimeout)
	})
	resp.Body = body
	return resp, nil
}

type idleTimeoutBody struct {
	body        io.ReadCloser
	cancel      context.CancelCauseFunc
	idleTimeout time.Duration
	timer       *time.Timer
	timedOut    atomic.Bool
	closeOnce   sync.Once
}

func (b *idleTimeoutBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	if n > 0 {
		b.timer.Reset(b.idleTimeout)
	}
	if err != nil {
		b.timer.Stop()
		if !errors.Is(err, io.EOF) && b.timedOut.Load() {
			return n, ErrBodyIdleTimeout
		}
	}
	return n, err
}

func (b *idleTimeoutBody) Close() error {
	var closeErr error
	b.closeOnce.Do(func() {
		b.timer.Stop()
		closeErr = b.body.Close()
		b.cancel(nil)
	})
	return closeErr
}

var reservedNetworks = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001:db8::/32"),
}

func isPublicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, prefix := range reservedNetworks {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

func publicDial(ctx context.Context, dialer *net.Dialer, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	for _, ip := range addrs {
		if !isPublicIP(ip) {
			return nil, ErrUnsafeURL
		}
	}
	var dialErr error
	for _, ip := range addrs {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		dialErr = err
	}
	if dialErr == nil {
		return nil, errors.New("host resolved to no public addresses")
	}
	return nil, dialErr
}
