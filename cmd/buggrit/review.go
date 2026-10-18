package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/middleware"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"

	"github.com/SnapdragonPartners/buggrit/internal/config"
	"github.com/SnapdragonPartners/buggrit/internal/github"
	"github.com/SnapdragonPartners/buggrit/internal/llm"
	"github.com/SnapdragonPartners/buggrit/internal/publish"
	"github.com/SnapdragonPartners/buggrit/internal/review"
	"github.com/SnapdragonPartners/buggrit/internal/state"
)

// reviewDeps are review's seams to the outside world, replaced in tests:
// the checkout's HEAD and the stages' chat clients. clients reports each
// model attempt to obs.
type reviewDeps struct {
	head    func(dir string) (string, error)
	clients func(cfg *config.Config, obs middleware.Observer) (*llm.Clients, error)
}

func defaultReviewDeps() reviewDeps {
	return reviewDeps{
		head: gitHead,
		clients: func(cfg *config.Config, obs middleware.Observer) (*llm.Clients, error) {
			return llm.New(cfg, llm.WithObserver(obs)) //nolint:wrapcheck // already names the stage
		},
	}
}

// modelLog logs each model attempt, so a slow or failing endpoint shows in
// the job log.
type modelLog struct{ w io.Writer }

//nolint:gocritic // hugeParam: the signature is fixed by middleware.Observer
func (l modelLog) Observe(e middleware.Event) {
	if e.Err != nil {
		say(l.w, "buggrit: model %s failed after %s: %v\n", e.Model, e.Latency.Round(time.Millisecond), e.Err)
		return
	}
	say(l.w, "buggrit: model %s %s: %d in, %d out (%d reasoning)\n",
		e.Model, e.Latency.Round(time.Millisecond), e.Usage.InputTokens, e.Usage.OutputTokens, e.Usage.ReasoningTokens)
}

// reviewArgs are review's settings. Each flag defaults from the environment
// a GitHub Action provides, so the workflow needs none of them.
type reviewArgs struct {
	repo, apiURL, workspace, configPath, stepSummary, token string
	pr                                                      int
	dryRun                                                  bool
}

