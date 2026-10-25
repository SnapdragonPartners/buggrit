package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// FileContent reads a file from the repository at ref (a branch, tag or
// commit SHA). buggrit reads its config this way, from the base branch, so a
// PR can't change it (design §4). A missing file is an *APIError for which
// IsNotFound is true.
func (c *Client) FileContent(ctx context.Context, repo Repo, path, ref string) ([]byte, error) {
	segments := repo.segments("contents")
	segments = append(segments, strings.Split(path, "/")...)
	_, body, err := c.do(ctx, &request{
		method: http.MethodGet,
		url:    c.rest(segments...) + "?ref=" + url.QueryEscape(ref),
		accept: "application/vnd.github.raw+json",
	})
	if err != nil {
		return nil, fmt.Errorf("read %s at %s in %s: %w", path, ref, repo, err)
	}
	return body, nil
}

// ReviewSummary is a submitted review on a PR. buggrit's pass markers live in
// its review bodies (design §6, "Review baseline").
type ReviewSummary struct {
	SubmittedAt time.Time
	Author      string
	Body        string
	CommitSHA   string
	ID          int64
}

// Reviews lists a PR's reviews, oldest first.
func (c *Client) Reviews(ctx context.Context, repo Repo, number int) ([]ReviewSummary, error) {
	var out []ReviewSummary
	next := c.rest(repo.segments("pulls", strconv.Itoa(number), "reviews")...) + "?per_page=100"
	for next != "" {
		var page []struct {
			SubmittedAt time.Time `json:"submitted_at"`
			User        restUser  `json:"user"`
			Body        string    `json:"body"`
			CommitID    string    `json:"commit_id"`
			ID          int64     `json:"id"`
		}
		resp, _, err := c.do(ctx, &request{method: http.MethodGet, url: next, out: &page})
		if err != nil {
			return nil, fmt.Errorf("list reviews on %s#%d: %w", repo, number, err)
		}
		for i := range page {
			out = append(out, ReviewSummary{SubmittedAt: page[i].SubmittedAt, ID: page[i].ID, Author: page[i].User.Login, Body: page[i].Body, CommitSHA: page[i].CommitID})
		}
		next = nextLink(resp.Header.Get("Link"))
	}
	return out, nil
}
