package aakb

import (
	"context"
	"errors"
	"os"
	"strings"

	"github.com/bjoernaagaard/aakb/internal/httpx"
)

// DownloadRequest selects exactly one of Hash, DOI, and OpenAlexID.
// Title and Format control the filename. Artifact ("pdf" or "tei_xml"),
// LocationID and Version ("published", "accepted" or "submitted") choose
// among the copies of an OpenAlex work and need OpenAlexID. Progress, when
// set, is called from the downloading goroutine with the bytes written and
// the total, which is zero when the server does not report a size.
type DownloadRequest struct {
	Hash, DOI, OpenAlexID         string
	Title, Format                 string
	Artifact, LocationID, Version string
	Progress                      func(done, total int64)
}

// Verification describes the digest evidence for a downloaded file.
type Verification string

const (
	// VerificationMD5 means the file matches the archive's expected digest.
	VerificationMD5 Verification = "md5"
	// VerificationSHA256 is a locally computed fingerprint for an OpenAlex file.
	VerificationSHA256 Verification = "sha256"
	// VerificationNone means no digest is available.
	VerificationNone Verification = "none"
)

// DownloadResult identifies the downloaded file and its verification.
type DownloadResult struct {
	Path          string       `json:"path"`
	Name          string       `json:"name"`
	Bytes         int64        `json:"bytes"`
	Verification  Verification `json:"verification"`
	MD5           string       `json:"md5,omitempty"`
	SHA256        string       `json:"sha256,omitempty"`
	Reused        bool         `json:"reused"`
	DownloadsLeft *int         `json:"downloads_left,omitempty"`
}

// Download fetches a file or reuses a matching local copy.
// If ctx has no deadline, Download applies a 30-minute deadline.
func (c *Client) Download(ctx context.Context, req DownloadRequest) (*DownloadResult, error) {
	const op = "download"
	ctx, cancel := withDefaultTimeout(ctx, defaultDownloadTimeout)
	defer cancel()
	if err := classify(ctx, op, nil); err != nil {
		return nil, err
	}
	req.Hash = strings.TrimSpace(req.Hash)
	req.DOI = strings.TrimSpace(req.DOI)
	req.OpenAlexID = strings.TrimSpace(req.OpenAlexID)
	given := 0
	for _, id := range []string{req.Hash, req.DOI, req.OpenAlexID} {
		if id != "" {
			given++
		}
	}
	if given != 1 {
		return nil, newError(op, CodeInvalidArgument, "give exactly one of hash, doi or openalex_id")
	}
	if req.OpenAlexID == "" && strings.TrimSpace(req.Artifact+req.LocationID+req.Version) != "" {
		return nil, newError(op, CodeInvalidArgument, "artifact, location_id and version need openalex_id")
	}
	if req.OpenAlexID != "" {
		if c.papers == nil {
			return nil, newError(op, CodeConfig, "OpenAlexKey is required for an OpenAlex download")
		}
		out, err := c.papers.Download(ctx, openAlexDownloadRequest{
			OpenAlexID: req.OpenAlexID,
			Title:      req.Title,
			Format:     req.Format,
			Artifact:   req.Artifact,
			LocationID: req.LocationID,
			Version:    req.Version,
			Progress:   req.Progress,
		})
		if err := classify(ctx, op, err); err != nil {
			return nil, err
		}
		return out, nil
	}
	if req.Title == "" {
		// A failed metadata lookup leaves the filename choice to the provider.
		if req.DOI != "" {
			if paper, err := c.archive.LookupDOI(ctx, req.DOI); err == nil {
				req.Title = paper.Title
			}
		} else if details, err := c.archive.Details(ctx, req.Hash); err == nil {
			req.Title = details.Title
		}
		if err := classify(ctx, op, nil); err != nil {
			return nil, err
		}
	}
	archiveReq := archiveDownloadRequest{
		Hash:     req.Hash,
		DOI:      req.DOI,
		Title:    req.Title,
		Format:   req.Format,
		Progress: req.Progress,
	}
	if req.DOI != "" {
		out, err := downloadDOIWithFallback(ctx, c.archive, c.doiMirror, archiveReq)
		if err := classify(ctx, op, err); err != nil {
			return nil, err
		}
		return out, nil
	}
	out, err := c.archive.DownloadHash(ctx, archiveReq)
	if err := classify(ctx, op, err); err != nil {
		return nil, err
	}
	return out, nil
}

// downloadDOIWithFallback tries the primary archive first, then falls back to
// the optional DOI mirror on non-terminal archive failures (including when the
// primary archive is unconfigured).
func downloadDOIWithFallback(ctx context.Context, archive archiveProvider, mirror doiMirrorProvider, req archiveDownloadRequest) (*DownloadResult, error) {
	out, archiveErr := archive.DownloadDOI(ctx, req)
	if archiveErr == nil {
		return out, nil
	}
	if mirror == nil || !doiMirrorFallbackAllowed(ctx, archiveErr) {
		return nil, archiveErr
	}
	title := req.Title
	if title == "" {
		title = req.ExpectedTitle
	}
	mirrorOut, mirrorErr := mirror.DownloadDOI(ctx, doiMirrorDownloadRequest{
		DOI:      req.DOI,
		Title:    title,
		Format:   req.Format,
		Progress: req.Progress,
	})
	if mirrorErr == nil {
		return mirrorOut, nil
	}
	return nil, chooseDOIFallbackError(archiveErr, mirrorErr)
}

func doiMirrorFallbackAllowed(ctx context.Context, err error) bool {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if pathErr, ok := errors.AsType[*os.PathError](err); ok && pathErr != nil {
		return false
	}
	if pe, ok := errors.AsType[*httpx.ProviderError](err); ok && pe != nil {
		if pe.Reason == "local_io" || pe.Code == httpx.CodeInvalidArgument {
			return false
		}
	}
	return true
}

func chooseDOIFallbackError(archiveErr, mirrorErr error) error {
	if errors.Is(mirrorErr, context.Canceled) || errors.Is(mirrorErr, context.DeadlineExceeded) {
		return mirrorErr
	}
	if ape, ok := errors.AsType[*httpx.ProviderError](archiveErr); ok && ape != nil && ape.Code == httpx.CodeConfig {
		return mirrorErr
	}
	if mpe, ok := errors.AsType[*httpx.ProviderError](mirrorErr); ok && mpe != nil && mpe.Code == httpx.CodeNotFound {
		return archiveErr
	}
	return mirrorErr
}
