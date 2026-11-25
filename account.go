package aakb

import "context"

// Account describes Anna's Archive membership and fast-download allowance.
// A nil SearchAllowed means the provider did not report sufficient information.
type Account struct {
	Member         bool     `json:"member"`
	DownloadsLeft  int      `json:"downloads_left"`
	DownloadsLimit int      `json:"downloads_limit,omitempty"`
	WindowHours    int      `json:"window_hours,omitempty"`
	Memberships    []string `json:"memberships,omitempty"`
	SearchAllowed  *bool    `json:"search_allowed"`
}

// AccountStatus returns the configured archive account's current allowance.
// If ctx has no deadline, AccountStatus applies a 60-second deadline.
func (c *Client) AccountStatus(ctx context.Context) (*Account, error) {
	const op = "account"
	ctx, cancel := withDefaultTimeout(ctx, defaultTimeout)
	defer cancel()
	status, err := c.archive.AccountStatus(ctx, 0)
	if err := classify(ctx, op, err); err != nil {
		return nil, err
	}
	return accountResult(status), nil
}
