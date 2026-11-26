package store

import (
	"bufio"
	"context"
	"crypto/md5" //nolint:gosec // G501: MD5 is required to verify archive file checksums
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/bjoernaagaard/aakb/internal/httpx"
)

const (
	// MaxDownloadBytes is the default maximum size for downloaded files (8 GiB).
	MaxDownloadBytes int64 = 8 << 30
	// MaxXMLBytes is the default maximum size for TEI XML files (128 MiB).
	MaxXMLBytes int64 = 128 << 20

	maxResumeAttempts = 2
	progressStepBytes = 512 << 10
	peekHeaderBytes   = 512
	copyBufferBytes   = 32 << 10
)

// Config configures a local download Store.
type Config struct {
	DownloadDir string
	CacheDir    string
	HTTPClient  *httpx.Client
}

// Store manages local download files, manifest reuse, singleflight deduplication,
// and cross-process lease locking. It is safe for concurrent use.
type Store struct {
	downloadDir  string
	cacheDir     string
	locksDir     string
	manifestPath string
	httpClient   *httpx.Client

	manifestMu sync.Mutex
	flightMu   sync.Mutex
	inFlight   map[string]chan struct{}
}

// Result describes a finalized or reused local file.
type Result struct {
	Path     string
	Name     string
	Bytes    int64
	MD5      string
	SHA256   string
	MIMEType string
	Reused   bool
}

// FetchFunc issues a GET request with optional extra headers (such as Range).
type FetchFunc func(ctx context.Context, rawURL string, extraHeaders http.Header) (*http.Response, error)

// TransferRequest describes one download to execute or reuse through the Store.
type TransferRequest struct {
	Key            string
	URL            string
	Title          string
	Format         string
	MIMEType       string
	ExpectedMD5    string
	ExpectedSHA256 string
	MaxBytes       int64
	Validation     ValidationMode
	AllowResume    bool
	Fetch          FetchFunc
	Progress       func(done, total int64)
}

// New validates paths and constructs a Store without touching the network.
func New(cfg Config) (*Store, error) {
	if !absolutePath(cfg.DownloadDir) {
		return nil, httpx.NewError(httpx.CodeConfig, "invalid_download_dir", "DownloadDir must be an absolute path")
	}
	if !absolutePath(cfg.CacheDir) {
		return nil, httpx.NewError(httpx.CodeConfig, "invalid_cache_dir", "CacheDir must be an absolute path")
	}
	downloadDir := filepath.Clean(cfg.DownloadDir)
	cacheDir := filepath.Clean(cfg.CacheDir)
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = httpx.NewClient(httpx.ClientOptions{})
	}
	return &Store{
		downloadDir:  downloadDir,
		cacheDir:     cacheDir,
		locksDir:     filepath.Join(cacheDir, "locks"),
		manifestPath: filepath.Join(cacheDir, manifestFilename),
		httpClient:   httpClient,
		inFlight:     make(map[string]chan struct{}),
	}, nil
}

func absolutePath(path string) bool {
	return path != "" && filepath.IsAbs(path) && !strings.ContainsRune(path, 0)
}

// DownloadDir returns the absolute directory where downloaded files live.
func (s *Store) DownloadDir() string {
	return s.downloadDir
}

// LookupMD5 returns a stat-verified (or digest-reverified) local file matching md5Hex.
func (s *Store) LookupMD5(ctx context.Context, md5Hex string) (Result, bool) {
	md5Hex = strings.ToLower(strings.TrimSpace(md5Hex))
	if md5Hex == "" {
		return Result{}, false
	}
	entry, ok := s.readManifestEntry(func(m *Manifest) (Entry, bool) {
		e, exists := m.ByMD5[md5Hex]
		return e, exists
	})
	if !ok {
		return Result{}, false
	}
	return s.verifyEntry(ctx, entry, "", md5Hex, "")
}

// LookupSHA256 returns a stat-verified (or digest-reverified) local file matching sha256Hex.
func (s *Store) LookupSHA256(ctx context.Context, sha256Hex string) (Result, bool) {
	sha256Hex = strings.ToLower(strings.TrimSpace(sha256Hex))
	if sha256Hex == "" {
		return Result{}, false
	}
	entry, ok := s.readManifestEntry(func(m *Manifest) (Entry, bool) {
		e, exists := m.BySHA256[sha256Hex]
		return e, exists
	})
	if !ok {
		return Result{}, false
	}
	return s.verifyEntry(ctx, entry, "", "", sha256Hex)
}

