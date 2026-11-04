package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Comparison statuses, from GitHub's compare endpoint. base is an ancestor
// of head exactly when the status is CompareAhead or CompareIdentical.
const (
	CompareAhead     = "ahead"
	CompareIdentical = "identical"
	CompareBehind    = "behind"
	CompareDiverged  = "diverged"
)

// CompareStatus says how head relates to base: CompareAhead, CompareBehind,
// CompareDiverged or CompareIdentical. A commit GitHub doesn't know, such as
// one dropped by a force-push and since collected, is an *APIError for which
// IsNotFound is true.
func (c *Client) CompareStatus(ctx context.Context, repo Repo, base, head string) (string, error) {
	var out struct {
		Status string `json:"status"`
	}
	// per_page=1 keeps the commit list, which isn't needed, short.
	if _, _, err := c.do(ctx, &request{
		method: http.MethodGet,
		url:    c.rest(repo.segments("compare", base+"..."+head)...) + "?per_page=1",
		out:    &out,
	}); err != nil {
		return "", fmt.Errorf("compare %s...%s in %s: %w", short(base), short(head), repo, err)
	}
	if out.Status == "" {
		return "", fmt.Errorf("compare %s...%s in %s: response has no status", short(base), short(head), repo)
	}
	return out.Status, nil
}

// CompareDiff fetches and parses the diff from the merge base of base and
// head to head. A diff GitHub won't render wraps ErrDiffTooLarge.
func (c *Client) CompareDiff(ctx context.Context, repo Repo, base, head string) (*Diff, error) {
	_, body, err := c.do(ctx, &request{
		method: http.MethodGet,
		url:    c.rest(repo.segments("compare", base+"..."+head)...),
		accept: "application/vnd.github.diff",
	})
	if err != nil {
		var e *APIError
		if errors.As(err, &e) && e.Status == http.StatusNotAcceptable {
			err = fmt.Errorf("%w: %w", ErrDiffTooLarge, err)
		}
		return nil, fmt.Errorf("compare diff %s...%s in %s: %w", short(base), short(head), repo, err)
	}
	d, err := ParseDiff(string(body))
	if err != nil {
		return nil, fmt.Errorf("parse compare diff %s...%s in %s: %w", short(base), short(head), repo, err)
	}
	return d, nil
}

// CommitSHA resolves ref (a branch, tag or SHA) to the full SHA of the
// commit it names now. A ref GitHub doesn't know is an *APIError for which
// IsNotFound is true.
func (c *Client) CommitSHA(ctx context.Context, repo Repo, ref string) (string, error) {
	_, body, err := c.do(ctx, &request{
		method: http.MethodGet,
		url:    c.rest(repo.segments("commits", ref)...),
		accept: "application/vnd.github.sha",
	})
	if err != nil {
		return "", fmt.Errorf("resolve %s in %s: %w", ref, repo, err)
	}
	sha := strings.TrimSpace(string(body))
	if !IsFullSHA(sha) {
		return "", fmt.Errorf("resolve %s in %s: response is not a commit SHA: %.80q", ref, repo, sha)
	}
	return sha, nil
}

// IsFullSHA says whether s is a full, lowercase hex commit SHA.
func IsFullSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
