package archive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/bjoernaagaard/aakb/internal/httpx"
)

const (
	accountCookieName   = "aa_account_id2"
	sessionFilename     = "session.json"
	signInRetryInterval = time.Minute
	maxSignInBodyBytes  = 1 << 20
	maxSessionFileBytes = 64 << 10

	manualCookieSteps = "in a browser signed in to the archive, open developer tools -> Application (Chrome, Edge) or Storage (Firefox, Safari) -> Cookies and copy the aa_account_id2 value into AccountCookie"
)

type savedSession struct {
	KeySHA256      string `json:"key_sha256"`
	Cookie         string `json:"cookie"`
	ReplacedSHA256 string `json:"replaced_cookie_sha256,omitempty"`
}

// forbiddenError represents an HTTP 403 that a renewed session did not resolve.
type forbiddenError struct {
	message string
}

func (e *forbiddenError) Error() string { return e.message }

func (e *forbiddenError) Unwrap() error {
	return httpx.NewError(httpx.CodeUpstreamBlocked, "forbidden", e.message)
}

// NormalizeAccountCookie validates either a raw aa_account_id2 value (including
// base64 padding) or a Cookie header containing aa_account_id2.
func NormalizeAccountCookie(value string) (string, error) {
	raw := strings.Trim(strings.TrimSpace(value), `"'`)
	if raw == "" {
		return "", nil
	}
	if hasInvalidCookieChars(raw) {
		return "", httpx.NewError(httpx.CodeConfig, "invalid_cookie", "AccountCookie contains invalid cookie characters")
	}
	if strings.ContainsRune(raw, ';') {
		extracted, ok := cookieValue(raw, accountCookieName)
		if !ok {
			return "", httpx.NewError(httpx.CodeConfig, "invalid_cookie", "AccountCookie must be a raw aa_account_id2 value or a Cookie header containing aa_account_id2")
		}
		return accountCookieName + "=" + extracted, nil
	}
	if trimmed, ok := strings.CutPrefix(raw, accountCookieName+"="); ok {
		if !validCookieValue(trimmed) {
			return "", httpx.NewError(httpx.CodeConfig, "invalid_cookie", "AccountCookie contains an empty or invalid aa_account_id2 value")
		}
		return accountCookieName + "=" + trimmed, nil
	}
	if strings.ContainsRune(raw, '=') && !isPaddedRawCookieValue(raw) {
		return "", httpx.NewError(httpx.CodeConfig, "invalid_cookie", "AccountCookie must be a raw aa_account_id2 value or a Cookie header containing aa_account_id2")
	}
	if !validCookieValue(raw) {
		return "", httpx.NewError(httpx.CodeConfig, "invalid_cookie", "AccountCookie contains an empty or invalid aa_account_id2 value")
	}
	return accountCookieName + "=" + raw, nil
}

func cookieValue(header, name string) (string, bool) {
	if !strings.Contains(header, name+"=") {
		return "", false
	}
	for part := range strings.SplitSeq(header, ";") {
		key, val, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && strings.TrimSpace(key) == name {
			val = strings.TrimSpace(val)
			if !validCookieValue(val) {
				return "", false
			}
			return val, true
		}
	}
	return "", false
}

func hasInvalidCookieChars(value string) bool {
	return strings.IndexFunc(value, func(r rune) bool {
		return r < 0x20 || r == 0x7f
	}) >= 0
}

func validCookieValue(value string) bool {
	if value == "" {
		return false
	}
	for i := range len(value) {
		ch := value[i]
		if ch < 0x21 || ch > 0x7e || ch == '"' || ch == ',' || ch == ';' || ch == '\\' {
			return false
		}
	}
	return true
}

