package review

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"

	"github.com/SnapdragonPartners/buggrit/internal/config"
	"github.com/SnapdragonPartners/buggrit/internal/github"
	"github.com/SnapdragonPartners/buggrit/internal/glob"
	"github.com/SnapdragonPartners/buggrit/internal/state"
)

// GitHub is what the engine reads from GitHub. *github.Client implements it;
// tests use githubtest. Publishing is internal/publish's job.
type GitHub interface {
	PullRequest(ctx context.Context, repo github.Repo, number int) (*github.PullRequest, error)
	Diff(ctx context.Context, repo github.Repo, number int) (*github.Diff, error)
	ReviewThreads(ctx context.Context, repo github.Repo, number int) ([]github.Thread, error)
	Reviews(ctx context.Context, repo github.Repo, number int) ([]github.ReviewSummary, error)
	CompareStatus(ctx context.Context, repo github.Repo, base, head string) (string, error)
	CompareDiff(ctx context.Context, repo github.Repo, base, head string) (*github.Diff, error)
}

// PRRef names the pull request to review and the head the caller checked
// out as the Workspace.
type PRRef struct {
	Repo    github.Repo
	HeadSHA string
	Number  int
}

// Options are a pass's settings and model.
type Options struct {
	Config *config.Config
	// Find is the finder stage's chat client.
	Find llms.ChatClient
	// OnToolCall, if set, observes each tool call.
	OnToolCall func(toolloop.ToolCallEvent)
}

// Result is a pass's outcome, ready to publish.
type Result struct {
	PR *github.PullRequest
	// Find is the finder's run; nil when the pass was skipped.
	Find *FindResult
	// Skipped is set when there was nothing to do; SkipReason says why.
	SkipReason string
	// Reason says why an incomplete pass stopped.
	Reason string
	// Scope says what the pass looked for new findings in: the whole PR, or
	// the lines changed since the last completed pass (design §7 rule 2).
	Scope    string
	Summary  string
	HeadSHA  string
	Findings []Candidate
	Elapsed  time.Duration
	// Pass is this pass's number (design §6).
	Pass int
	// Duplicates and BelowThreshold count findings dropped by reconcile.
	Duplicates     int
	BelowThreshold int
	Skipped        bool
	// Complete is set when the finder covered its whole scope (§6, §9).
	Complete bool
}

