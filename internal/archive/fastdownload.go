package archive

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bjoernaagaard/aakb/internal/httpx"
	"github.com/bjoernaagaard/aakb/internal/store"
)

const (
	maxDownloadPaths       = 3
	maxFastAPIResponseSize = 1 << 20
	noFastDownloadAPIError = "invalid domain_index or path_index"

	fastServerBaseCooldown = 30 * time.Second
	fastServerMaxCooldown  = 10 * time.Minute
)

var (
	domainIndexes     = []int{0, 1, 2, 6, 12, 13, 14}
	errNoFastDownload = httpx.NewError(httpx.CodeNotFound, "no_fast_download", "The archive has no fast download for this file; choose another copy from search")
)

// DownloadRequest describes a hash-based fast download from the archive.
type DownloadRequest struct {
	Hash     string
	Title    string
	Format   string
	Progress func(done, total int64)
}

// DownloadResult identifies a finalized or reused local archive download.
type DownloadResult struct {
	Path          string
	Name          string
	Bytes         int64
	MD5           string
	SHA256        string
	Reused        bool
	DownloadsLeft *int
}

type fastDownloadAnswer struct {
	DownloadURL *string              `json:"download_url"`
	Error       string               `json:"error"`
	AccountInfo *fastDownloadAccount `json:"account_fast_download_info"`
}

type fastDownloadAccount struct {
	DownloadsLeft          int      `json:"downloads_left"`
	DownloadsPerDay        int      `json:"downloads_per_day"`
	RecentlyDownloadedMD5s []string `json:"recently_downloaded_md5s"`
}

type fastDownloadGrant struct {
	url     string
	account *fastDownloadAccount
}

// stopDownload wraps a terminal API failure that applies across all servers.
type stopDownloadError struct {
	err error
}

func (e *stopDownloadError) Error() string { return e.err.Error() }
func (e *stopDownloadError) Unwrap() error { return e.err }

type fastServerKey struct {
	pathIndex   int
	domainIndex int
}

type fastServerState struct {
	failures      int
	cooldownUntil time.Time
}

// fastServerHealth tracks per-(pathIndex, domainIndex) cooldowns so failing
// fast partner servers are demoted to the back of the attempt order instead of
// delaying every subsequent download.
type fastServerHealth struct {
	mu     sync.Mutex
	states map[fastServerKey]fastServerState
	now    func() time.Time
}

func newFastServerHealth() *fastServerHealth {
	return &fastServerHealth{
		states: make(map[fastServerKey]fastServerState),
		now:    time.Now,
	}
}

func (h *fastServerHealth) Succeeded(pathIndex, domainIndex int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.states, fastServerKey{pathIndex: pathIndex, domainIndex: domainIndex})
}

func (h *fastServerHealth) Failed(pathIndex, domainIndex int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := fastServerKey{pathIndex: pathIndex, domainIndex: domainIndex}
	state := h.states[key]
	now := h.now()
	if now.Before(state.cooldownUntil) {
		return
	}
	state.failures++
	cooldown := fastServerBaseCooldown
	for i := 1; i < state.failures && cooldown < fastServerMaxCooldown; i++ {
		cooldown *= 2
	}
	state.cooldownUntil = now.Add(min(cooldown, fastServerMaxCooldown))
	h.states[key] = state
}

// orderDomains returns domain indexes for pathIndex with healthy servers first
// in default preference order, followed by cooling-down servers ordered by
// earliest expiry.
func (h *fastServerHealth) orderDomains(pathIndex int, defaults []int) []int {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()

	type candidate struct {
		domainIndex   int
		defaultOrder  int
		inCooldown    bool
		cooldownUntil time.Time
	}
	ranked := make([]candidate, len(defaults))
	for i, domainIndex := range defaults {
		state := h.states[fastServerKey{pathIndex: pathIndex, domainIndex: domainIndex}]
		inCooldown := now.Before(state.cooldownUntil)
		ranked[i] = candidate{
			domainIndex:   domainIndex,
			defaultOrder:  i,
			inCooldown:    inCooldown,
			cooldownUntil: state.cooldownUntil,
		}
	}
	slices.SortStableFunc(ranked, func(a, b candidate) int {
		if a.inCooldown != b.inCooldown {
			if !a.inCooldown {
				return -1
			}
			return 1
		}
		if a.inCooldown {
			if c := a.cooldownUntil.Compare(b.cooldownUntil); c != 0 {
				return c
			}
		}
		return cmp.Compare(a.defaultOrder, b.defaultOrder)
	})
	out := make([]int, len(ranked))
	for i, item := range ranked {
		out[i] = item.domainIndex
	}
	return out
}