// LookupKey returns a stat-verified (or digest-reverified) local file matching an artifact key.
func (s *Store) LookupKey(ctx context.Context, key string) (Result, bool) {
	key = strings.TrimSpace(key)
	if key == "" {
		return Result{}, false
	}
	entry, ok := s.readManifestEntry(func(m *Manifest) (Entry, bool) {
		e, exists := m.ByKey[key]
		return e, exists
	})
	if !ok {
		return Result{}, false
	}
	return s.verifyEntry(ctx, entry, key, entry.MD5, entry.SHA256)
}

func (s *Store) verifyEntry(ctx context.Context, entry Entry, key, wantMD5, wantSHA256 string) (Result, bool) {
	if !ValidFilename(entry.File) {
		s.pruneEntry(ctx, key, wantMD5, wantSHA256)
		return Result{}, false
	}
	root, err := os.OpenRoot(s.downloadDir)
	if err != nil {
		return Result{}, false
	}
	defer func() { _ = root.Close() }() //nolint:errcheck // closing read-only download root

	info, err := root.Lstat(entry.File)
	if err != nil || !info.Mode().IsRegular() {
		s.pruneEntry(ctx, key, wantMD5, wantSHA256)
		return Result{}, false
	}

	fullPath := filepath.Join(s.downloadDir, entry.File)
	if info.Size() == entry.Bytes && info.ModTime().UnixNano() == entry.ModTimeUnixNano {
		return Result{
			Path:     fullPath,
			Name:     entry.File,
			Bytes:    entry.Bytes,
			MD5:      entry.MD5,
			SHA256:   entry.SHA256,
			MIMEType: entry.MIMEType,
			Reused:   true,
		}, true
	}

	// File metadata changed on disk; recompute digests via OpenRoot + O_NONBLOCK without following symlinks.
	file, err := root.OpenFile(entry.File, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		s.pruneEntry(ctx, key, wantMD5, wantSHA256)
		return Result{}, false
	}
	defer func() { _ = file.Close() }() //nolint:errcheck // closing read-only file after digest verification

	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		s.pruneEntry(ctx, key, wantMD5, wantSHA256)
		return Result{}, false
	}

	gotMD5, gotSHA256, size, err := hashReader(ctx, file, MaxDownloadBytes)
	if err != nil {
		return Result{}, false
	}
	hasExpected := wantMD5 != "" || wantSHA256 != ""
	md5Mismatch := wantMD5 != "" && !strings.EqualFold(gotMD5, wantMD5)
	shaMismatch := wantSHA256 != "" && !strings.EqualFold(gotSHA256, wantSHA256)
	if !hasExpected || md5Mismatch || shaMismatch {
		s.pruneEntry(ctx, key, wantMD5, wantSHA256)
		return Result{}, false
	}

	updated := Entry{
		File:            entry.File,
		Bytes:           size,
		ModTimeUnixNano: opened.ModTime().UnixNano(),
		MD5:             gotMD5,
		SHA256:          gotSHA256,
		MIMEType:        entry.MIMEType,
	}
	s.recordEntry(ctx, key, updated)
	return Result{
		Path:     fullPath,
		Name:     entry.File,
		Bytes:    size,
		MD5:      gotMD5,
		SHA256:   gotSHA256,
		MIMEType: entry.MIMEType,
		Reused:   true,
	}, true
}

func (s *Store) pruneEntry(ctx context.Context, key, md5Hex, sha256Hex string) {
	if ctx.Err() != nil {
		return
	}
	_ = s.mutateManifest(ctx, func(m *Manifest) bool { //nolint:errcheck // best-effort lazy pruning of missing manifest entries
		changed := false
		if key != "" {
			if _, ok := m.ByKey[key]; ok {
				delete(m.ByKey, key)
				changed = true
			}
		}
		if md5Hex != "" {
			if _, ok := m.ByMD5[md5Hex]; ok {
				delete(m.ByMD5, md5Hex)
				changed = true
			}
		}
		if sha256Hex != "" {
			if _, ok := m.BySHA256[sha256Hex]; ok {
				delete(m.BySHA256, sha256Hex)
				changed = true
			}
		}
		return changed
	})
}

