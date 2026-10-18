package publish

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/SnapdragonPartners/buggrit/internal/github"
	"github.com/SnapdragonPartners/buggrit/internal/review"
	"github.com/SnapdragonPartners/buggrit/internal/state"
)

// GitHub is what publishing writes to. *github.Client implements it.
type GitHub interface {
	github.HeadReader
	CreateReview(ctx context.Context, repo github.Repo, number int, r *github.Review) (*github.PostedReview, error)
	CreateFileComment(ctx context.Context, repo github.Repo, number int, commitID, path, body string) (*github.Comment, error)
	Reply(ctx context.Context, repo github.Repo, number int, commentID int64, body string) (*github.Comment, error)
	ResolveThread(ctx context.Context, threadID string) error
	UnresolveThread(ctx context.Context, threadID string) error
	ThreadResolved(ctx context.Context, threadID string) (bool, error)
}

// Posted reports what Publish created.
type Posted struct {
	// Manual lists the threads GitHub wouldn't resolve or unresolve for
	// buggrit's token, for a person to do by hand (design §3).
	Manual []Manual
	// Threads is the number of finding threads started.
	Threads int
	// Replies is the number of transition replies posted, and Resolved and
	// Unresolved the threads whose resolution changed.
	Replies, Resolved, Unresolved int
	// ReviewID is the pass's review.
	ReviewID int64
	// FellBack is set when GitHub rejected a line anchor and every finding
	// was posted at file level instead.
	FellBack bool
}

// Manual is a thread whose resolution must be changed by hand.
type Manual struct {
	ID  string
	URL string
	// Resolve is true for a thread to resolve, false for one to reopen.
	Resolve bool
	// New is set when the refusal follows a transition posted this run,
	// rather than a repair retried from an earlier one.
	New bool
}

// Publish posts a pass. First the transitions of earlier findings: each
// reply with its marker, then the resolve or unresolve. Then, for a full
// pass, each new finding as its own thread, and last the review whose body
// carries the pass marker. The marker goes last, so a pass that stops
// partway isn't recorded as complete, and the next run redoes it; dedupe
// against the posted threads' fingerprints keeps it from posting them twice
// (design §2, §6). A run killed between a reply and its resolve is
// repaired by the next one.
//
// It checks the fence first and before each state-changing mutation. If
// the head moved, it returns an error matching github.ErrHeadMoved and
// posts nothing more: a newer run owns the PR.
func Publish(ctx context.Context, gh GitHub, ref review.PRRef, res *review.Result) (*Posted, error) {
	out := &Posted{}
	if res.Skipped {
		return out, nil
	}
	fence := github.NewFence(gh, ref.Repo, ref.Number, res.HeadSHA)
	if err := fence.Check(ctx); err != nil {
		return nil, fmt.Errorf("publish: %w", err)
	}
	for i := range res.Transitions {
		if err := publishTransition(ctx, gh, fence, ref, res, &res.Transitions[i], out); err != nil {
			return out, err
		}
	}
	if res.ReconcileOnly {
		return out, publishManual(ctx, gh, fence, ref, res, out)
	}
	if len(res.Transitions) > 0 {
		// The transitions took time; check again before the new threads.
		if err := fence.Check(ctx); err != nil {
			return out, fmt.Errorf("publish: %w", err)
		}
	}
	return out, publishReview(ctx, gh, ref, res, out)
}

// publishReview posts a full pass's new findings, then its review.
func publishReview(ctx context.Context, gh GitHub, ref review.PRRef, res *review.Result, out *Posted) error {
	body, err := ReviewBody(res, out.Manual)
	if err != nil {
		return err
	}
	var lineFindings []review.Candidate
	var lines []github.ReviewComment
	for i := range res.Findings {
		c := &res.Findings[i]
		rc, rerr := comment(c, res)
		if rerr != nil {
			return rerr
		}
		if !c.Anchor.FileLevel() {
			lineFindings, lines = append(lineFindings, *c), append(lines, rc)
			continue
		}
		if perr := postFileLevel(ctx, gh, ref, res.HeadSHA, &rc); perr != nil {
			return perr
		}
		out.Threads++
	}
	rv, err := gh.CreateReview(ctx, ref.Repo, ref.Number, &github.Review{Body: body, CommitID: res.HeadSHA, Comments: lines})
	if err != nil && rejectedAnchor(err) && len(lines) > 0 {
		// The anchors come from the same diff GitHub serves, so this
		// shouldn't happen. If it does, the failed call posted nothing, and
		// file-level threads still reach the author. They're re-rendered
		// at file level, which drops any suggestion: with no lines to
		// replace, it can't be applied (design §8).
		out.FellBack = true
		for i := range lineFindings {
			c := lineFindings[i]
			c.Anchor = github.Anchor{Path: c.Anchor.Path}
			rc, rerr := comment(&c, res)
			if rerr != nil {
				return rerr
			}
			if perr := postFileLevel(ctx, gh, ref, res.HeadSHA, &rc); perr != nil {
				return perr
			}
			out.Threads++
		}
		lines = nil
		rv, err = gh.CreateReview(ctx, ref.Repo, ref.Number, &github.Review{Body: body, CommitID: res.HeadSHA})
	}
	if err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	out.Threads += len(lines)
	out.ReviewID = rv.ID
	return nil
}

