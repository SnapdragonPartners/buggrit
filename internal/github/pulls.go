package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// PullRequest is the part of a PR a pass needs.
type PullRequest struct {
	Title   string
	Body    string
	Author  string
	State   string // "open" or "closed"
	HeadSHA string
	HeadRef string
	BaseSHA string
	BaseRef string
	NodeID  string
	Number  int
	Draft   bool
	Merged  bool
}

type restUser struct {
	Login string `json:"login"`
}

type restPR struct {
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	State  string   `json:"state"`
	NodeID string   `json:"node_id"`
	User   restUser `json:"user"`
	Head   struct {
		SHA string `json:"sha"`
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		SHA string `json:"sha"`
		Ref string `json:"ref"`
	} `json:"base"`
	Number int  `json:"number"`
	Draft  bool `json:"draft"`
	Merged bool `json:"merged"`
}

// PullRequest fetches a PR. Its HeadSHA is the PR's current head, which is
// what a pass pins itself to (design §3 "Snapshot").
func (c *Client) PullRequest(ctx context.Context, repo Repo, number int) (*PullRequest, error) {
	var pr restPR
	if _, _, err := c.do(ctx, &request{
		method: http.MethodGet,
		url:    c.rest(repo.segments("pulls", strconv.Itoa(number))...),
		out:    &pr,
	}); err != nil {
		return nil, fmt.Errorf("get pull request %s#%d: %w", repo, number, err)
	}
	if pr.Head.SHA == "" {
		return nil, fmt.Errorf("get pull request %s#%d: response has no head SHA", repo, number)
	}
	return &PullRequest{
		Number:  pr.Number,
		Title:   pr.Title,
		Body:    pr.Body,
		Author:  pr.User.Login,
		State:   pr.State,
		Draft:   pr.Draft,
		Merged:  pr.Merged,
		NodeID:  pr.NodeID,
		HeadSHA: pr.Head.SHA,
		HeadRef: pr.Head.Ref,
		BaseSHA: pr.Base.SHA,
		BaseRef: pr.Base.Ref,
	}, nil
}

// HeadSHA returns a PR's current head commit. It is what the publish fence
// re-reads (design §2).
func (c *Client) HeadSHA(ctx context.Context, repo Repo, number int) (string, error) {
	pr, err := c.PullRequest(ctx, repo, number)
	if err != nil {
		return "", err
	}
	return pr.HeadSHA, nil
}

// ErrDiffTooLarge means GitHub refused to render the PR's diff. Large PRs are
// handled by build step 6 (design §12), which works from the checkout instead.
var ErrDiffTooLarge = errors.New("pull request diff is too large for the GitHub API")

// DiffText fetches a PR's diff (base merge-base to head) as unified-diff text.
func (c *Client) DiffText(ctx context.Context, repo Repo, number int) (string, error) {
	_, body, err := c.do(ctx, &request{
		method: http.MethodGet,
		url:    c.rest(repo.segments("pulls", strconv.Itoa(number))...),
		accept: "application/vnd.github.diff",
	})
	if err != nil {
		var e *APIError
		if errors.As(err, &e) && e.Status == http.StatusNotAcceptable {
			return "", fmt.Errorf("get diff %s#%d: %w: %w", repo, number, ErrDiffTooLarge, err)
		}
		return "", fmt.Errorf("get diff %s#%d: %w", repo, number, err)
	}
	return string(body), nil
}

// Diff fetches and parses a PR's diff.
func (c *Client) Diff(ctx context.Context, repo Repo, number int) (*Diff, error) {
	text, err := c.DiffText(ctx, repo, number)
	if err != nil {
		return nil, err
	}
	d, err := ParseDiff(text)
	if err != nil {
		return nil, fmt.Errorf("parse diff %s#%d: %w", repo, number, err)
	}
	return d, nil
}

// ReviewComment is one inline comment in a new review. A zero Line makes it a
// file-level comment, which CreateReview posts separately (see there).
type ReviewComment struct {
	Body   string
	Anchor Anchor
}

// Review is a review to post. Event is always COMMENT (design §8).
type Review struct {
	Body     string
	CommitID string
	Comments []ReviewComment
}

// PostedReview reports what CreateReview created.
type PostedReview struct {
	// FileComments are the file-level comments, in the order given.
	FileComments []Comment
	// ID is the review's ID, or 0 if there was nothing to put in a review
	// (no body and only file-level comments).
	ID int64
}