func (s *Store) recordEntry(ctx context.Context, key string, entry Entry) {
	if ctx.Err() != nil {
		return
	}
	_ = s.mutateManifest(ctx, func(m *Manifest) bool { //nolint:errcheck // best-effort manifest update must not hide a published file
		if entry.MD5 != "" {
			m.ByMD5[strings.ToLower(entry.MD5)] = entry
		}
		if entry.SHA256 != "" {
			m.BySHA256[strings.ToLower(entry.SHA256)] = entry
		}
		if key != "" {
			m.ByKey[key] = entry
		}
		return true
	})
}

// Download reuses a matching local file if present, or streams a verified file
// to a temporary .part file and publishes it atomically under DownloadDir.
func (s *Store) Download(ctx context.Context, req TransferRequest) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	req.Key = strings.TrimSpace(req.Key)
	req.URL = strings.TrimSpace(req.URL)
	req.ExpectedMD5 = strings.ToLower(strings.TrimSpace(req.ExpectedMD5))
	req.ExpectedSHA256 = strings.ToLower(strings.TrimSpace(req.ExpectedSHA256))

	if req.ExpectedMD5 != "" {
		if err := validateHexDigest(req.ExpectedMD5, md5.Size*2, "expected MD5"); err != nil {
			return Result{}, err
		}
	}
	if req.ExpectedSHA256 != "" {
		if err := validateHexDigest(req.ExpectedSHA256, sha256.Size*2, "expected SHA-256"); err != nil {
			return Result{}, err
		}
	}
	if req.URL == "" {
		return Result{}, httpx.NewError(httpx.CodeInvalidArgument, "missing_url", "download URL is required")
	}

	maxBytes := req.MaxBytes
	if maxBytes <= 0 {
		if req.Validation == ValidationTEIXML {
			maxBytes = MaxXMLBytes
		} else {
			maxBytes = MaxDownloadBytes
		}
	}

	if err := os.MkdirAll(s.downloadDir, downloadDirMode); err != nil {
		return Result{}, localIOError(err)
	}
	if err := os.MkdirAll(s.cacheDir, privateDirMode); err != nil {
		return Result{}, localIOError(err)
	}

	lockKey := transferLockKey(req)
	releaseFlight, err := s.acquireInFlight(ctx, lockKey)
	if err != nil {
		return Result{}, err
	}
	defer releaseFlight()

	unlock, owned, err := acquireLease(ctx, s.locksDir, s.downloadDir, lockKey)
	if err != nil {
		return Result{}, err
	}
	defer unlock()

	if reused, ok := s.lookupForRequest(ctx, req); ok {
		return reused, nil
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	fetch := req.Fetch
	if fetch == nil {
		fetch = s.defaultFetch
	}

	resp, err := fetch(ctx, req.URL, nil)
	if err != nil {
		return Result{}, err
	}
	defer func() {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close() //nolint:errcheck // closing transfer response body
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return Result{}, unexpectedStatusError(resp.StatusCode)
	}
	if resp.ContentLength > maxBytes {
		return Result{}, httpx.NewError(httpx.CodeUpstream, "download_too_large", "download exceeds the maximum allowed size")
	}

	reader := bufio.NewReader(resp.Body)
	prefix, peekErr := reader.Peek(peekHeaderBytes)
	if peekErr != nil && !errors.Is(peekErr, io.EOF) && !errors.Is(peekErr, bufio.ErrBufferFull) {
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		return Result{}, httpx.WrapError(httpx.CodeUpstream, "read_failed", "failed to inspect download stream", peekErr)
	}
	if LooksLikeHTML(resp.Header.Get("Content-Type"), prefix) {
		return Result{}, httpx.NewError(httpx.CodeUpstreamBlocked, "html_challenge", "download returned HTML instead of a file")
	}
	if req.Validation == ValidationPDF && !ValidatePDFPrefix(prefix) {
		return Result{}, httpx.NewError(httpx.CodeUpstream, "invalid_pdf", "download did not contain a valid PDF")
	}

	format := req.Format
	if format == "" {
		var finalURL *url.URL
		if resp.Request != nil {
			finalURL = resp.Request.URL
		}
		format = DownloadFormat(resp.Header, finalURL)
		if format == "bin" {
			switch req.Validation {
			case ValidationPDF:
				format = "pdf"
			case ValidationTEIXML:
				format = "xml"
			case ValidationGeneral:
			}
		}
	}
	format, err = NormalizeFormat(format)
	if err != nil {
		return Result{}, err
	}

	temp, err := os.CreateTemp(s.downloadDir, ".aakb-download-*.part")
	if err != nil {
		return Result{}, localIOError(err)
	}
	tempPath := temp.Name()
	defer func() {
		_ = temp.Close()        //nolint:errcheck // cleanup temporary download part
		_ = os.Remove(tempPath) //nolint:errcheck // cleanup temporary download part
	}()

	xfer := &transferState{
		dst:      temp,
		md5Hash:  md5.New(), //nolint:gosec // G401: MD5 is required to verify archive file checksums
		shaHash:  sha256.New(),
		expected: resp.ContentLength,
		maxBytes: maxBytes,
		progress: req.Progress,
	}

	var src io.Reader = reader
	for resumes := 0; ; resumes++ {
		err = xfer.copyFrom(ctx, src)
		if err == nil {
			break
		}
		var interrupted *interruptedTransferError
		canResume := req.AllowResume && resumes < maxResumeAttempts && xfer.written > 0 && ctx.Err() == nil
		if !canResume || !errors.As(err, &interrupted) {
			break
		}
		_ = resp.Body.Close() //nolint:errcheck // closing interrupted response before resuming
		nextResp, resumeErr := resumeTransfer(ctx, fetch, req.URL, xfer.written, xfer.expected)
		if resumeErr != nil {
			resp = nil
			err = resumeErr
			break
		}
		resp = nextResp
		src = resp.Body
	}
	if err == nil {
		err = xfer.finish(req.ExpectedMD5, req.ExpectedSHA256)
	}
	if err != nil {
		return Result{}, err
	}

	if req.Validation == ValidationTEIXML {
		if _, err := temp.Seek(0, io.SeekStart); err != nil {
			return Result{}, localIOError(err)
		}
		if err := ValidateTEIXML(temp); err != nil {
			return Result{}, err
		}
	}

	if err := temp.Sync(); err != nil {
		return Result{}, localIOError(err)
	}
	if err := temp.Chmod(downloadFileMode); err != nil {
		return Result{}, localIOError(err)
	}
	if err := temp.Close(); err != nil {
		return Result{}, localIOError(err)
	}

	if !owned() {
		return Result{}, httpx.NewError(httpx.CodeUpstream, "lease_lost", "download lease was lost; retry the request")
	}

	md5Hex := hex.EncodeToString(xfer.md5Hash.Sum(nil))
	sha256Hex := hex.EncodeToString(xfer.shaHash.Sum(nil))
	digestPrefix := req.ExpectedMD5
	if digestPrefix == "" {
		digestPrefix = req.ExpectedSHA256
	}
	fallbackKey := req.Key
	if fallbackKey == "" {
		fallbackKey = sha256Hex
	}

	publishedName, modTimeNano, err := s.publishFile(ctx, filepath.Base(tempPath), req.Title, format, digestPrefix, fallbackKey)
	if err != nil {
		return Result{}, err
	}

	mimeType := req.MIMEType
	if mimeType == "" {
		mimeType = resp.Header.Get("Content-Type")
	}
	entry := Entry{
		File:            publishedName,
		Bytes:           xfer.written,
		ModTimeUnixNano: modTimeNano,
		MD5:             md5Hex,
		SHA256:          sha256Hex,
		MIMEType:        mimeType,
	}
	s.recordEntry(ctx, req.Key, entry)

	return Result{
		Path:     filepath.Join(s.downloadDir, publishedName),
		Name:     publishedName,
		Bytes:    xfer.written,
		MD5:      md5Hex,
		SHA256:   sha256Hex,
		MIMEType: mimeType,
		Reused:   false,
	}, nil
}

