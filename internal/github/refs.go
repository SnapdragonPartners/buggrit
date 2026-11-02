package github

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// The calls in this file set up and tear down a replay pull request for the
// benchmark harness (cmd/buggrit-bench, design §12): a branch at a commit,
// a draft PR between two such branches, and closing and deleting them
// afterwards. Reviews never make them.

// CreateBranch creates branch name (without "refs/heads/") at sha.
func (c *Client) CreateBranch(ctx context.Context, repo Repo, name, sha string) error {
	if _, _, err := c.do(ctx, &request{
		method: http.MethodPost,
		url:    c.rest(repo.segments("git", "refs")...),
		body:   map[string]string{"ref": "refs/heads/" + name, "sha": sha},
	}); err != nil {
		return fmt.Errorf("create branch %s at %s in %s: %w", name, sha, repo, err)
	}
	return nil
}

// DeleteBranch deletes branch name (without "refs/heads/"). A branch that
// doesn't exist is an error IsNotFound reports.
func (c *Client) DeleteBranch(ctx context.Context, repo Repo, name string) error {
	segs := append([]string{"git", "refs", "heads"}, strings.Split(name, "/")...)
	if _, _, err := c.do(ctx, &request{method: http.MethodDelete, url: c.rest(repo.segments(segs...)...)}); err != nil {
		return fmt.Errorf("delete branch %s in %s: %w", name, repo, err)
	}
	return nil
}

// NewPull is a pull request to open.
type NewPull struct {
	Title string
	Body  string
	// Head and Base are branch names in the same repository.
	Head  string
	Base  string
	Draft bool
}

// CreatePull opens a pull request and returns its number.
func (c *Client) CreatePull(ctx context.Context, repo Repo, p *NewPull) (int, error) {
	var out struct {
		Number int `json:"number"`
	}
	if _, _, err := c.do(ctx, &request{
		method: http.MethodPost,
		url:    c.rest(repo.segments("pulls")...),
		body:   map[string]any{"title": p.Title, "body": p.Body, "head": p.Head, "base": p.Base, "draft": p.Draft},
		out:    &out,
	}); err != nil {
		return 0, fmt.Errorf("open a pull request %s → %s in %s: %w", p.Head, p.Base, repo, err)
	}
	if out.Number <= 0 {
		return 0, fmt.Errorf("open a pull request in %s: no number in the response", repo)
	}
	return out.Number, nil
}

// ClosePull closes a pull request without merging it.
func (c *Client) ClosePull(ctx context.Context, repo Repo, number int) error {
	if _, _, err := c.do(ctx, &request{
		method: http.MethodPatch,
		url:    c.rest(repo.segments("pulls", strconv.Itoa(number))...),
		body:   map[string]string{"state": "closed"},
	}); err != nil {
		return fmt.Errorf("close %s#%d: %w", repo, number, err)
	}
	return nil
}
