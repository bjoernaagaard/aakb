package aakb

import (
	"context"
	"path/filepath"

	"github.com/bjoernaagaard/aakb/internal/doi"
	"github.com/bjoernaagaard/aakb/internal/store"
)

type doiDownloadClient interface {
	DownloadDOI(ctx context.Context, req doi.DownloadRequest) (*store.Result, error)
}

type remoteDOIDownloadProvider struct {
	client doiDownloadClient
}

func newDOIDownloadProvider(client doiDownloadClient) doiDownloadProvider {
	return &remoteDOIDownloadProvider{client: client}
}

func (p *remoteDOIDownloadProvider) DownloadDOI(ctx context.Context, req doiDownloadRequest) (*DownloadResult, error) {
	res, err := p.client.DownloadDOI(ctx, req)
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
