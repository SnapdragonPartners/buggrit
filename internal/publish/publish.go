package publish

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/SnapdragonPartners/buggrit/internal/github"
	"github.com/SnapdragonPartners/buggrit/internal/review"
)

// GitHub is what publishing writes to. *github.Client implements it.
type GitHub interface {
	github.HeadReader
	CreateReview(ctx context.Context, repo github.Repo, number int, r *github.Review) (*github.PostedReview, error)
	CreateFileComment(ctx context.Context, repo github.Repo, number int, commitID, path, body string) (*github.Comment, error)
}

// Posted reports what Publish created.
type Posted struct {
	// Threads is the number of finding threads started.
	Threads int
	// ReviewID is the pass's review.
	ReviewID int64
	// FellBack is set when GitHub rejected a line anchor and every finding
	// was posted at file level instead.
	FellBack bool
}

// Publish posts a pass: each finding as its own thread, then the review
// whose body carries the pass marker. The marker goes last, so a pass that
// stops partway isn't recorded as complete, and the next run redoes it;
// dedupe against the posted threads' fingerprints keeps it from posting them
// twice (design §2, §6).
//
// It checks the fence first. If the head moved, it returns an error matching
// github.ErrHeadMoved and posts nothing: a newer run owns the PR.
func Publish(ctx context.Context, gh GitHub, ref review.PRRef, res *review.Result) (*Posted, error) {
	if res.Skipped {
		return &Posted{}, nil
	}
	if err := github.NewFence(gh, ref.Repo, ref.Number, res.HeadSHA).Check(ctx); err != nil {
		return nil, fmt.Errorf("publish: %w", err)
	}
	body, err := ReviewBody(res)
	if err != nil {
		return nil, err
	}
	comments := make([]github.ReviewComment, 0, len(res.Findings))
	for i := range res.Findings {
		c := &res.Findings[i]
		text, rerr := FindingBody(c, res.Pass, res.HeadSHA)
		if rerr != nil {
			return nil, fmt.Errorf("publish %s: %w", c.ID, rerr)
		}
		comments = append(comments, github.ReviewComment{Body: text, Anchor: c.Anchor})
	}

	out := &Posted{}
	var lines []github.ReviewComment
	for i := range comments {
		c := &comments[i]
		if !c.Anchor.FileLevel() {
			lines = append(lines, *c)
			continue
		}
		if perr := postFileLevel(ctx, gh, ref, res.HeadSHA, c); perr != nil {
			return out, perr
		}
		out.Threads++
	}
	rv, err := gh.CreateReview(ctx, ref.Repo, ref.Number, &github.Review{Body: body, CommitID: res.HeadSHA, Comments: lines})
	if err != nil && rejectedAnchor(err) && len(lines) > 0 {
		// The anchors come from the same diff GitHub serves, so this
		// shouldn't happen. If it does, the failed call posted nothing, and
		// file-level threads still reach the author.
		out.FellBack = true
		for i := range lines {
			if perr := postFileLevel(ctx, gh, ref, res.HeadSHA, &lines[i]); perr != nil {
				return out, perr
			}
			out.Threads++
		}
		lines = nil
		rv, err = gh.CreateReview(ctx, ref.Repo, ref.Number, &github.Review{Body: body, CommitID: res.HeadSHA})
	}
	if err != nil {
		return out, fmt.Errorf("publish: %w", err)
	}
	out.Threads += len(lines)
	out.ReviewID = rv.ID
	return out, nil
}

// postFileLevel starts a file-level thread for c.
func postFileLevel(ctx context.Context, gh GitHub, ref review.PRRef, sha string, c *github.ReviewComment) error {
	if _, err := gh.CreateFileComment(ctx, ref.Repo, ref.Number, sha, c.Anchor.Path, c.Body); err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	return nil
}

// rejectedAnchor reports GitHub's 422 for a comment it couldn't place.
func rejectedAnchor(err error) bool {
	var e *github.APIError
	return errors.As(err, &e) && e.Status == http.StatusUnprocessableEntity
}