func (s *Store) lookupForRequest(ctx context.Context, req TransferRequest) (Result, bool) {
	if req.Key != "" {
		if res, ok := s.LookupKey(ctx, req.Key); ok {
			return res, true
		}
	}
	if req.ExpectedMD5 != "" {
		if res, ok := s.LookupMD5(ctx, req.ExpectedMD5); ok {
			return res, true
		}
	}
	if req.ExpectedSHA256 != "" {
		if res, ok := s.LookupSHA256(ctx, req.ExpectedSHA256); ok {
			return res, true
		}
	}
	return Result{}, false
}

func (s *Store) defaultFetch(ctx context.Context, rawURL string, extraHeaders http.Header) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, httpx.NewError(httpx.CodeInvalidArgument, "invalid_url", "invalid download URL")
	}
	for k, vals := range extraHeaders {
		for _, v := range vals {
			httpReq.Header.Add(k, v)
		}
	}
	return s.httpClient.Do(httpReq)
}

func (s *Store) acquireInFlight(ctx context.Context, key string) (func(), error) {
	for {
		s.flightMu.Lock()
		waitCh, busy := s.inFlight[key]
		if !busy {
			ch := make(chan struct{})
			s.inFlight[key] = ch
			s.flightMu.Unlock()
			return func() {
				s.flightMu.Lock()
				delete(s.inFlight, key)
				s.flightMu.Unlock()
				close(ch)
			}, nil
		}
		s.flightMu.Unlock()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-waitCh:
		}
	}
}