// publishManual tells the PR about resolutions GitHub refused in a
// reconcile-only run, which posts no pass review to list them in (§3). It
// posts only for refusals that follow a transition from this run; a repair
// retried every run is already known, and stays in the job summary.
func publishManual(ctx context.Context, gh GitHub, fence *github.Fence, ref review.PRRef, res *review.Result, out *Posted) error {
	if !slices.ContainsFunc(out.Manual, func(m Manual) bool { return m.New }) {
		return nil
	}
	if err := fence.Check(ctx); err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	body, err := ManualBody(res, out.Manual)
	if err != nil {
		return err
	}
	rv, err := gh.CreateReview(ctx, ref.Repo, ref.Number, &github.Review{Body: body, CommitID: res.HeadSHA})
	if err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	out.ReviewID = rv.ID
	return nil
}

// publishTransition posts one transition's reply and fixes the thread's
// resolution, each behind the fence. Usually the reply goes first, so a run
// killed in between leaves the marker for the next run to repair from. A
// needs_human transition goes the other way: replay reads a resolution
// under its marker as a human settling the finding, so the thread must be
// unresolved before the marker lands (§6). That unresolve is sent whatever
// the thread looked like when it was read, since someone may have resolved
// it while the verifier ran. GitHub refusing a resolution is
// recorded in out.Manual, not an error (design §3).
func publishTransition(ctx context.Context, gh GitHub, fence *github.Fence, ref review.PRRef, res *review.Result, tr *review.Transition, out *Posted) error {
	if tr.Kind == state.KindNeedsHuman {
		refused, err := setResolution(ctx, gh, fence, tr, true, out)
		if err != nil {
			return err
		}
		return postReply(ctx, gh, fence, ref, res, tr, refused, out)
	}
	if err := postReply(ctx, gh, fence, ref, res, tr, false, out); err != nil {
		return err
	}
	_, err := setResolution(ctx, gh, fence, tr, false, out)
	return err
}

// postReply posts tr's transition reply, if it has one.
func postReply(ctx context.Context, gh GitHub, fence *github.Fence, ref review.PRRef, res *review.Result, tr *review.Transition, leftResolved bool, out *Posted) error {
	if tr.Kind == "" {
		return nil
	}
	t := tr.Tracked
	text, err := TransitionBody(tr, res.HeadSHA, leftResolved)
	if err != nil {
		return err
	}
	if err := fence.Check(ctx); err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	if _, err := gh.Reply(ctx, ref.Repo, ref.Number, t.Root.DatabaseID, text); err != nil {
		return fmt.Errorf("publish %s: %w", t.ID, err)
	}
	out.Replies++
	return nil
}

// setResolution resolves or unresolves tr's thread if it wasn't already as
// wanted when read, or always if always is set. refused reports that GitHub
// wouldn't let buggrit change a thread that needed changing. GitHub refuses
// by token, not by thread, so a refusal is checked against the thread as it
// is now: one already as wanted was refused nothing (§3).
func setResolution(ctx context.Context, gh GitHub, fence *github.Fence, tr *review.Transition, always bool, out *Posted) (refused bool, err error) {
	if tr.Want == state.Keep || !always && !tr.Repair() {
		return false, nil
	}
	if ferr := fence.Check(ctx); ferr != nil {
		return false, fmt.Errorf("publish: %w", ferr)
	}
	t := tr.Tracked
	resolve := tr.Want == state.Resolve
	if resolve {
		err = gh.ResolveThread(ctx, t.Thread.ID)
	} else {
		err = gh.UnresolveThread(ctx, t.Thread.ID)
	}
	switch {
	case github.IsForbidden(err):
		if cur, rerr := gh.ThreadResolved(ctx, t.Thread.ID); rerr == nil && cur == resolve {
			return false, nil
		}
		// Still not as wanted, or unknown: a person has to do it.
		out.Manual = append(out.Manual, Manual{ID: t.ID, URL: t.Root.URL, Resolve: resolve, New: tr.Kind != ""})
		return true, nil
	case err != nil:
		return false, fmt.Errorf("publish %s: %w", t.ID, err)
	case resolve:
		out.Resolved++
	default:
		out.Unresolved++
	}
	return false, nil
}

// comment renders c as a review comment at its anchor.
func comment(c *review.Candidate, res *review.Result) (github.ReviewComment, error) {
	text, err := FindingBody(c, res.Pass, res.HeadSHA)
	if err != nil {
		return github.ReviewComment{}, fmt.Errorf("publish %s: %w", c.ID, err)
	}
	return github.ReviewComment{Body: text, Anchor: c.Anchor}, nil
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
