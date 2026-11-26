package archive

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"

	xhtml "golang.org/x/net/html"

	"github.com/bjoernaagaard/aakb/internal/httpx"
)

// AccountStatus is the signed-in account's membership and fast download
// allowance. The archive counts fast downloads over a rolling window of
// WindowHours, and downloading a file again within it is free.
// SearchAllowed says whether a membership allows searching, which needs Lucky
// Librarian or higher; it is nil when the page does not show the tiers.
type AccountStatus struct {
	Member         bool     `json:"member"`
	DownloadsLeft  int      `json:"downloads_left"`
	DownloadsLimit int      `json:"downloads_limit,omitempty"`
	WindowHours    int      `json:"window_hours,omitempty"`
	Memberships    []string `json:"memberships,omitempty"`
	SearchAllowed  *bool    `json:"search_allowed,omitempty"`
}

// searchTier is the lowest membership tier allowed to search: Lucky
// Librarian. Brilliant Bookworm, tier 2, can only download.
const searchTier = 3

// AccountPage is what the account page shows: whether the session is signed
// in, the fast downloads used of the allowance over a window of WindowHours,
// and the active memberships with the tier number of each (0 when unknown).
type AccountPage struct {
	SignedIn    bool
	Used, Limit int
	WindowHours int
	Memberships []string
	Tiers       []int
}

var downloadsUsedPattern = regexp.MustCompile(`^\s*(\d+)\s*/\s*(\d+)\s*$`)

// atoiOrZero parses a decimal count the page guarantees to be numeric.
func atoiOrZero(text string) int {
	value, err := strconv.Atoi(text)
	if err != nil {
		return 0
	}
	return value
}

// ParseAccountPage reads the account page, which shows active memberships
// and "Fast downloads used (last 18 hours): 4 / 75".
func ParseAccountPage(root *xhtml.Node) AccountPage {
	var page AccountPage
	if root == nil {
		return page
	}
	page.SignedIn = findFirstNode(root, func(node *xhtml.Node) bool {
		return isElement(node, "a") && nodeAttr(node, "href") == "/account/secret_key"
	}) != nil
	for _, link := range findNodes(root, func(node *xhtml.Node) bool {
		return isElement(node, "a") && nodeAttr(node, "href") == "/account/downloaded"
	}) {
		line := link.Parent
		strong := findFirstNode(line, func(node *xhtml.Node) bool { return isElement(node, "strong") })
		match := downloadsUsedPattern.FindStringSubmatch(nodeText(strong))
		if match == nil {
			continue
		}
		page.Used = atoiOrZero(match[1])
		page.Limit = atoiOrZero(match[2])
		if hours := firstNumberPattern.FindString(nodeOwnText(line)); hours != "" {
			page.WindowHours = atoiOrZero(hours)
		}
		break
	}
	// Expired memberships stay listed with line-through.
	for _, link := range findNodes(root, func(node *xhtml.Node) bool {
		return isElement(node, "a") && strings.HasPrefix(nodeAttr(node, "href"), "/donate?tier=")
	}) {
		line := link.Parent
		tier := strings.Join(strings.Fields(nodeText(findFirstNode(line, func(node *xhtml.Node) bool {
			return isElement(node, "strong")
		}))), " ")
		if tier == "" || nodeHasClass(line, "line-through") {
			continue
		}
		text := strings.Join(strings.Fields(removeLinkText(line, link)), " ")
		if i := strings.Index(text, tier); i >= 0 {
			text = text[i:]
		}
		page.Memberships = append(page.Memberships, text)
		number := atoiOrZero(strings.TrimPrefix(nodeAttr(link, "href"), "/donate?tier="))
		page.Tiers = append(page.Tiers, number)
	}
	return page
}

// removeLinkText renders line without the membership link's own text.
func removeLinkText(line, link *xhtml.Node) string {
	if line == nil {
		return ""
	}
	var out strings.Builder
	var walk func(*xhtml.Node)
	walk = func(current *xhtml.Node) {
		if current == link {
			return
		}
		if current.Type == xhtml.TextNode {
			out.WriteString(current.Data)
		}
		for child := range current.ChildNodes() {
			walk(child)
		}
	}
	walk(line)
	return out.String()
}