func (s *Store) publishFile(ctx context.Context, tempBase, title, format, digestPrefix, fallbackKey string) (name string, modTimeNano int64, err error) {
	root, err := os.OpenRoot(s.downloadDir)
	if err != nil {
		return "", 0, localIOError(err)
	}
	defer func() { _ = root.Close() }() //nolint:errcheck // closing download root after publication

	for candidate := range CandidateNames(title, format, digestPrefix, fallbackKey) {
		if err := ctx.Err(); err != nil {
			return "", 0, err
		}
		if !ValidFilename(candidate) {
			continue
		}
		if err := root.Link(tempBase, candidate); err == nil {
			info, statErr := root.Lstat(candidate)
			if statErr != nil {
				return candidate, 0, nil
			}
			return candidate, info.ModTime().UnixNano(), nil
		} else if !errors.Is(err, os.ErrExist) {
			return "", 0, localIOError(err)
		}
	}
	return "", 0, httpx.NewError(httpx.CodeInternal, "no_free_filename", "could not find a free download filename")
}

func transferLockKey(req TransferRequest) string {
	switch {
	case req.Key != "":
		return "key:" + req.Key
	case req.ExpectedMD5 != "":
		return "md5:" + req.ExpectedMD5
	case req.ExpectedSHA256 != "":
		return "sha256:" + req.ExpectedSHA256
	default:
		return "url:" + req.URL
	}
}

func validateHexDigest(value string, length int, label string) error {
	if len(value) != length {
		return httpx.NewError(httpx.CodeInvalidArgument, "invalid_digest", label+" has invalid length")
	}
	if _, err := hex.DecodeString(value); err != nil {
		return httpx.NewError(httpx.CodeInvalidArgument, "invalid_digest", label+" must be hexadecimal")
	}
	return nil
}

func unexpectedStatusError(status int) error {
	switch status {
	case http.StatusUnauthorized:
		return httpx.NewError(httpx.CodeConfig, "unauthorized", "upstream rejected download credentials")
	case http.StatusForbidden:
		return httpx.NewError(httpx.CodeUpstreamBlocked, "forbidden", "upstream host refused download access")
	case http.StatusNotFound:
		return httpx.NewError(httpx.CodeNotFound, "not_found", "requested download file was not found")
	case http.StatusTooManyRequests:
		return httpx.NewError(httpx.CodeUpstream, "rate_limited", "upstream download rate limit reached")
	default:
		return &httpx.ProviderError{
			Code:        httpx.CodeUpstream,
			Reason:      "http_status",
			SafeMessage: fmt.Sprintf("download host returned HTTP %d", status),
			StatusCode:  status,
		}
	}
}

type interruptedTransferError struct {
	written int64
	cause   error
}

func (e *interruptedTransferError) Error() string {
	return fmt.Sprintf("download interrupted after %d bytes", e.written)
}

func (e *interruptedTransferError) Unwrap() error { return e.cause }

type readSourceError struct{ cause error }

func (e *readSourceError) Error() string { return e.cause.Error() }
func (e *readSourceError) Unwrap() error { return e.cause }

type writeDestError struct{ cause error }

func (e *writeDestError) Error() string { return e.cause.Error() }
func (e *writeDestError) Unwrap() error { return e.cause }

type transferState struct {
	dst      io.Writer
	md5Hash  hash.Hash
	shaHash  hash.Hash
	written  int64
	reported int64
	expected int64
	maxBytes int64
	progress func(done, total int64)
}