type restReviewComment struct {
	Path      string `json:"path"`
	Body      string `json:"body"`
	Side      Side   `json:"side,omitempty"`
	StartSide Side   `json:"start_side,omitempty"`
	Line      int    `json:"line,omitempty"`
	StartLine int    `json:"start_line,omitempty"`
}

// CreateReview posts one COMMENT review holding every line-anchored comment,
// so a pass shows up as a single review with one thread per finding.
//
// GitHub's create-review endpoint has no file-level comments, so those are
// posted after it, one by one, as review comments with subject_type "file".
// Each still gets its own thread. If posting stops partway, the markers let
// the next pass see what landed and post the rest (design §2).
func (c *Client) CreateReview(ctx context.Context, repo Repo, number int, r *Review) (*PostedReview, error) {
	if r.CommitID == "" {
		return nil, errors.New("create review: commit ID is required so comments anchor to the reviewed head")
	}
	lineComments := make([]restReviewComment, 0, len(r.Comments))
	var fileComments []ReviewComment
	for _, rc := range r.Comments {
		if rc.Anchor.FileLevel() {
			fileComments = append(fileComments, rc)
			continue
		}
		side := rc.Anchor.Side
		if side == "" {
			side = SideRight
		}
		out := restReviewComment{Path: rc.Anchor.Path, Body: rc.Body, Line: rc.Anchor.Line, Side: side}
		if rc.Anchor.StartLine > 0 && rc.Anchor.StartLine < rc.Anchor.Line {
			out.StartLine, out.StartSide = rc.Anchor.StartLine, side
		}
		lineComments = append(lineComments, out)
	}

	posted := &PostedReview{}
	// GitHub rejects a COMMENT review with neither a body nor line comments.
	if len(lineComments) > 0 || r.Body != "" {
		id, err := c.postReview(ctx, repo, number, r, lineComments)
		if err != nil {
			return nil, err
		}
		posted.ID = id
	}
	for _, fc := range fileComments {
		cm, err := c.CreateFileComment(ctx, repo, number, r.CommitID, fc.Anchor.Path, fc.Body)
		if err != nil {
			return posted, err
		}
		posted.FileComments = append(posted.FileComments, *cm)
	}
	return posted, nil
}

func (c *Client) postReview(ctx context.Context, repo Repo, number int, r *Review, comments []restReviewComment) (int64, error) {
	var created struct {
		ID int64 `json:"id"`
	}
	if _, _, err := c.do(ctx, &request{
		method: http.MethodPost,
		url:    c.rest(repo.segments("pulls", strconv.Itoa(number), "reviews")...),
		body: map[string]any{
			"commit_id": r.CommitID,
			"body":      r.Body,
			"event":     "COMMENT",
			"comments":  comments,
		},
		out: &created,
	}); err != nil {
		return 0, fmt.Errorf("create review on %s#%d: %w", repo, number, err)
	}
	return created.ID, nil
}

// restComment is a REST review or issue comment.
type restComment struct {
	CreatedAt time.Time `json:"created_at"`
	User      restUser  `json:"user"`
	NodeID    string    `json:"node_id"`
	Body      string    `json:"body"`
	HTMLURL   string    `json:"html_url"`
	ID        int64     `json:"id"`
}

func (rc *restComment) comment() Comment {
	return Comment{
		ID:         rc.NodeID,
		DatabaseID: rc.ID,
		Author:     rc.User.Login,
		Body:       rc.Body,
		URL:        rc.HTMLURL,
		CreatedAt:  rc.CreatedAt,
	}
}

// CreateFileComment starts a file-level review thread on path.
func (c *Client) CreateFileComment(ctx context.Context, repo Repo, number int, commitID, path, body string) (*Comment, error) {
	var rc restComment
	if _, _, err := c.do(ctx, &request{
		method: http.MethodPost,
		url:    c.rest(repo.segments("pulls", strconv.Itoa(number), "comments")...),
		body: map[string]any{
			"commit_id":    commitID,
			"path":         path,
			"body":         body,
			"subject_type": "file",
		},
		out: &rc,
	}); err != nil {
		return nil, fmt.Errorf("create file comment on %s#%d %s: %w", repo, number, path, err)
	}
	cm := rc.comment()
	return &cm, nil
}