// DownloadHash downloads or reuses a verified file by MD5 hash via the archive's fast
// download API and internal/store.
func (c *Client) DownloadHash(ctx context.Context, req DownloadRequest) (*DownloadResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	prefix, hash, err := ParseRecordID(req.Hash)
	if err != nil || prefix != "md5" {
		return nil, httpx.NewError(httpx.CodeInvalidArgument, "invalid_hash", "hash must be a 32-character MD5 or an archive /md5/ page URL")
	}
	if req.Format != "" {
		if _, err := store.NormalizeFormat(req.Format); err != nil {
			return nil, err
		}
	}
	if c.store == nil {
		return nil, httpx.NewError(httpx.CodeConfig, "missing_store", "DownloadDir and CacheDir are required for downloads")
	}

	// O(1) manifest reuse before requiring credentials or calling the network.
	if reused, ok := c.store.LookupMD5(ctx, hash); ok {
		return downloadResultFromStore(reused), nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.secretKey == "" {
		return nil, httpx.NewError(httpx.CodeConfig, "missing_secret_key", "SecretKey is required for downloads")
	}

	var attempts []error
	var account *fastDownloadAccount
	missing := 0

collections:
	for pathIndex := range maxDownloadPaths {
		granted := false
		minInvalidDomain := -1
		orderedDomains := c.fastHealth.orderDomains(pathIndex, domainIndexes)

		for _, domainIndex := range orderedDomains {
			if minInvalidDomain >= 0 && domainIndex >= minInvalidDomain {
				continue
			}
			if err := ctx.Err(); err != nil {
				return nil, combineDownloadAttempts(attempts, err)
			}
			base, err := c.baseURLFor(ctx)
			if err != nil {
				return nil, combineDownloadAttempts(attempts, err)
			}

			var grant fastDownloadGrant
			err = c.onMirrorFrom(ctx, base, func(mirrorBase string) error {
				var resolveErr error
				grant, resolveErr = c.resolveDownloadURL(ctx, mirrorBase, hash, pathIndex, domainIndex)
				return resolveErr
			})
			if errors.Is(err, errNoFastDownload) {
				if domainIndex == 0 {
					if pathIndex == 0 {
						attempts = append(attempts, err)
					}
					break collections
				}
				if minInvalidDomain < 0 || domainIndex < minInvalidDomain {
					minInvalidDomain = domainIndex
				}
				continue
			}
			if err != nil {
				if stop, ok := errors.AsType[*stopDownloadError](err); ok && stop != nil {
					return nil, stop.err
				}
				attempts = append(attempts, err)
				continue
			}

			granted = true
			if grant.account != nil {
				account = grant.account
			}

			res, transferErr := c.store.Download(ctx, store.TransferRequest{
				URL:         grant.url,
				Title:       req.Title,
				Format:      req.Format,
				ExpectedMD5: hash,
				Validation:  store.ValidationGeneral,
				AllowResume: true,
				Fetch:       c.fetchDownloadStream,
				Progress:    req.Progress,
			})
			if transferErr == nil {
				c.fastHealth.Succeeded(pathIndex, domainIndex)
				out := downloadResultFromStore(res)
				if !res.Reused {
					out.DownloadsLeft = downloadsLeftAfter(account, hash)
				}
				return out, nil
			}
			if ctx.Err() != nil {
				return nil, combineDownloadAttempts(attempts, ctx.Err())
			}
			if isTerminalTransferError(transferErr) {
				return nil, transferErr
			}
			if isFileMissingError(transferErr) {
				missing++
			} else {
				c.fastHealth.Failed(pathIndex, domainIndex)
			}
			attempts = append(attempts, transferErr)
		}
		if !granted {
			break
		}
	}

	if missing > 0 && missing == len(attempts) && ctx.Err() == nil {
		return nil, httpx.NewError(
			httpx.CodeNotFound,
			"not_on_fast_servers",
			fmt.Sprintf("this file is missing from every fast download server: all %d attempts returned HTTP 404", missing),
		)
	}
	return nil, combineDownloadAttempts(attempts, ctx.Err())
}

func (c *Client) fetchDownloadStream(ctx context.Context, rawURL string, extraHeaders http.Header) (*http.Response, error) {
	return c.doGetWith(ctx, rawURL, extraHeaders)
}

func (c *Client) resolveDownloadURL(ctx context.Context, base, hash string, pathIndex, domainIndex int) (fastDownloadGrant, error) {
	values := url.Values{}
	values.Set("md5", hash)
	values.Set("key", c.secretKey)
	values.Set("path_index", strconv.Itoa(pathIndex))
	values.Set("domain_index", strconv.Itoa(domainIndex))
	apiURL := buildPathURL(base, "/dyn/api/fast_download.json") + "?" + values.Encode()

	resp, err := c.doGet(ctx, apiURL)
	if err != nil {
		return fastDownloadGrant{}, err
	}
	body, readErr := httpx.ReadBounded(resp.Body, maxFastAPIResponseSize)
	closeErr := resp.Body.Close()
	if readErr != nil {
		return fastDownloadGrant{}, c.http.RequestError(readErr, apiURL)
	}
	if closeErr != nil {
		return fastDownloadGrant{}, c.http.RequestError(closeErr, apiURL)
	}

	var payload fastDownloadAnswer
	jsonErr := json.Unmarshal(body, &payload)
	apiError := strings.TrimSpace(payload.Error)
	if jsonErr != nil || (resp.StatusCode != http.StatusOK && apiError == "") {
		switch {
		case resp.StatusCode == http.StatusForbidden:
			baseErr := httpx.StatusError(resp, apiURL)
			return fastDownloadGrant{}, httpx.NewError(httpx.CodeUpstreamBlocked, "fast_api_forbidden", baseErr.SafeMessage+"; check account access and credentials")
		case resp.StatusCode != http.StatusOK:
			return fastDownloadGrant{}, httpx.StatusError(resp, apiURL)
		default:
			return fastDownloadGrant{}, httpx.WrapError(httpx.CodeUpstream, "invalid_fast_api_json", fmt.Sprintf("invalid JSON from %s (HTTP status %d)", httpx.ResponseHost(resp, apiURL), resp.StatusCode), jsonErr)
		}
	}
	if apiError != "" {
		if resp.StatusCode == http.StatusBadRequest && strings.EqualFold(apiError, noFastDownloadAPIError) {
			return fastDownloadGrant{}, errNoFastDownload
		}
		redactedMsg := c.http.Redactor().RedactString(apiError)
		return fastDownloadGrant{}, classifyFastAPIRejection(resp, apiURL, redactedMsg)
	}
	if payload.DownloadURL == nil || strings.TrimSpace(*payload.DownloadURL) == "" {
		return fastDownloadGrant{}, httpx.NewError(httpx.CodeUpstream, "empty_download_url", fmt.Sprintf("API at %s returned an empty download URL", httpx.ResponseHost(resp, apiURL)))
	}
	rawDownloadURL := strings.TrimSpace(*payload.DownloadURL)
	parsed, err := url.Parse(rawDownloadURL)
	if err != nil || !parsed.IsAbs() || httpx.ValidateHTTPSURL(parsed) != nil {
		return fastDownloadGrant{}, httpx.NewError(httpx.CodeUpstream, "invalid_download_url", fmt.Sprintf("API at %s returned an invalid download URL", httpx.ResponseHost(resp, apiURL)))
	}
	return fastDownloadGrant{url: parsed.String(), account: payload.AccountInfo}, nil
}

func downloadResultFromStore(res store.Result) *DownloadResult {
	return &DownloadResult{
		Path:   res.Path,
		Name:   res.Name,
		Bytes:  res.Bytes,
		MD5:    res.MD5,
		SHA256: res.SHA256,
		Reused: res.Reused,
	}
}

func classifyFastAPIRejection(resp *http.Response, apiURL, apiError string) error {
	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		return &stopDownloadError{err: httpx.NewError(httpx.CodeUpstreamBlocked, "fast_downloads_exhausted", "no fast downloads left: this membership's allowance for the rolling window is used up. Files downloaded recently can still be downloaded again; account_status shows the allowance")}
	case http.StatusUnauthorized:
		return &stopDownloadError{err: httpx.NewError(httpx.CodeConfig, "invalid_secret_key", "The archive rejected SecretKey for fast downloads: copy the secret key from your archive account page")}
	case http.StatusForbidden:
		return &stopDownloadError{err: httpx.NewError(httpx.CodeUpstreamBlocked, "membership_required", "fast downloads need an active membership, and the archive says this account has none")}
	case http.StatusNotFound:
		return &stopDownloadError{err: httpx.NewError(httpx.CodeNotFound, "hash_not_found", "The archive has no file with this hash; choose a hash from search")}
	default:
		return httpx.NewError(httpx.CodeUpstream, "fast_api_rejected", fmt.Sprintf("API rejected request at %s (HTTP status %d): %s", httpx.ResponseHost(resp, apiURL), resp.StatusCode, apiErrorMessage(apiError)))
	}
}