func (t *transferState) copyFrom(ctx context.Context, src io.Reader) error {
	multi := io.MultiWriter(t.dst, t.md5Hash, t.shaHash)
	_, err := copyBounded(ctx, multi, src, t.maxBytes-t.written, func(count int) {
		t.written += int64(count)
		if t.progress != nil && t.written-t.reported >= progressStepBytes {
			t.progress(t.written, max(t.expected, 0))
			t.reported = t.written
		}
	})
	if err != nil {
		if writeErr, ok := errors.AsType[*writeDestError](err); ok {
			return localIOError(writeErr.cause)
		}
		if readErr, ok := errors.AsType[*readSourceError](err); ok {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return &interruptedTransferError{written: t.written, cause: readErr.cause}
		}
		return err
	}
	if t.expected >= 0 && t.written < t.expected {
		return &interruptedTransferError{written: t.written, cause: io.ErrUnexpectedEOF}
	}
	return nil
}

func (t *transferState) finish(expectedMD5, expectedSHA256 string) error {
	if t.progress != nil && t.reported != t.written {
		t.progress(t.written, max(t.expected, 0))
		t.reported = t.written
	}
	if t.written == 0 {
		return httpx.NewError(httpx.CodeUpstream, "empty_download", "download returned an empty file")
	}
	if t.expected >= 0 && t.written != t.expected {
		return httpx.NewError(httpx.CodeUpstream, "truncated_download", "downloaded file size did not match Content-Length")
	}
	if expectedMD5 != "" && !strings.EqualFold(hex.EncodeToString(t.md5Hash.Sum(nil)), expectedMD5) {
		return httpx.NewError(httpx.CodeUpstream, "checksum_mismatch", "downloaded file MD5 checksum did not match")
	}
	if expectedSHA256 != "" && !strings.EqualFold(hex.EncodeToString(t.shaHash.Sum(nil)), expectedSHA256) {
		return httpx.NewError(httpx.CodeUpstream, "checksum_mismatch", "downloaded file SHA-256 checksum did not match")
	}
	return nil
}

func resumeTransfer(ctx context.Context, fetch FetchFunc, rawURL string, offset, expectedTotal int64) (*http.Response, error) {
	headers := http.Header{"Range": {fmt.Sprintf("bytes=%d-", offset)}}
	resp, err := fetch(ctx, rawURL, headers)
	if err != nil {
		return nil, err
	}
	start, total, ok := ContentRangeStart(resp.Header.Get("Content-Range"))
	totalMismatch := expectedTotal >= 0 && total >= 0 && total != expectedTotal
	if resp.StatusCode != http.StatusPartialContent || !ok || start != offset || totalMismatch {
		httpx.DrainAndClose(resp.Body)
		return nil, httpx.NewError(httpx.CodeUpstream, "resume_rejected", "upstream server did not support resuming from the written offset")
	}
	return resp, nil
}

func copyBounded(ctx context.Context, dst io.Writer, src io.Reader, limit int64, onWrite func(int)) (int64, error) {
	var written int64
	buf := make([]byte, copyBufferBytes)
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			if written+int64(n) > limit {
				return written, httpx.NewError(httpx.CodeUpstream, "download_too_large", "download exceeded the maximum allowed size")
			}
			count, writeErr := dst.Write(buf[:n])
			written += int64(count)
			if onWrite != nil && count > 0 {
				onWrite(count)
			}
			if writeErr != nil {
				return written, &writeDestError{cause: writeErr}
			}
			if count != n {
				return written, &writeDestError{cause: io.ErrShortWrite}
			}
		}
		if errors.Is(readErr, io.EOF) {
			return written, nil
		}
		if readErr != nil {
			return written, &readSourceError{cause: readErr}
		}
	}
}

func hashReader(ctx context.Context, r io.Reader, limit int64) (md5Hex, sha256Hex string, written int64, err error) {
	md5Hash := md5.New() //nolint:gosec // G401: MD5 is required to verify archive file checksums
	shaHash := sha256.New()
	written, err = copyBounded(ctx, io.MultiWriter(md5Hash, shaHash), r, limit, nil)
	if err != nil {
		return "", "", written, err
	}
	return hex.EncodeToString(md5Hash.Sum(nil)), hex.EncodeToString(shaHash.Sum(nil)), written, nil
}