// Reply adds a comment to a review thread. commentID is the database ID of
// the thread's root comment: GitHub doesn't accept replies to replies.
func (c *Client) Reply(ctx context.Context, repo Repo, number int, commentID int64, body string) (*Comment, error) {
	var rc restComment
	if _, _, err := c.do(ctx, &request{
		method: http.MethodPost,
		url: c.rest(repo.segments("pulls", strconv.Itoa(number), "comments",
			strconv.FormatInt(commentID, 10), "replies")...),
		body: map[string]any{"body": body},
		out:  &rc,
	}); err != nil {
		return nil, fmt.Errorf("reply to comment %d on %s#%d: %w", commentID, repo, number, err)
	}
	cm := rc.comment()
	return &cm, nil
}

// DeleteReviewComment deletes a review comment. Only the live check uses it,
// to clean up after itself; buggrit never deletes findings.
func (c *Client) DeleteReviewComment(ctx context.Context, repo Repo, commentID int64) error {
	if _, _, err := c.do(ctx, &request{
		method: http.MethodDelete,
		url:    c.rest(repo.segments("pulls", "comments", strconv.FormatInt(commentID, 10))...),
	}); err != nil {
		return fmt.Errorf("delete review comment %d on %s: %w", commentID, repo, err)
	}
	return nil
}

// IssueComments lists a PR's conversation comments (not review comments),
// where the summary lives.
func (c *Client) IssueComments(ctx context.Context, repo Repo, number int) ([]Comment, error) {
	var out []Comment
	next := c.rest(repo.segments("issues", strconv.Itoa(number), "comments")...) + "?per_page=100"
	for next != "" {
		var page []restComment
		resp, _, err := c.do(ctx, &request{method: http.MethodGet, url: next, out: &page})
		if err != nil {
			return nil, fmt.Errorf("list issue comments on %s#%d: %w", repo, number, err)
		}
		for i := range page {
			out = append(out, page[i].comment())
		}
		next = nextLink(resp.Header.Get("Link"))
	}
	return out, nil
}

// CreateIssueComment posts a conversation comment on a PR.
func (c *Client) CreateIssueComment(ctx context.Context, repo Repo, number int, body string) (*Comment, error) {
	var rc restComment
	if _, _, err := c.do(ctx, &request{
		method: http.MethodPost,
		url:    c.rest(repo.segments("issues", strconv.Itoa(number), "comments")...),
		body:   map[string]any{"body": body},
		out:    &rc,
	}); err != nil {
		return nil, fmt.Errorf("create issue comment on %s#%d: %w", repo, number, err)
	}
	cm := rc.comment()
	return &cm, nil
}

// UpdateIssueComment replaces a conversation comment's body; the summary is
// updated in place (design §6).
func (c *Client) UpdateIssueComment(ctx context.Context, repo Repo, commentID int64, body string) (*Comment, error) {
	var rc restComment
	if _, _, err := c.do(ctx, &request{
		method: http.MethodPatch,
		url:    c.rest(repo.segments("issues", "comments", strconv.FormatInt(commentID, 10))...),
		body:   map[string]any{"body": body},
		out:    &rc,
	}); err != nil {
		return nil, fmt.Errorf("update issue comment %d on %s: %w", commentID, repo, err)
	}
	cm := rc.comment()
	return &cm, nil
}

// DeleteIssueComment deletes a conversation comment. Only the live check
// uses it, to clean up after itself.
func (c *Client) DeleteIssueComment(ctx context.Context, repo Repo, commentID int64) error {
	if _, _, err := c.do(ctx, &request{
		method: http.MethodDelete,
		url:    c.rest(repo.segments("issues", "comments", strconv.FormatInt(commentID, 10))...),
	}); err != nil {
		return fmt.Errorf("delete issue comment %d on %s: %w", commentID, repo, err)
	}
	return nil
}

// nextLink returns the rel="next" URL of a Link header, or "".
func nextLink(header string) string {
	for _, part := range strings.Split(header, ",") {
		target, params, ok := strings.Cut(part, ";")
		if !ok {
			continue
		}
		for _, p := range strings.Split(params, ";") {
			if strings.TrimSpace(p) == `rel="next"` {
				return strings.Trim(strings.TrimSpace(target), "<>")
			}
		}
	}
	return ""
}