// searchAllowed reads the membership tiers; it is nil when any is unknown.
func searchAllowed(tiers []int) *bool {
	if len(tiers) == 0 {
		return nil
	}
	allowed := false
	for _, tier := range tiers {
		if tier <= 0 {
			return nil
		}
		allowed = allowed || tier >= searchTier
	}
	return &allowed
}

// AccountStatus reads the account page, which shows active memberships and
// "Fast downloads used (last 18 hours): 4 / 75".
func (c *Client) AccountStatus(ctx context.Context) (*AccountStatus, error) {
	if err := c.requireArchiveAccess(ctx); err != nil {
		return nil, err
	}
	var status *AccountStatus
	err := c.onMirror(ctx, func(base string) error {
		var err error
		status, err = c.accountStatusOn(ctx, base)
		return err
	})
	return status, err
}

func (c *Client) accountStatusOn(ctx context.Context, base string) (*AccountStatus, error) {
	page, err := c.fetchAccountPage(ctx, base)
	if err != nil {
		return nil, err
	}
	status := &AccountStatus{
		Member:         page.Limit > 0,
		DownloadsLimit: page.Limit,
		WindowHours:    page.WindowHours,
		Memberships:    page.Memberships,
		SearchAllowed:  searchAllowed(page.Tiers),
	}
	if page.Limit > 0 {
		status.DownloadsLeft = max(0, page.Limit-page.Used)
		return status, nil
	}
	// No allowance line: the account has no membership, or the page changed.
	// The statistics endpoint answers either way.
	summary, err := c.FetchSummary(ctx, base, strings.Repeat("0", 32))
	if err != nil {
		return nil, err
	}
	status.Member, status.DownloadsLeft = summary.IsMember == 1, summary.DownloadsLeft
	return status, nil
}

// fetchAccountPage signs in again once if the archive no longer recognizes
// the session: the account page answers 200 either way.
func (c *Client) fetchAccountPage(ctx context.Context, base string) (AccountPage, error) {
	accountURL := buildPathURL(base, "/account/")
	for renewed := false; ; renewed = true {
		cookie := c.sessionCookie()
		root, err := c.fetchHTML(ctx, accountURL)
		if err != nil {
			return AccountPage{}, err
		}
		page := ParseAccountPage(root)
		if page.SignedIn {
			return page, nil
		}
		if !renewed {
			retry, err := c.signIn(ctx, cookie)
			if err != nil {
				return AccountPage{}, err
			}
			if retry {
				continue
			}
		}
		return AccountPage{}, httpx.NewError(httpx.CodeUpstreamBlocked, "signed_out", "The archive shows this session as signed out; check SecretKey, or set AccountCookie: "+manualCookieSteps)
	}
}

// explainSearchRefusal replaces a 403 on a search with the reason when the
// account page shows that no membership allows searching. Any other error,
// or a failure to read the account page, leaves err as it was.
func (c *Client) explainSearchRefusal(ctx context.Context, err error) error {
	var forbidden *forbiddenError
	if !errors.As(err, &forbidden) || ctx.Err() != nil {
		return err
	}
	var page AccountPage
	if c.onMirror(ctx, func(base string) error {
		var accountErr error
		page, accountErr = c.fetchAccountPage(ctx, base)
		return accountErr
	}) != nil {
		return err
	}
	if allowed := searchAllowed(page.Tiers); allowed == nil || *allowed {
		return err
	}
	return httpx.NewError(httpx.CodeUpstreamBlocked, "search_membership_required", "The archive refused the search because this account's membership ("+strings.Join(page.Memberships, "; ")+
		") can download but not search. Searching needs Lucky Librarian or higher, which skips the browser checks that searches otherwise get. Downloads by file hash or /md5/ page URL still work")
}
