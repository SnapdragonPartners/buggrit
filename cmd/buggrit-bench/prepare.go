package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/SnapdragonPartners/buggrit/internal/bench"
	"github.com/SnapdragonPartners/buggrit/internal/github"
)

// identityCommit is the commit that added meta.mph to telemetry (#49). An
// arm built from a ref without it records no identity, so its runs can't
// be grouped (docs/telemetry.md, "Identity").
const identityCommit = "216d2fa09917f959db557db8dd44f3b91dbbbb34"

// matrixEntry is one review job: one run of one arm.
type matrixEntry struct {
	Arm string `json:"arm"`
	// Ref is the arm's ref as given, and Commit the SHA prepare resolved it
	// to, which the job builds: every run of an arm builds the same commit,
	// however late its job starts (#60).
	Ref    string `json:"ref"`
	Commit string `json:"commit"`
	Label  string `json:"label"`
	// Config is the arm's merged config, as JSON text.
	Config string `json:"config"`
	Repo   string `json:"repo"`
	Head   string `json:"head"`
	Run    int    `json:"run"`
	PR     int    `json:"pr"`
	// Timeout is the job's timeout-minutes.
	Timeout int `json:"timeout"`
}

// benchFlags are the flags commands share. Each command registers only
// the ones it uses.
type benchFlags struct {
	fs                                *flag.FlagSet
	getenv                            func(string) string
	apiURL, buggritRepo, output, root string
	workflowRun                       int64
	attempt                           int
}

func newFlags(name string, getenv func(string) string, stderr io.Writer) *benchFlags {
	f := &benchFlags{fs: flag.NewFlagSet(name, flag.ContinueOnError), getenv: getenv}
	f.fs.SetOutput(stderr)
	return f
}

// apiURLFlag registers --api-url, for the commands that call GitHub.
func (f *benchFlags) apiURLFlag() *benchFlags {
	f.fs.StringVar(&f.apiURL, "api-url", orDefault(f.getenv("GITHUB_API_URL"), github.DefaultAPIURL), "GitHub API URL (default $GITHUB_API_URL)")
	return f
}

// workflowRunFlags registers --workflow-run and --attempt.
func (f *benchFlags) workflowRunFlags() *benchFlags {
	runID, _ := strconv.ParseInt(f.getenv("GITHUB_RUN_ID"), 10, 64)
	attempt, _ := strconv.Atoi(f.getenv("GITHUB_RUN_ATTEMPT"))
	f.fs.Int64Var(&f.workflowRun, "workflow-run", runID, "the workflow run that ran the experiment (default $GITHUB_RUN_ID)")
	f.fs.IntVar(&f.attempt, "attempt", max(attempt, 1), "its attempt (default $GITHUB_RUN_ATTEMPT, else 1)")
	return f
}