func parseReviewArgs(args []string, getenv func(string) string, stderr io.Writer) (*reviewArgs, error) {
	fs := flag.NewFlagSet("review", flag.ContinueOnError)
	fs.SetOutput(stderr)
	a := &reviewArgs{token: getenv("GITHUB_TOKEN"), stepSummary: getenv("GITHUB_STEP_SUMMARY")}
	pr, _ := strconv.Atoi(getenv("BUGGRIT_PR"))
	fs.StringVar(&a.repo, "repo", getenv("GITHUB_REPOSITORY"), "repository, owner/name (default $GITHUB_REPOSITORY)")
	fs.IntVar(&a.pr, "pr", pr, "pull request number (default $BUGGRIT_PR)")
	fs.StringVar(&a.workspace, "workspace", orDefault(getenv("GITHUB_WORKSPACE"), "."), "checkout of the PR head (default $GITHUB_WORKSPACE)")
	fs.StringVar(&a.apiURL, "api-url", orDefault(getenv("GITHUB_API_URL"), github.DefaultAPIURL), "GitHub API URL (default $GITHUB_API_URL)")
	fs.StringVar(&a.configPath, "config", "", "read the config from this file instead of the base branch (for local runs)")
	fs.BoolVar(&a.dryRun, "dry-run", false, "print the findings instead of posting them")
	if err := fs.Parse(args); err != nil {
		return nil, err //nolint:wrapcheck // flag already printed it
	}
	switch {
	case fs.NArg() > 0:
		return nil, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	case a.token == "":
		return nil, errors.New("GITHUB_TOKEN is not set")
	case a.repo == "":
		return nil, errors.New("no repository: set --repo or GITHUB_REPOSITORY")
	case a.pr <= 0:
		return nil, errors.New("no pull request: set --pr or BUGGRIT_PR")
	}
	return a, nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// runReview runs one pass and publishes it. Exit codes: 0 for a complete
// pass, a skipped one, or one that stopped because the head moved; 1 for an
// incomplete pass (including earlier findings left unverified) or an
// error, so it shows on the PR's checks.
func runReview(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	return runReviewWith(ctx, defaultReviewDeps(), args, getenv, stdout, stderr)
}

func runReviewWith(ctx context.Context, deps reviewDeps, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	a, err := parseReviewArgs(args, getenv, stderr)
	if err != nil {
		say(stderr, "buggrit review: %v\n", err)
		return 2
	}
	repo, err := github.ParseRepo(a.repo)
	if err != nil {
		say(stderr, "buggrit review: %v\n", err)
		return 2
	}
	gh := github.NewClient(a.token, github.WithBaseURL(a.apiURL))
	// The verifier and the finder run at once and both log.
	stdout, stderr = &lockedWriter{w: stdout}, &lockedWriter{w: stderr}
	r := &reviewer{args: a, deps: deps, repo: repo, gh: gh, stdout: stdout, stderr: stderr}
	code, summary := r.run(ctx)
	if a.stepSummary != "" && summary != "" {
		if err := appendFile(a.stepSummary, summary); err != nil {
			say(stderr, "buggrit review: write job summary: %v\n", err)
		}
	}
	return code
}

type reviewer struct {
	args   *reviewArgs
	deps   reviewDeps
	gh     *github.Client
	stdout io.Writer
	stderr io.Writer
	repo   github.Repo
}

// run does the pass and returns the exit code and the job summary.
func (r *reviewer) run(ctx context.Context) (int, string) {
	pr, err := r.gh.PullRequest(ctx, r.repo, r.args.pr)
	if err != nil {
		return r.fail(err)
	}
	// Snapshot (design §3): the checkout must be the PR's current head. If
	// it isn't, the head moved since the workflow looked it up, and the run
	// the push queued will review it.
	head, err := r.deps.head(r.args.workspace)
	if err != nil {
		return r.fail(err)
	}
	if head != pr.HeadSHA {
		return r.stop(fmt.Sprintf("the checkout is at %s but the PR head is now %s; a newer run will review it", short(head), short(pr.HeadSHA)))
	}

	cfg, err := r.loadConfig(ctx, pr)
	if errors.Is(err, errNoConfig) {
		return r.stop(err.Error())
	}
	if err != nil {
		return r.fail(err)
	}
	clients, err := r.deps.clients(cfg, modelLog{r.stderr})
	if err != nil {
		return r.fail(err)
	}
	ws, closeWS, err := review.OpenDir(r.args.workspace)
	if err != nil {
		return r.fail(err)
	}
	defer func() { _ = closeWS() }()

	ref := review.PRRef{Repo: r.repo, Number: pr.Number, HeadSHA: head}
	say(r.stderr, "buggrit: reviewing %s#%d at %s with %s\n", r.repo, pr.Number, short(head), cfg.Stage(config.StageFind).Model)
	opts := &review.Options{Config: cfg, Find: clients.Find, Verify: clients.Verify, OnToolCall: r.logTool}
	res, err := review.Review(ctx, ref, ws, r.gh, opts)
	if errors.Is(err, github.ErrHeadMoved) {
		return r.stop(err.Error())
	}
	if err != nil {
		return r.fail(err)
	}

	return r.finish(ctx, ref, res, cfg)
}

// finish publishes the pass, or prints it for a dry run, and reports it.
func (r *reviewer) finish(ctx context.Context, ref review.PRRef, res *review.Result, cfg *config.Config) (int, string) {
	if r.args.dryRun {
		r.print(res)
	} else {
		posted, err := publish.Publish(ctx, r.gh, ref, res)
		switch {
		case errors.Is(err, github.ErrHeadMoved):
			return r.stop(err.Error())
		case err != nil:
			return r.fail(err)
		case posted.FellBack:
			say(r.stderr, "buggrit: GitHub rejected a line anchor; findings were posted at file level\n")
		}
		for i := range posted.Manual {
			m := &posted.Manual[i]
			say(r.stderr, "buggrit: GitHub refused to change the resolution of %s; do it by hand: %s\n", m.ID, m.URL)
		}
	}
	say(r.stdout, "buggrit: %s\n", publish.Status(res))
	code := 0
	if !res.Done() {
		code = 1
	}
	return code, jobSummary(res, cfg)
}

// stop ends a run that has nothing to do; that isn't a failure.
func (r *reviewer) stop(reason string) (int, string) {
	say(r.stdout, "buggrit: nothing to do: %s\n", reason)
	return 0, "## buggrit\n\nNothing to do: " + reason + "\n"
}

func (r *reviewer) fail(err error) (int, string) {
	say(r.stderr, "buggrit review: %v\n", err)
	return 1, "## buggrit\n\nFailed: " + err.Error() + "\n"
}

var errNoConfig = errors.New("buggrit is not configured")

// loadConfig reads the config from the base branch, never the PR head, so a
// PR can't redirect the model endpoint (design §4, §10). --config overrides
// it for local runs.
func (r *reviewer) loadConfig(ctx context.Context, pr *github.PullRequest) (*config.Config, error) {
	var data []byte
	var err error
	source := r.args.configPath
	if source != "" {
		data, err = os.ReadFile(source) //nolint:gosec // the path is the operator's own argument
	} else {
		source = config.Path + " on " + pr.BaseRef
		data, err = r.gh.FileContent(ctx, r.repo, config.Path, pr.BaseRef)
		if github.IsNotFound(err) {
			return nil, fmt.Errorf("%w: no %s", errNoConfig, source)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg, err := config.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	return cfg, nil
}

//nolint:gocritic // hugeParam: the signature is fixed by toolloop's OnToolCall
func (r *reviewer) logTool(stage config.Stage, e toolloop.ToolCallEvent) {
	args := truncate(strings.Join(strings.Fields(string(e.Call.Parameters)), " "), 160)
	state := ""
	switch {
	case e.Err != nil:
		state = " (error: " + truncate(e.Err.Error(), 200) + ")"
	case e.Result.IsError:
		state = " (error: " + truncate(firstLine(e.Result.Content), 200) + ")"
	}
	say(r.stderr, "buggrit: [%s %d] %s %s%s %s\n", stage, e.Iteration, e.Call.Name, args, state, e.Latency.Round(time.Millisecond))
}

// print shows what a dry run would post.
func (r *reviewer) print(res *review.Result) {
	if res.Skipped {
		return
	}
	for i := range res.Transitions {
		tr := &res.Transitions[i]
		var lines []string
		if tr.Kind != "" {
			text, err := publish.TransitionBody(tr, res.HeadSHA, false)
			if err != nil {
				say(r.stderr, "buggrit: %v\n", err)
				continue
			}
			lines = append(lines, text)
		}
		if tr.Repair() {
			action := "unresolve"
			if tr.Want == state.Resolve {
				action = "resolve"
			}
			lines = append(lines, "(then "+action+" the thread)")
		}
		say(r.stdout, "=== reply on %s (%s)\n%s\n\n", tr.Tracked.ID, tr.Tracked.Thread.Path, strings.Join(lines, "\n"))
	}
	if res.ReconcileOnly {
		return
	}
	body, err := publish.ReviewBody(res, nil)
	if err != nil {
		say(r.stderr, "buggrit: %v\n", err)
	}
	say(r.stdout, "=== review body\n%s\n\n", body)
	for i := range res.Findings {
		c := &res.Findings[i]
		text, err := publish.FindingBody(c, res.Pass, res.HeadSHA)
		if err != nil {
			say(r.stderr, "buggrit: %v\n", err)
			continue
		}
		where := c.Anchor.Path
		if !c.Anchor.FileLevel() {
			where = fmt.Sprintf("%s:%d (%s)", c.Anchor.Path, c.Anchor.Line, c.Anchor.Side)
		}
		say(r.stdout, "=== %s\n%s\n\n", where, text)
	}
}

// jobSummary renders the pass for $GITHUB_STEP_SUMMARY (design §8).
func jobSummary(res *review.Result, cfg *config.Config) string {
	var b strings.Builder
	if res.ReconcileOnly {
		fmt.Fprintf(&b, "## buggrit\n\n**%s**\n\n%s already had a complete pass; this run only reconciled earlier findings.\n\n",
			publish.Status(res), short(res.HeadSHA))
	} else {
		fmt.Fprintf(&b, "## buggrit pass %d\n\n**%s**\n\n", res.Pass, publish.Status(res))
	}
	if res.Scope != "" {
		fmt.Fprintf(&b, "Scope: %s.\n\n", res.Scope)
	}
	if res.Find == nil && res.Verify == nil {
		return b.String()
	}
	b.WriteString("| Stage | Model | Tool iterations | Input tokens | Output tokens | Reasoning tokens |\n|---|---|---|---|---|---|\n")
	row := func(stage config.Stage, iterations int, u *llms.Usage) {
		fmt.Fprintf(&b, "| %s | %s | %d | %d | %d | %d |\n",
			stage, cfg.Stage(stage).Model, iterations, u.InputTokens, u.OutputTokens, u.ReasoningTokens)
	}
	if res.Verify != nil {
		row(config.StageVerify, res.Verify.Iterations, &res.Verify.Usage)
	}
	if res.Find != nil {
		row(config.StageFind, res.Find.Iterations, &res.Find.Usage)
	}
	fmt.Fprintf(&b, "\nWall time: %s.\n", res.Elapsed.Round(time.Second))
	if res.Duplicates+res.BelowThreshold > 0 {
		fmt.Fprintf(&b, "\nNot posted: %d already raised, %d below thresholds.\n", res.Duplicates, res.BelowThreshold)
	}
	return b.String()
}

// lockedWriter serializes writes from concurrent stages.
type lockedWriter struct {
	w  io.Writer
	mu sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p) //nolint:wrapcheck // a transparent wrapper
}

// gitHead returns the commit checked out in dir.
func gitHead(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("read the checkout's HEAD in %s: %w", dir, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func appendFile(path, text string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // the runner's own summary file
	if err != nil {
		return err //nolint:wrapcheck // the caller says what failed
	}
	_, werr := f.WriteString(text)
	cerr := f.Close()
	return errors.Join(werr, cerr) //nolint:wrapcheck // the caller says what failed
}

// signalContext is canceled by SIGINT or SIGTERM, which is how Actions
// cancels a job; the pass then reports itself incomplete.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
