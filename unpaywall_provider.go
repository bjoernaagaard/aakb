package aakb

import (
	"context"
	"path/filepath"

	"github.com/bjoernaagaard/aakb/internal/unpaywall"
)

// remoteUnpaywallProvider implements unpaywallProvider on internal/unpaywall.
type remoteUnpaywallProvider struct {
	client *unpaywall.Client
}

func newUnpaywallProvider(client *unpaywall.Client) *remoteUnpaywallProvider {
	return &remoteUnpaywallProvider{client: client}
}

func (p *remoteUnpaywallProvider) DownloadDOI(ctx context.Context, req doiDownloadRequest) (*DownloadResult, error) {
	res, err := p.client.DownloadDOI(ctx, unpaywall.DownloadRequest{
		DOI:      req.DOI,
		Title:    req.Title,
		Format:   req.Format,
		Progress: req.Progress,
	})
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
