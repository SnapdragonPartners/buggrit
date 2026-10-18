package review

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
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

// Options are a pass's settings and models.
type Options struct {
	Config *config.Config
	// Find and Verify are the finder's and the verifier's chat clients.
	Find   llms.ChatClient
	Verify llms.ChatClient
	// OnToolCall, if set, observes each tool call, with the stage that made
	// it. With both stages running, it's called from both, concurrently.
	OnToolCall func(config.Stage, toolloop.ToolCallEvent)
}

// Result is a pass's outcome, ready to publish.
type Result struct {
	PR *github.PullRequest
	// Find is the finder's run; nil when the finder didn't run.
	Find *FindResult
	// Verify is the verifier's run; nil when nothing needed verifying.
	Verify *VerifyResult
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
	// Checks are the earlier findings selected for verification, and
	// Transitions what the pass does with earlier findings (§6).
	Checks      []Check
	Transitions []Transition
	// Unverified lists the checks the verifier didn't reach. They keep
	// their state and are selected again next run (§6).
	Unverified []string
	Elapsed    time.Duration
	// Pass is this pass's number (design §6).
	Pass int
	// Duplicates and BelowThreshold count findings dropped by reconcile.
	Duplicates     int
	BelowThreshold int
	Skipped        bool
	// ReconcileOnly is set when the head already had a complete pass: the
	// run verifies and publishes transitions, with no finder and no review
	// (§2, step 0).
	ReconcileOnly bool
	// Complete is set when the pass covered its whole scope for new
	// findings (§6, §9). It is what the pass marker records. Done also
	// needs every check verified.
	Complete bool
}

// Done reports whether the pass finished everything it set out to do: its
// whole scope searched and every selected finding verified. The status
// line says "incomplete" otherwise (§7 rule 6).
func (r *Result) Done() bool { return r.Skipped || r.Complete && len(r.Unverified) == 0 }

// Review runs one pass over ref, whose head is checked out as ws: pick the
// mode, load the PR and its history, verify earlier findings and find new
// ones, then reconcile (design §2). It doesn't publish.
//
// If the PR's head is no longer ref.HeadSHA, it returns a
// *github.HeadMovedError and does nothing: a newer run owns the PR (§3,
// "Snapshot").
func Review(ctx context.Context, ref PRRef, ws Workspace, gh GitHub, opts *Options) (*Result, error) {
	start := time.Now()
	p := &pass{ref: ref, ws: ws, gh: gh, opts: opts, cfg: opts.Config}
	res, err := p.run(ctx)
	if res != nil {
		res.Elapsed = time.Since(start)
	}
	return res, err
}

// pass is one run of Review.
type pass struct {
	ws       Workspace
	gh       GitHub
	opts     *Options
	cfg      *config.Config
	res      *Result
	diff     *github.Diff
	baseline state.Baseline
	threads  []github.Thread
	tracked  []state.Tracked
	ref      PRRef
}

func (p *pass) run(ctx context.Context) (*Result, error) {
	if err := p.load(ctx); err != nil {
		return nil, err
	}
	res, cfg := p.res, p.cfg
	// Mode (§2, step 0): a full pass unless this head already has a
	// completed one; then only earlier findings are reconciled.
	res.ReconcileOnly = p.baseline.CompletedAt(p.ref.HeadSHA)
	var recheck []string
	if res.ReconcileOnly {
		res.Complete = true
		recheck = p.baseline.UnverifiedAt(p.ref.HeadSHA)
	}
	res.Checks = selectChecks(ctx, p.gh, p.ref, p.tracked, res.ReconcileOnly, recheck)
	if res.ReconcileOnly && len(res.Checks) == 0 {
		// Settling and repairs need no model and no diff.
		res.Transitions = transitions(p.tracked, nil, nil, cfg.Convergence.MaxRebuttals)
		if len(res.Transitions) == 0 {
			res.Skipped = true
			res.SkipReason = "a pass already completed at " + short(p.ref.HeadSHA) + " and no earlier finding needs attention"
		}
		return res, nil
	}

	diff, err := p.gh.Diff(ctx, p.ref.Repo, p.ref.Number)
	if err != nil {
		return nil, fmt.Errorf("review: %w", err)
	}
	p.diff = diff
	fin, err := p.findInput(ctx)
	if err != nil {
		return nil, err
	}
	if res.Verify, res.Find, err = runStages(ctx, cfg.Limits.PassDeadline.Std(), p.verifyInput(), fin); err != nil {
		return nil, fmt.Errorf("review: %w", err)
	}
	p.collect()
	return res, nil
}

// load reads the PR, its passes and its threads, and replays the threads.
func (p *pass) load(ctx context.Context) error {
	pr, err := p.gh.PullRequest(ctx, p.ref.Repo, p.ref.Number)
	if err != nil {
		return fmt.Errorf("review: %w", err)
	}
	if pr.HeadSHA != p.ref.HeadSHA {
		return &github.HeadMovedError{Reviewed: p.ref.HeadSHA, Current: pr.HeadSHA}
	}
	p.res = &Result{PR: pr, HeadSHA: p.ref.HeadSHA}
	reviews, err := p.gh.Reviews(ctx, p.ref.Repo, p.ref.Number)
	if err != nil {
		return fmt.Errorf("review: %w", err)
	}
	p.baseline = state.ReadBaseline(reviews, p.cfg.Identity.BotLogin)
	p.res.Pass = p.baseline.LastPass + 1
	if p.threads, err = p.gh.ReviewThreads(ctx, p.ref.Repo, p.ref.Number); err != nil {
		return fmt.Errorf("review: %w", err)
	}
	p.tracked = state.Replay(p.threads, p.cfg.Identity.BotLogin)
	return nil
}

