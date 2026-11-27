package aakb

import (
	"context"
	"path/filepath"

	"github.com/bjoernaagaard/aakb/internal/doimirror"
)

// remoteDOIMirrorProvider implements doiMirrorProvider on internal/doimirror
// with result mapping to root types.
type remoteDOIMirrorProvider struct {
	client *doimirror.Client
}

func newDOIMirrorProvider(client *doimirror.Client) *remoteDOIMirrorProvider {
	return &remoteDOIMirrorProvider{client: client}
}

func (p *remoteDOIMirrorProvider) DownloadDOI(ctx context.Context, req doiDownloadRequest) (*DownloadResult, error) {
	res, err := p.client.DownloadDOI(ctx, doimirror.DownloadRequest{
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