// rootFlag registers --root.
func (f *benchFlags) rootFlag() *benchFlags {
	f.fs.StringVar(&f.root, "root", ".", "buggrit's checkout, which the experiment's paths are relative to")
	return f
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// runPrepare validates the experiment, checks every arm's ref records an
// identity, resolves each to a commit, opens the replay and writes the
// matrix, the commits and the replay as step outputs (or to stdout).
func runPrepare(ctx context.Context, d deps, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	f := newFlags("prepare", getenv, stderr).apiURLFlag().workflowRunFlags().rootFlag()
	path := f.fs.String("experiment", "", "the experiment file, or - for stdin (required)")
	f.fs.StringVar(&f.buggritRepo, "buggrit-repo", getenv("GITHUB_REPOSITORY"), "the repository arms' refs are in (default $GITHUB_REPOSITORY)")
	f.fs.StringVar(&f.output, "output", getenv("GITHUB_OUTPUT"), "append step outputs here (default $GITHUB_OUTPUT; else print them)")
	if err := f.fs.Parse(args); err != nil {
		return usageError{err}
	}
	switch {
	case f.fs.NArg() > 0:
		return usagef("unexpected arguments: %s", strings.Join(f.fs.Args(), " "))
	case *path == "":
		return usagef("--experiment is required")
	case f.workflowRun <= 0:
		return usagef("no workflow run: set --workflow-run or GITHUB_RUN_ID")
	case f.buggritRepo == "":
		return usagef("no buggrit repository: set --buggrit-repo or GITHUB_REPOSITORY")
	}
	token := getenv("GITHUB_TOKEN")
	if token == "" {
		return usagef("GITHUB_TOKEN is not set")
	}
	l, err := load(f.root, *path)
	if err != nil {
		return err
	}
	dbURL, err := databaseURL(getenv)
	if err != nil {
		return err
	}
	gh := d.github(token, f.apiURL)
	commits, err := resolveRefs(ctx, gh, f.buggritRepo, l.exp.Arms, stderr)
	if err != nil {
		return err
	}
	w := bench.WorkflowRun{ID: f.workflowRun, Attempt: f.attempt}
	if derr := checkDatabase(ctx, d, dbURL, w); derr != nil {
		return derr
	}

	t := l.target()
	replayer := d.replayer(gh)
	replay, err := replayer.Open(ctx, t, bench.Tag(l.exp.Name, w))
	if err != nil {
		return fmt.Errorf("open the replay: %w", err)
	}
	say(stderr, "buggrit-bench: replay %s#%d (%s...%s)\n", replay.Repo, replay.PR, replay.Base, replay.Head)

	include := make([]matrixEntry, 0, len(l.exp.Arms)*l.exp.Runs)
	for i := range l.exp.Arms {
		a := &l.exp.Arms[i]
		for n := 1; n <= l.exp.Runs; n++ {
			include = append(include, matrixEntry{
				Arm: a.Name, Ref: a.Ref, Commit: commits[a.Ref], Run: n, Label: l.exp.Label(a.Name), Config: string(l.configs[a.Name]),
				Repo: replay.Repo, PR: replay.PR, Head: t.Head, Timeout: l.timeouts[a.Name],
			})
		}
	}
	err = writeOutputs(f.output, stdout, map[string]any{
		"commits":        commits,
		"matrix":         map[string]any{"include": include},
		"parallel":       l.exp.Parallel,
		"report_timeout": l.reportTimeout(),
		"replay":         replay,
	})
	if err != nil {
		// Nothing downstream knows about the replay, so clean it up here.
		return errors.Join(err, replayer.Close(context.WithoutCancel(ctx), replay)) //nolint:wrapcheck // each says what failed
	}
	return nil
}

// checkDatabase checks, before any model time is spent, that the database
// takes this login, is migrated for the harness, and lets it write the
// runs' telemetry and the scores. A review that can't use the database
// carries on without it (design §6), so a run would otherwise record
// nothing. Each review job also checks its own login.
//
// It also checks no experiment is recorded under w yet: report files the
// scores under the experiment recorded for its workflow run and attempt,
// so a second one there would score its runs under the first. An Actions
// run's prepare always comes before its report, but a local run's ID is
// chosen by hand (scripts/bench-local.sh).
func checkDatabase(ctx context.Context, d deps, url string, w bench.WorkflowRun) error {
	db, err := d.openStore(ctx, url)
	if err != nil {
		return fmt.Errorf("the database: %w", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.CheckWrites(ctx); err != nil {
		return fmt.Errorf("the database: %w", err)
	}
	switch rec, err := db.LoadExperiment(ctx, w); {
	case err == nil:
		return fmt.Errorf("workflow run %d attempt %d already recorded experiment %s; give this one a workflow run of its own",
			w.ID, w.Attempt, rec.Experiment.Name)
	case !errors.Is(err, bench.ErrNoExperiment):
		return fmt.Errorf("the database: %w", err)
	}
	return nil
}

// resolveRefs resolves each arm's ref to the commit it names now, once
// per distinct ref, and checks that commit contains identityCommit. It
// logs and returns the commits by ref.
func resolveRefs(ctx context.Context, gh *github.Client, repoName string, arms []bench.Arm, stderr io.Writer) (map[string]string, error) {
	repo, err := github.ParseRepo(repoName)
	if err != nil {
		return nil, err //nolint:wrapcheck // names the repo
	}
	commits := map[string]string{}
	for i := range arms {
		a := &arms[i]
		if commits[a.Ref] != "" {
			continue
		}
		sha, err := gh.CommitSHA(ctx, repo, a.Ref)
		if err != nil {
			return nil, fmt.Errorf("arm %s: ref %s: %w", a.Name, a.Ref, err)
		}
		st, err := gh.CompareStatus(ctx, repo, identityCommit, sha)
		if err != nil {
			return nil, fmt.Errorf("arm %s: ref %s (%s): %w", a.Name, a.Ref, short(sha), err)
		}
		if st != github.CompareAhead && st != github.CompareIdentical {
			return nil, fmt.Errorf("arm %s: ref %s doesn't contain %s, which records the run's identity (#49); rebase it on main", a.Name, a.Ref, short(identityCommit))
		}
		commits[a.Ref] = sha
		say(stderr, "buggrit-bench: ref %s @ %s\n", a.Ref, sha)
	}
	return commits, nil
}

// writeOutputs appends name=JSON lines to a GitHub step output file, or
// prints them when there's none.
func writeOutputs(path string, stdout io.Writer, outputs map[string]any) error {
	var sb strings.Builder
	for _, k := range slices.Sorted(maps.Keys(outputs)) {
		data, err := json.Marshal(outputs[k])
		if err != nil {
			return fmt.Errorf("encode %s: %w", k, err)
		}
		fmt.Fprintf(&sb, "%s=%s\n", k, data)
	}
	if path == "" {
		say(stdout, "%s", sb.String())
		return nil
	}
	fh, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // the runner's own output file
	if err != nil {
		return fmt.Errorf("write step outputs: %w", err)
	}
	_, werr := fh.WriteString(sb.String())
	if cerr := fh.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return fmt.Errorf("write step outputs: %w", werr)
	}
	return nil
}

// runCleanup closes the replay prepare opened.
func runCleanup(ctx context.Context, d deps, args []string, getenv func(string) string, stderr io.Writer) error {
	f := newFlags("cleanup", getenv, stderr).apiURLFlag()
	replayJSON := f.fs.String("replay", "", "the replay, as prepare wrote it (required)")
	if err := f.fs.Parse(args); err != nil {
		return usageError{err}
	}
	if f.fs.NArg() > 0 {
		return usagef("unexpected arguments: %s", strings.Join(f.fs.Args(), " "))
	}
	replay, err := parseReplay(*replayJSON)
	if err != nil {
		return err
	}
	token := getenv("GITHUB_TOKEN")
	if token == "" {
		return usagef("GITHUB_TOKEN is not set")
	}
	if err := d.replayer(d.github(token, f.apiURL)).Close(ctx, replay); err != nil {
		return fmt.Errorf("close the replay: %w", err)
	}
	say(stderr, "buggrit-bench: closed %s#%d and deleted %s and %s\n", replay.Repo, replay.PR, replay.Head, replay.Base)
	return nil
}

func parseReplay(s string) (*bench.Replay, error) {
	if s == "" {
		return nil, usagef("--replay is required")
	}
	var r bench.Replay
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		return nil, usagef("--replay: %v", err)
	}
	if _, err := github.ParseRepo(r.Repo); err != nil || r.PR <= 0 || !strings.HasPrefix(r.Head, bench.BranchPrefix) || !strings.HasPrefix(r.Base, bench.BranchPrefix) {
		return nil, usagef("--replay: not a replay prepare wrote: %s", s)
	}
	return &r, nil
}