func isPaddedRawCookieValue(value string) bool {
	payload := strings.TrimRight(value, "=")
	padding := len(value) - len(payload)
	if padding == 0 || padding > 2 || payload == "" || strings.ContainsRune(payload, '=') {
		return false
	}
	for _, r := range payload {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') &&
			r != '+' && r != '/' && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

func secretFingerprint(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func (c *Client) restoreSession() {
	saved, ok := c.loadSession()
	if !ok {
		return
	}
	switch {
	case c.configuredCookie == "":
		c.cookie = saved.Cookie
	case saved.ReplacedSHA256 == secretFingerprint(c.configuredCookie):
		c.cookie = saved.Cookie
		c.log.Warn("configured account cookie was previously rejected; using renewed session")
	}
}

func (c *Client) loadSession() (savedSession, bool) {
	if c.cacheDir == "" || c.secretKey == "" {
		return savedSession{}, false
	}
	data, err := readPrivateFile(c.cacheDir, sessionFilename, maxSessionFileBytes)
	if err != nil {
		return savedSession{}, false
	}
	var saved savedSession
	if err := json.Unmarshal(data, &saved); err != nil {
		return savedSession{}, false
	}
	normalized, err := NormalizeAccountCookie(saved.Cookie)
	if err != nil || saved.KeySHA256 != secretFingerprint(c.secretKey) ||
		!strings.HasPrefix(saved.Cookie, accountCookieName+"=") || normalized != saved.Cookie {
		return savedSession{}, false
	}
	return saved, true
}

func (c *Client) saveSession(ctx context.Context, cookie string) {
	if c.cacheDir == "" || c.secretKey == "" {
		return
	}
	saved := savedSession{
		KeySHA256: secretFingerprint(c.secretKey),
		Cookie:    cookie,
	}
	if c.configuredCookie != "" {
		saved.ReplacedSHA256 = secretFingerprint(c.configuredCookie)
	}
	path := filepath.Join(c.cacheDir, sessionFilename)
	if err := writePrivateJSON(c.cacheDir, path, ".session-*.tmp", saved); err != nil {
		c.log.WarnContext(ctx, "could not persist archive session", "error", err)
	}
}

func (c *Client) sessionCookie() string {
	c.sessionMu.Lock()
	defer c.sessionMu.Unlock()
	return c.cookie
}

func (c *Client) ensureSession(ctx context.Context) error {
	if c.sessionCookie() != "" {
		return nil
	}
	_, err := c.signIn(ctx, "")
	return err
}

// signIn replaces a missing or rejected session via singleflight POST /account/
// and reports whether the caller should retry its request with the new cookie.
func (c *Client) signIn(ctx context.Context, rejected string) (bool, error) {
	if c.secretKey == "" {
		return false, nil
	}
	for {
		c.sessionMu.Lock()
		if c.cookie != rejected {
			c.sessionMu.Unlock()
			return true, nil
		}
		if pending := c.signingIn; pending != nil {
			c.sessionMu.Unlock()
			select {
			case <-ctx.Done():
				return false, ctx.Err()
			case <-pending:
				continue
			}
		}
		if !c.signedInAt.IsZero() && time.Since(c.signedInAt) < signInRetryInterval {
			err := c.signInErr
			c.sessionMu.Unlock()
			return false, err
		}
		pending := make(chan struct{})
		c.signingIn = pending
		c.sessionMu.Unlock()

		cookie, err := c.signInAnyMirror(ctx)

		c.sessionMu.Lock()
		adopted := err == nil && ctx.Err() == nil
		if ctx.Err() == nil {
			if adopted {
				c.cookie = cookie
			}
			pe, isPE := errors.AsType[*httpx.ProviderError](err)
			if err == nil || (isPE && (pe.Code == httpx.CodeConfig || pe.Code == httpx.CodeUpstreamBlocked)) {
				c.signedInAt = time.Now()
				c.signInErr = err
			}
		}
		c.signingIn = nil
		close(pending)
		c.sessionMu.Unlock()

		if adopted {
			c.saveSession(ctx, cookie)
		}
		return err == nil, err
	}
}

func (c *Client) signInAnyMirror(ctx context.Context) (string, error) {
	origins, err := c.signInOrigins(ctx)
	if err != nil {
		return "", err
	}
	var reasons []string
	code := httpx.CodeUpstream
	for _, origin := range origins {
		cookie, err := c.signInAt(ctx, origin)
		if err == nil {
			return cookie, nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if pe, ok := errors.AsType[*httpx.ProviderError](err); ok && pe != nil {
			if pe.Code == httpx.CodeConfig {
				return "", pe
			}
			if pe.Code == httpx.CodeUpstreamBlocked {
				code = httpx.CodeUpstreamBlocked
			}
		}
		reasons = append(reasons, err.Error())
	}
	safe := "automatic sign-in with SecretKey failed (" + strings.Join(reasons, "; ") + "). If the archive works in your browser, set the cookie by hand: " + manualCookieSteps
	return "", httpx.NewError(code, "signin_failed", c.http.Redactor().RedactString(safe))
}

func (c *Client) signInOrigins(ctx context.Context) ([]string, error) {
	if !c.autoBaseURL {
		return []string{c.baseURL}, nil
	}
	var origins []string
	candidates := append([]string{c.selector.Current()}, c.selector.Hosts()...)
	for _, host := range candidates {
		if host == "" || !c.health.Eligible(host) {
			continue
		}
		origin := "https://" + host
		if !slices.Contains(origins, origin) {
			origins = append(origins, origin)
		}
	}
	if len(origins) > 0 {
		return origins, nil
	}
	host, err := c.selector.Select(ctx)
	if err != nil {
		return nil, mirrorUnavailableError(err)
	}
	return []string{"https://" + host}, nil
}

func (c *Client) signInAt(ctx context.Context, origin string) (string, error) {
	parent := ctx
	signCtx, cancel := context.WithTimeout(ctx, c.signInTimeout)
	defer cancel()

	accountURL := origin + "/account/"
	form := url.Values{"key": {c.secretKey}}
	req, err := http.NewRequestWithContext(signCtx, http.MethodPost, accountURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", c.http.Redactor().WrapError(httpx.CodeInternal, "signin_request", "could not construct sign-in request", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	noFollow := c.http.WithOnRedirect(func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	})
	resp, err := noFollow.Do(req)
	if err != nil {
		if parent.Err() != nil {
			return "", parent.Err()
		}
		c.mirrorFailed(parent, origin, unreachableFailure(err))
		if signCtx.Err() != nil {
			return "", httpx.NewError(httpx.CodeTimeout, "signin_timeout", fmt.Sprintf("%s did not answer the sign-in form within %s", requestHost(origin), c.signInTimeout))
		}
		return "", c.requestFailure(err, accountURL)
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxSignInBodyBytes))
	_ = resp.Body.Close() //nolint:errcheck // reading sign-in response body is complete

	if failure := responseFailure(resp); failure != nil {
		c.mirrorFailed(parent, origin, *failure)
	}
	for _, ck := range resp.Cookies() {
		if ck.Name == accountCookieName && ck.Value != "" && ck.MaxAge >= 0 && validCookieValue(ck.Value) {
			return accountCookieName + "=" + ck.Value, nil
		}
	}
	host := responseHost(resp, accountURL)
	if readErr == nil && bytes.Contains(body, []byte("Invalid secret key")) {
		return "", httpx.NewError(httpx.CodeConfig, "invalid_secret_key", fmt.Sprintf("The archive rejected SecretKey. Copy the secret key from https://%s/account/ into SecretKey", host))
	}
	if resp.StatusCode == http.StatusForbidden && readErr == nil && bytes.Contains(body, []byte("DDoS-Guard")) {
		return "", httpx.NewError(httpx.CodeUpstreamBlocked, "ddos_guard", host+" put the sign-in form behind a DDoS-Guard browser check")
	}
	return "", httpx.NewError(httpx.CodeUpstreamBlocked, "signin_no_session", fmt.Sprintf("%s answered the sign-in form with HTTP status %d but no session", host, resp.StatusCode))
}

func (c *Client) doGet(ctx context.Context, rawURL string) (*http.Response, error) {
	return c.doGetWith(ctx, rawURL, nil)
}

func (c *Client) doGetWith(ctx context.Context, rawURL string, extraHeaders http.Header) (*http.Response, error) {
	return c.doGetWithClient(ctx, rawURL, extraHeaders, c.http)
}

// doGetWithClient is doGetWith over an explicit guarded client, so callers
// with their own redirect policy still get the session cookie, session
// renewal, mirror failure tracking, and credential-safe errors.
func (c *Client) doGetWithClient(ctx context.Context, rawURL string, extraHeaders http.Header, client *httpx.Client) (*http.Response, error) {
	for renewed := false; ; renewed = true {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, client.Redactor().WrapError(httpx.CodeInvalidArgument, "invalid_url", "invalid upstream URL", err)
		}
		for key, values := range extraHeaders {
			req.Header[key] = slices.Clone(values)
		}
		_, isArchive := c.archiveHost(req.URL)
		cookie := ""
		if isArchive {
			cookie = c.sessionCookie()
			if cookie != "" {
				req.Header.Set("Cookie", cookie)
			}
		}

		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() == nil {
				c.mirrorFailed(ctx, rawURL, unreachableFailure(err))
			}
			return nil, c.requestFailure(err, rawURL)
		}

		// Renew session once on HTTP 403 for archive HTML/JSON routes,
		// excluding ?key= fast download API routes that authenticate by key.
		if isArchive && !renewed && resp.StatusCode == http.StatusForbidden && !req.URL.Query().Has("key") {
			retry, signInErr := c.signIn(ctx, cookie)
			if signInErr != nil || retry {
				httpx.DrainAndClose(resp.Body)
				if signInErr != nil {
					return nil, signInErr
				}
				continue
			}
		}

		if failure := responseFailure(resp); failure != nil {
			c.mirrorFailed(ctx, rawURL, *failure)
		}
		return resp, nil
	}
}

func (c *Client) fetchJSON(ctx context.Context, rawURL string, target any, what string) error {
	return c.fetchJSONWithHeaders(ctx, rawURL, target, what, nil)
}

func (c *Client) fetchJSONWithHeaders(ctx context.Context, rawURL string, target any, what string, headers http.Header) error {
	resp, err := c.doGetWith(ctx, rawURL, headers)
	if err != nil {
		return err
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		httpx.DrainAndClose(resp.Body)
		return httpx.NewError(httpx.CodeNotFound, "not_found", what+" not found")
	case http.StatusForbidden:
		httpx.DrainAndClose(resp.Body)
		return &forbiddenError{message: "The archive refused " + what + "; it shows them to signed-in members only, so check that your membership is active"}
	default:
		httpx.DrainAndClose(resp.Body)
		return httpResponseError(resp, rawURL)
	}

	body, readErr := httpx.ReadBounded(resp.Body, httpx.DefaultMaxBodyBytes)
	closeErr := resp.Body.Close()
	if readErr != nil {
		return c.requestFailure(readErr, rawURL)
	}
	if closeErr != nil {
		return c.requestFailure(closeErr, rawURL)
	}
	if trimmed := bytes.TrimSpace(body); len(trimmed) > 0 && trimmed[0] == '<' {
		c.mirrorFailed(ctx, rawURL, MirrorFailure{Reason: "returned a web page instead of " + what, Blocked: true})
		return httpx.NewError(httpx.CodeUpstreamBlocked, "html_instead_of_json", "archive returned a web page instead of "+what+"; check the mirror and account access")
	}
	if err := json.Unmarshal(body, target); err != nil {
		return httpx.WrapError(httpx.CodeUpstream, "invalid_json", "invalid "+what+" from "+responseHost(resp, rawURL), err)
	}
	return nil
}