func downloadsLeftAfter(account *fastDownloadAccount, hash string) *int {
	if account == nil {
		return nil
	}
	left := account.DownloadsLeft
	if !slices.ContainsFunc(account.RecentlyDownloadedMD5s, func(seen string) bool {
		return strings.EqualFold(strings.TrimSpace(seen), hash)
	}) {
		left = max(0, left-1)
	}
	return &left
}

func isFileMissingError(err error) bool {
	if pe, ok := errors.AsType[*httpx.ProviderError](err); ok && pe != nil {
		return pe.Code == httpx.CodeNotFound && (pe.Reason == "not_found" || pe.StatusCode == http.StatusNotFound)
	}
	return false
}

func isTerminalTransferError(err error) bool {
	if pe, ok := errors.AsType[*httpx.ProviderError](err); ok && pe != nil {
		return pe.Code == httpx.CodeConfig || pe.Reason == "local_io"
	}
	return false
}

func combineDownloadAttempts(attempts []error, stop error) error {
	if stop != nil {
		return stop
	}
	if len(attempts) == 0 {
		return httpx.NewError(httpx.CodeUpstream, "no_download_servers", "download failed: no download servers were attempted")
	}
	last := attempts[len(attempts)-1]
	if pe, ok := errors.AsType[*httpx.ProviderError](last); ok && pe != nil {
		return pe
	}
	return httpx.WrapError(httpx.CodeUpstream, "download_failed", last.Error(), last)
}
