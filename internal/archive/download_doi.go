package archive

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/bjoernaagaard/aakb/internal/httpx"
	"github.com/bjoernaagaard/aakb/internal/store"
)

// DownloadDOIRequest describes an article download by DOI or hash. DOI and
// Hash are mutually exclusive. ExpectedTitle optionally checks a DOI result
// against an independently identified citation before spending a download
// grant. Title only controls the filename.
type DownloadDOIRequest struct {
	DOI           string
	Hash          string
	Title         string
	ExpectedTitle string
	Format        string
	Progress      func(done, total int64)
}

// DownloadDOI downloads an article by DOI, preferring SciDB's free MD5-verified
// PDF when an authoritative SciDB page already supplies one and falling back to
// the fast download servers (or vice versa for search-inferred matches), or by
// hash straight through the fast download servers.
func (c *Client) DownloadDOI(ctx context.Context, req DownloadDOIRequest) (*DownloadResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	hash, doi := strings.TrimSpace(req.Hash), strings.TrimSpace(req.DOI)
	if (hash == "") == (doi == "") {
		return nil, httpx.NewError(httpx.CodeInvalidArgument, "invalid_request", "pass exactly one of doi or hash")
	}
	if hash != "" {
		if req.ExpectedTitle != "" {
			return nil, httpx.NewError(httpx.CodeInvalidArgument, "invalid_request", "ExpectedTitle requires a DOI")
		}
		return c.DownloadHash(ctx, DownloadRequest{Hash: hash, Title: req.Title, Format: req.Format, Progress: req.Progress})
	}
	if _, ok := ParseDOI(doi); !ok {
		return nil, httpx.NewError(httpx.CodeInvalidArgument, "invalid_doi", "doi must be a valid DOI or DOI URL")
	}
	if req.Format != "" {
		if _, err := store.NormalizeFormat(req.Format); err != nil {
			return nil, err
		}
	}
	if c.store == nil {
		return nil, httpx.NewError(httpx.CodeConfig, "missing_store", "DownloadDir and CacheDir are required for downloads")
	}
	paper, err := c.LookupDOI(ctx, doi)
	if err != nil {
		return nil, err
	}
	// Canonical /scidb/<doi>/ pages already establish exact DOI identity, while
	// their record metadata frequently carries a publisher filename stub or an
	// empty title. Enforce ExpectedTitle only on search-inferred DOI matches.
	if req.ExpectedTitle != "" && !paper.AuthoritativeDOI {
		expected := NormalizeTitle(req.ExpectedTitle)
		if expected == "" || expected != NormalizeTitle(paper.Title) {
			return nil, httpx.NewError(httpx.CodeNotFound, "title_mismatch", "archive DOI record does not match the selected paper title")
		}
	}
	if req.Title != "" {
		paper.Title = req.Title
	} else if req.ExpectedTitle != "" && (paper.Title == "" || paper.AuthoritativeDOI) {
		paper.Title = req.ExpectedTitle
	}
	// When canonical /scidb/<doi>/ already provided a free PDF link, try it
	// before spending paid fast-download quota or triggering fast-API 429s.
	if paper.DownloadURL != "" {
		result, scidbErr := c.downloadPaperFromSciDB(ctx, paper, req)
		if scidbErr == nil {
			return result, nil
		}
		if isLocalIOError(scidbErr) || ctx.Err() != nil {
			return nil, scidbErr
		}
		if paper.Hash != "" {
			fastResult, fastErr := c.DownloadHash(ctx, DownloadRequest{Hash: paper.Hash, Title: paper.Title, Format: req.Format, Progress: req.Progress})
			if fastErr == nil {
				return fastResult, nil
			}
			return nil, fastErr
		}
		return nil, scidbErr
	}
	var fastErr error
	if paper.Hash != "" {
		result, err := c.DownloadHash(ctx, DownloadRequest{Hash: paper.Hash, Title: paper.Title, Format: req.Format, Progress: req.Progress})
		if err == nil {
			return result, nil
		}
		fastErr = err
		// A broken local download path fails the SciDB fallback the same way,
		// so report it directly instead of masking it with a SciDB miss.
		if isLocalIOError(err) {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result, err := c.downloadPaperFromSciDB(ctx, paper, req)
	if err != nil {
		// The fast download error is the primary failure: SciDB was only a
		// fallback, so its miss must not mask quota, credential, or upstream
		// errors from the fast attempt.
		if fastErr != nil {
			return nil, fastErr
		}
		return nil, err
	}
	return result, nil
}

// isLocalIOError reports store failures from the local machine, such as an
// unwritable download folder, that a SciDB fallback would hit the same way.
func isLocalIOError(err error) bool {
	pe, ok := errors.AsType[*httpx.ProviderError](err)
	return ok && pe != nil && pe.Reason == "local_io"
}

// resolveSciDBFile maps a DOI to SciDB's PDF URL and archive hash on base.
func (c *Client) resolveSciDBFile(ctx context.Context, base string, paper *Paper) (string, string, error) {
	pageURL := buildSciDBURL(base, paper.DOI)
	root, finalURL, err := c.fetchSciDBPage(ctx, pageURL)
	if err != nil {
		return "", "", err
	}
	if !isSciDBDOIPage(finalURL, base, paper.DOI) {
		return "", "", httpx.NewError(httpx.CodeNotFound, "scidb_redirected", "SciDB redirected without providing the requested paper's PDF")
	}
	downloadURL, hash, err := sciDBFile(root, finalURL.String())
	if err != nil {
		return "", "", err
	}
	if paper.Hash != "" && !strings.EqualFold(hash, paper.Hash) {
		return "", "", httpx.NewError(httpx.CodeUpstream, "scidb_hash_mismatch", fmt.Sprintf("SciDB file does not match the selected archive hash %s", paper.Hash))
	}
	return downloadURL, hash, nil
}

// downloadPaperFromSciDB downloads a paper's free SciDB PDF through the
// store, which checks the %PDF- magic bytes and verifies the MD5 checksum.
func (c *Client) downloadPaperFromSciDB(ctx context.Context, paper *Paper, req DownloadDOIRequest) (*DownloadResult, error) {
	if paper == nil {
		return nil, httpx.NewError(httpx.CodeInvalidArgument, "invalid_request", "paper is required")
	}
	var base string
	sourceURL := paper.DownloadURL
	var expectedMD5 string
	if sourceURL == "" {
		if _, ok := ParseDOI(paper.DOI); !ok {
			return nil, httpx.NewError(httpx.CodeNotFound, "missing_doi", "no DOI is available for SciDB lookup")
		}
		var err error
		err = c.onMirror(ctx, func(mirrorBase string) error {
			base = mirrorBase
			sourceURL, expectedMD5, err = c.resolveSciDBFile(ctx, base, paper)
			return err
		})
		if err != nil {
			return nil, err
		}
	} else {
		var err error
		base, err = c.baseURLFor(ctx)
		if err != nil {
			return nil, err
		}
		var hashErr error
		expectedMD5, hashErr = normalizeHash(paper.Hash)
		if paper.Hash != "" && hashErr != nil {
			return nil, hashErr
		}
	}
	downloadURL, err := resolvePaperDownloadURL(base, sourceURL)
	if err != nil {
		return nil, err
	}
	name := paper.Title
	if name == "" {
		name = paper.DOI
	}
	format := req.Format
	if format == "" {
		format = "pdf"
	}
	res, transferErr := c.store.Download(ctx, store.TransferRequest{
		URL:         downloadURL,
		Title:       name,
		Format:      format,
		ExpectedMD5: expectedMD5,
		Validation:  store.ValidationPDF,
		AllowResume: true,
		Fetch:       c.fetchDownloadStream,
		Progress:    req.Progress,
	})
	if transferErr != nil {
		return nil, transferErr
	}
	return &DownloadResult{
		Path:   res.Path,
		Name:   res.Name,
		Bytes:  res.Bytes,
		MD5:    res.MD5,
		SHA256: res.SHA256,
		Reused: res.Reused,
	}, nil
}

// resolvePaperDownloadURL resolves a relative download link against its
// archive mirror. Absolute links must use HTTPS without credentials.
func resolvePaperDownloadURL(base, rawURL string) (string, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", httpx.NewError(httpx.CodeInvalidArgument, "invalid_url", "download URL is empty")
	}
	base = strings.TrimSpace(base)
	if !strings.Contains(base, "://") {
		base = "https://" + strings.TrimSuffix(base, "/")
	}
	baseURL, err := url.Parse(base)
	if err != nil || baseURL.Scheme != "https" || baseURL.Host == "" || baseURL.User != nil {
		return "", httpx.NewError(httpx.CodeInvalidArgument, "invalid_base", "download base URL must use HTTPS without credentials")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", httpx.WrapError(httpx.CodeInvalidArgument, "invalid_url", "invalid download URL", err)
	}
	resolved := parsed
	if !parsed.IsAbs() {
		resolved = baseURL.ResolveReference(parsed)
		if !httpx.SameOrigin(baseURL, resolved) {
			return "", httpx.NewError(httpx.CodeInvalidArgument, "invalid_url", "relative download URL must stay on the configured mirror")
		}
	}
	if resolved.Scheme != "https" || resolved.Host == "" || resolved.User != nil {
		return "", httpx.NewError(httpx.CodeInvalidArgument, "invalid_url", "download URL must use HTTPS without credentials")
	}
	return resolved.String(), nil
}
