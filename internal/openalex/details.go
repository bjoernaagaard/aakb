package openalex

import (
	"context"

	"github.com/bjoernaagaard/aakb/internal/httpx"
)

// Lookup resolves one work by W ID, OpenAlex work URL, DOI, or DOI URL. A DOI
// lookup that returns a different DOI fails instead of returning a stranger.
func (c *Client) Lookup(ctx context.Context, id string) (*Work, error) {
	canonical, err := CanonicalID(id)
	if err != nil {
		return nil, err
	}
	var work Work
	if err := c.get(ctx, "/works/"+canonical, nil, &work); err != nil {
		return nil, err
	}
	if _, err := CanonicalID(work.ID); err != nil || !IsWorkID(work.ID) {
		return nil, httpx.NewError(httpx.CodeUpstream, "invalid_response", "OpenAlex returned a work without a valid ID")
	}
	if doi := NormalizeDOI(id); doi != "" && !work.HasDOI(doi) {
		return nil, httpx.NewError(httpx.CodeUpstream, "identity_mismatch", "OpenAlex returned a different DOI")
	}
	return &work, nil
}