// onToolCall is opts.OnToolCall for one stage.
func (p *pass) onToolCall(stage config.Stage) func(toolloop.ToolCallEvent) {
	if p.opts.OnToolCall == nil {
		return nil
	}
	return func(e toolloop.ToolCallEvent) { p.opts.OnToolCall(stage, e) } //nolint:gocritic // hugeParam: fixed by toolloop
}

func (p *pass) context() Context {
	pr := p.res.PR
	return Context{Number: pr.Number, Title: pr.Title, Body: pr.Body, BaseRef: pr.BaseRef, HeadSHA: pr.HeadSHA}
}

// verifyInput is the verifier's input, or nil if nothing needs verifying.
func (p *pass) verifyInput() *VerifyInput {
	if len(p.res.Checks) == 0 {
		return nil
	}
	return &VerifyInput{
		Client:           p.opts.Verify,
		Workspace:        p.ws,
		Diff:             p.diff,
		OnToolCall:       p.onToolCall(config.StageVerify),
		PR:               p.context(),
		Instructions:     p.cfg.Review.Instructions,
		InstructionFiles: p.cfg.Review.InstructionFiles,
		Checks:           p.res.Checks,
		MaxRebuttals:     p.cfg.Convergence.MaxRebuttals,
		MaxIterations:    p.cfg.Limits.MaxToolIterations,
	}
}

// findInput works out the pass's scope and returns the finder's input, or
// nil if the finder doesn't run: in a reconcile-only run, or when nothing
// reviewable changed since the completed pass.
func (p *pass) findInput(ctx context.Context) (*FindInput, error) {
	res, cfg := p.res, p.cfg
	if res.ReconcileOnly {
		return nil, nil //nolint:nilnil // no finder is a normal outcome, documented above
	}
	scope := glob.Filter{Include: cfg.Review.Include, Exclude: cfg.Review.Exclude}
	inc, desc, err := passScope(ctx, p.gh, p.ref, p.baseline.CompletedSHA, p.diff, scope.InScope)
	if err != nil {
		return nil, fmt.Errorf("review: %w", err)
	}
	res.Scope = desc
	if inc.Empty() {
		// Nothing reviewable changed since the completed pass, say a merge
		// from the base branch: the search is complete without the finder.
		res.Complete = true
		res.Summary = "Nothing reviewable changed since " + short(inc.Since) + ", the last completed review."
		return nil, nil //nolint:nilnil // as above
	}
	_, notes := readThreads(p.threads, cfg.Identity.BotLogin)
	return &FindInput{
		Client:           p.opts.Find,
		Workspace:        p.ws,
		Diff:             p.diff,
		OnToolCall:       p.onToolCall(config.StageFind),
		PR:               p.context(),
		Instructions:     cfg.Review.Instructions,
		InstructionFiles: cfg.Review.InstructionFiles,
		OtherThreads:     notes,
		Scope:            scope,
		Incremental:      inc,
		MaxIterations:    cfg.Limits.MaxToolIterations,
	}, nil
}

// collect applies the state machine to the verdicts and reconciles the new
// findings.
func (p *pass) collect() {
	res, cfg := p.res, p.cfg
	var verdicts map[string]*state.Verdict
	if res.Verify != nil {
		verdicts = res.Verify.Verdicts
		res.Unverified = res.Verify.Unverified(res.Checks)
	}
	res.Transitions = transitions(p.tracked, res.Checks, verdicts, cfg.Convergence.MaxRebuttals)
	if f := res.Find; f != nil {
		res.Complete, res.Reason, res.Summary = f.Complete, f.Reason, f.Summary
		known, _ := readThreads(p.threads, cfg.Identity.BotLogin)
		rec := reconcile(f.Findings, &reconcileInput{ws: p.ws, diff: p.diff, known: known, review: &cfg.Review})
		res.Findings, res.Duplicates, res.BelowThreshold = rec.kept, rec.duplicates, rec.belowThreshold
	}
	if res.Verify != nil && !res.Verify.Complete {
		res.Reason = joinReasons(res.Reason, "verification: "+res.Verify.Reason)
	}
}

// runStages runs the verifier and the finder, whichever have input. They
// are independent and read-only, so they run at once and share the pass
// deadline; the limiter decides how many model calls actually run together,
// and with one lane they take turns (§9).
func runStages(ctx context.Context, passDeadline time.Duration, vin *VerifyInput, fin *FindInput) (*VerifyResult, *FindResult, error) {
	passCtx, cancel, wrapUp := passTimes(ctx, passDeadline)
	defer cancel()
	var wg sync.WaitGroup
	var vres *VerifyResult
	var fres *FindResult
	var verr, ferr error
	if vin != nil {
		if vin.Client == nil {
			return nil, nil, errors.New("no verifier client")
		}
		vin.WrapUp = wrapUp
		wg.Go(func() { vres, verr = Verify(passCtx, vin) })
	}
	if fin != nil {
		fin.WrapUp = wrapUp
		wg.Go(func() { fres, ferr = Find(passCtx, fin) })
	}
	wg.Wait()
	return vres, fres, errors.Join(verr, ferr) //nolint:wrapcheck // Verify and Find name their stage
}

// joinReasons joins the reasons two stages stopped early.
func joinReasons(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

// passTimes bounds the pass's stages by the pass deadline and works out
// their wrap-up reserve from the deadline that actually applies, which may
// be the caller's if it's sooner. With no deadline at all there's no
// reserve.
func passTimes(ctx context.Context, passDeadline time.Duration) (context.Context, context.CancelFunc, time.Duration) {
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

// wrapUpReserve is how much of the pass deadline a stage holds back to
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
