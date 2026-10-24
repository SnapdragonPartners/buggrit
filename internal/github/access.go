package github

import (
	"context"
	"fmt"
	"net/http"
)

// Permission reads login's permission on repo: "admin", "write", "read" or
// "none" (GitHub folds maintain into write and triage into read). On a
// public repository every GitHub user has at least "read". The engine asks
// only when a comment's author association doesn't already settle whether
// the author is on the team, as for org members whose membership is
// private (design §1).
func (c *Client) Permission(ctx context.Context, repo Repo, login string) (string, error) {
	var out struct {
		Permission string `json:"permission"`
	}
	if _, _, err := c.do(ctx, &request{
		method: http.MethodGet,
		url:    c.rest(repo.segments("collaborators", login, "permission")...),
		out:    &out,
	}); err != nil {
		return "", fmt.Errorf("read %s's permission on %s: %w", login, repo, err)
	}
	return out.Permission, nil
}