// Review runs one pass over ref, whose head is checked out as ws: pick the
// mode, load the PR, find, reconcile (design §2). It doesn't publish.
//
// If the PR's head is no longer ref.HeadSHA, it returns a
// *github.HeadMovedError and does nothing: a newer run owns the PR (§3,
// "Snapshot").
func Review(ctx context.Context, ref PRRef, ws Workspace, gh GitHub, opts *Options) (*Result, error) {
	start := time.Now()
	cfg := opts.Config
	pr, err := gh.PullRequest(ctx, ref.Repo, ref.Number)
	if err != nil {
		return nil, fmt.Errorf("review: %w", err)
	}
	if pr.HeadSHA != ref.HeadSHA {
		return nil, &github.HeadMovedError{Reviewed: ref.HeadSHA, Current: pr.HeadSHA}
	}
	res := &Result{PR: pr, HeadSHA: ref.HeadSHA}

	reviews, err := gh.Reviews(ctx, ref.Repo, ref.Number)
	if err != nil {
		return nil, fmt.Errorf("review: %w", err)
	}
	baseline := state.ReadBaseline(reviews, cfg.Identity.BotLogin)
	res.Pass = baseline.LastPass + 1
	// Mode (§2): a full pass unless this head already has a completed one.
	// Reconcile-only passes (verifying earlier findings) arrive with the
	// verifier, build step 4.
	if baseline.CompletedAt(ref.HeadSHA) {
		res.Skipped = true
		res.SkipReason = "a pass already completed at " + short(ref.HeadSHA)
		res.Elapsed = time.Since(start)
		return res, nil
	}

	diff, err := gh.Diff(ctx, ref.Repo, ref.Number)
	if err != nil {
		return nil, fmt.Errorf("review: %w", err)
	}
	threads, err := gh.ReviewThreads(ctx, ref.Repo, ref.Number)
	if err != nil {
		return nil, fmt.Errorf("review: %w", err)
	}
	known, notes := readThreads(threads, cfg.Identity.BotLogin)

	scope := glob.Filter{Include: cfg.Review.Include, Exclude: cfg.Review.Exclude}
	inc, desc, err := passScope(ctx, gh, ref, baseline.CompletedSHA, diff, scope.InScope)
	if err != nil {
		return nil, fmt.Errorf("review: %w", err)
	}
	res.Scope = desc
	if inc.Empty() {
		// Nothing reviewable changed since the completed pass, say a merge
		// from the base branch: the pass is complete without the finder.
		res.Complete = true
		res.Summary = "Nothing reviewable changed since " + short(inc.Since) + ", the last completed review."
		res.Elapsed = time.Since(start)
		return res, nil
	}

	findCtx, cancel, wrapUp := findTimes(ctx, cfg.Limits.PassDeadline.Std())
	defer cancel()
	found, err := Find(findCtx, &FindInput{
		Client:           opts.Find,
		Workspace:        ws,
		Diff:             diff,
		OnToolCall:       opts.OnToolCall,
		PR:               Context{Number: pr.Number, Title: pr.Title, Body: pr.Body, BaseRef: pr.BaseRef, HeadSHA: pr.HeadSHA},
		Instructions:     cfg.Review.Instructions,
		InstructionFiles: cfg.Review.InstructionFiles,
		OtherThreads:     notes,
		Scope:            scope,
		Incremental:      inc,
		MaxIterations:    cfg.Limits.MaxToolIterations,
		WrapUp:           wrapUp,
	})
	if err != nil {
		return nil, fmt.Errorf("review: %w", err)
	}
	res.Find, res.Complete, res.Reason, res.Summary = found, found.Complete, found.Reason, found.Summary
	rec := reconcile(found.Findings, &reconcileInput{ws: ws, diff: diff, known: known, review: &cfg.Review})
	res.Findings, res.Duplicates, res.BelowThreshold = rec.kept, rec.duplicates, rec.belowThreshold
	res.Elapsed = time.Since(start)
	return res, nil
}

// findTimes bounds the finder by the pass deadline and works out its
// wrap-up reserve from the deadline that actually applies, which may be the
// caller's if it's sooner. With no deadline at all there's no reserve.
func findTimes(ctx context.Context, passDeadline time.Duration) (context.Context, context.CancelFunc, time.Duration) {
	findCtx, cancel := ctx, context.CancelFunc(func() {})
	if passDeadline > 0 {
		findCtx, cancel = context.WithTimeout(ctx, passDeadline)
	}
	deadline, ok := findCtx.Deadline()
	if !ok {
		return findCtx, cancel, 0
	}
	return findCtx, cancel, wrapUpReserve(time.Until(deadline))
}

// wrapUpReserve is how much of the pass deadline the finder holds back to
// make the model submit what it has (§9): a quarter of it, between one and
// ten minutes, since a reasoning model can spend minutes on one turn. It
// never takes more than half the deadline, so a short deadline still leaves
// time to investigate and complete.
func wrapUpReserve(deadline time.Duration) time.Duration {
	return min(max(deadline/4, time.Minute), 10*time.Minute, deadline/2)
}

// readThreads splits the PR's threads into the fingerprints of buggrit's own
// findings, for dedupe, and notes on every thread, so the finder doesn't
// raise them again. buggrit's own earlier findings are included, since a
// pass over the whole PR (the first, or one after a force-push) can meet
// them again.
func readThreads(threads []github.Thread, botLogin string) (known map[string]bool, notes []ThreadNote) {
	known = map[string]bool{}
	for i := range threads {
		t := &threads[i]
		root := t.Root()
		if root == nil {
			continue
		}
		body := root.Body
		if m, ok := state.OwnMarker(root, botLogin); ok && m.FP != "" {
			known[m.FP] = true
			body = firstLine(body)
		}
		line := t.Line
		if line == 0 {
			line = t.OriginalLine
		}
		notes = append(notes, ThreadNote{Path: t.Path, Line: line, Author: root.Author, Body: body, Resolved: t.IsResolved})
	}
	return known, notes
}

// firstLine is the first line of s, which for a buggrit finding is its
// severity and title.
func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// short abbreviates a commit SHA for messages.
func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
