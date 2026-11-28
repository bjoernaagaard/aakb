package aakb

import (
	"context"
	"path/filepath"

	"github.com/bjoernaagaard/aakb/internal/doimirror"
	"github.com/bjoernaagaard/aakb/internal/store"
	"github.com/bjoernaagaard/aakb/internal/unpaywall"
)

type doiDownloadFunc func(ctx context.Context, req doiDownloadRequest) (*store.Result, error)

func (f doiDownloadFunc) DownloadDOI(ctx context.Context, req doiDownloadRequest) (*DownloadResult, error) {
	res, err := f(ctx, req)
	if err != nil {
		return nil, err
	}
	name := res.Name
	if name == "" && res.Path != "" {
		name = filepath.Base(res.Path)
	}
	verification := VerificationNone
	if res.SHA256 != "" {
		verification = VerificationSHA256
	}
	return &DownloadResult{
		Path:         res.Path,
		Name:         name,
		Bytes:        res.Bytes,
		Verification: verification,
		SHA256:       res.SHA256,
		Reused:       res.Reused,
	}, nil
}

func newDOIMirrorProvider(client *doimirror.Client) doiDownloadProvider {
	return doiDownloadFunc(func(ctx context.Context, req doiDownloadRequest) (*store.Result, error) {
		return client.DownloadDOI(ctx, doimirror.DownloadRequest{
			DOI:      req.DOI,
			Title:    req.Title,
			Format:   req.Format,
			Progress: req.Progress,
		})
	})
}

func newUnpaywallProvider(client *unpaywall.Client) doiDownloadProvider {
	return doiDownloadFunc(func(ctx context.Context, req doiDownloadRequest) (*store.Result, error) {
		return client.DownloadDOI(ctx, unpaywall.DownloadRequest{
			DOI:      req.DOI,
			Title:    req.Title,
			Format:   req.Format,
			Progress: req.Progress,
		})
	})
}
