package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/SnapdragonPartners/buggrit/internal/bench"
	"github.com/SnapdragonPartners/buggrit/internal/github"
)

// runReport reads an experiment's runs from telemetry, has the judge score
// any not yet scored, and writes the report. In the workflow it gets the
// experiment and the replay and records them; given only --workflow-run, it
// reads them back, so a report can be regenerated after overrides.
func runReport(ctx context.Context, d deps, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	f := newFlags("report", getenv, stderr)
	path := f.fs.String("experiment", "", "the experiment file, in the workflow; omit to regenerate from the database")
	replayJSON := f.fs.String("replay", "", "the replay, as prepare wrote it (with --experiment)")
	f.fs.StringVar(&f.root, "root", ".", "buggrit's checkout, which the experiment's paths are relative to")
	out := f.fs.String("out", "", "write the report to this file instead of stdout")
	comment := f.fs.Bool("comment", false, "post the report on the experiment's issue, if it names one")
	issueRepo := f.fs.String("issue-repo", getenv("GITHUB_REPOSITORY"), "the repository of the experiment's issue (default $GITHUB_REPOSITORY)")
	noJudge := f.fs.Bool("no-judge", false, "don't judge runs that have no scores yet")
	summary := getenv("GITHUB_STEP_SUMMARY")
	if err := f.fs.Parse(args); err != nil {
		return usageError{err}
	}
	switch {
	case f.fs.NArg() > 0:
		return usagef("unexpected arguments: %s", strings.Join(f.fs.Args(), " "))
	case f.workflowRun <= 0:
		return usagef("no workflow run: set --workflow-run or GITHUB_RUN_ID")
	case (*path == "") != (*replayJSON == ""):
		return usagef("--experiment and --replay go together; give neither to regenerate a report")
	}
	if e := getenv("BUGGRIT_DATABASE_ERROR"); e != "" {
		return fmt.Errorf("the database couldn't be set up: %s", e)
	}
	url := getenv("BUGGRIT_DATABASE_URL")
	if url == "" {
		return usagef("BUGGRIT_DATABASE_URL is not set; the results are in the database")
	}
	db, err := d.openStore(ctx, url)
	if err != nil {
		return fmt.Errorf("open the database: %w", err)
	}
	defer func() { _ = db.Close() }()

	w := bench.WorkflowRun{ID: f.workflowRun, Attempt: f.attempt}
	rec, err := experimentRecord(ctx, d, db, w, f.root, *path, *replayJSON)
	if err != nil {
		return err
	}
	runs, scores, judgeErr, err := results(ctx, d, db, rec, w, !*noJudge, getenv, stderr)
	if err != nil {
		return err
	}
	report := bench.Report(&bench.ReportInput{Experiment: rec, Runs: runs, Scores: scores, RunURL: runURL(getenv, w)})
	if err := emit(report, *out, summary, stdout, stderr); err != nil {
		return err
	}
	if *comment && rec.Experiment.Issue > 0 {
		if err := postComment(ctx, d, getenv, f.apiURL, *issueRepo, rec.Experiment.Issue, report); err != nil {
			return errors.Join(judgeErr, err) //nolint:wrapcheck // each says what failed
		}
	}
	return judgeErr
}

// results reads the experiment's runs and their scores, having the judge
// score the runs not yet scored when judge is set. A run the judge fails on
// is in judgeErr, and the report still goes out.
func results(ctx context.Context, d deps, db benchDB, rec *bench.ExperimentRecord, w bench.WorkflowRun, judge bool, getenv func(string) string, stderr io.Writer) (runs []bench.RunResult, scores map[string]*bench.RunScore, judgeErr, err error) {
	if runs, err = db.Runs(ctx, w, rec.Replay); err != nil {
		return nil, nil, nil, err //nolint:wrapcheck // says what it was reading
	}
	bench.AssignArms(rec.Experiment, runs)
	say(stderr, "buggrit-bench: %d run(s) of experiment %s in workflow run %d attempt %d\n", len(runs), rec.Experiment.Name, w.ID, w.Attempt)
	if rec.Benchmark == nil {
		return runs, nil, nil, nil
	}
	if scores, err = db.Scores(ctx, rec.ID); err != nil {
		return nil, nil, nil, err //nolint:wrapcheck // says what it was reading
	}
	if !judge {
		return runs, scores, nil, nil
	}
	judgeErr = judgeRuns(ctx, d, db, rec, runs, scores, getenv, stderr)
	if scores, err = db.Scores(ctx, rec.ID); err != nil {
		return nil, nil, nil, err //nolint:wrapcheck // says what it was reading
	}
	return runs, scores, judgeErr, nil //nolint:wrapcheck // judgeRuns says which runs failed
}

// emit writes the report to out, or stdout, and to the job summary.
func emit(report, out, summary string, stdout, stderr io.Writer) error {
	if out != "" {
		if err := os.WriteFile(out, []byte(report), 0o644); err != nil { //nolint:gosec // a report, not a secret
			return fmt.Errorf("write the report: %w", err)
		}
	} else {
		say(stdout, "%s", report)
	}
	if summary != "" {
		if err := appendFile(summary, report); err != nil {
			say(stderr, "buggrit-bench: write the job summary: %v\n", err)
		}
	}
	return nil
}

// experimentRecord records the experiment the workflow ran, or reads back
// the one recorded for w.
func experimentRecord(ctx context.Context, d deps, db benchDB, w bench.WorkflowRun, root, path, replayJSON string) (*bench.ExperimentRecord, error) {
	if path == "" {
		rec, err := db.LoadExperiment(ctx, w)
		if err != nil {
			return nil, err //nolint:wrapcheck // names the workflow run
		}
		return rec, nil
	}
	replay, err := parseReplay(replayJSON)
	if err != nil {
		return nil, err
	}
	l, err := load(root, path)
	if err != nil {
		return nil, err
	}
	rec := &bench.ExperimentRecord{
		ID: d.newID(), Experiment: l.exp, Spec: l.spec, Workflow: w, Replay: replay,
		Benchmark: l.bm, BenchmarkHash: l.hash,
	}
	if l.bm != nil {
		if serr := db.SaveBenchmark(ctx, l.hash, l.bm); serr != nil {
			return nil, serr //nolint:wrapcheck // names the benchmark
		}
	}
	if rec.ID, err = db.SaveExperiment(ctx, rec); err != nil {
		return nil, err //nolint:wrapcheck // names the experiment
	}
	return rec, nil
}

// judgeRuns scores each run that has a result and no scores yet. A run
// the judge fails on is reported and left unscored, so a later report can
// try again; the others are still scored.
func judgeRuns(ctx context.Context, d deps, db benchDB, rec *bench.ExperimentRecord, runs []bench.RunResult, scores map[string]*bench.RunScore, getenv func(string) string, stderr io.Writer) error {
	var todo []*bench.RunResult
	for i := range runs {
		if runs[i].HasResult && runs[i].Arm != "" && scores[runs[i].ID] == nil {
			todo = append(todo, &runs[i])
		}
	}
	if len(todo) == 0 {
		return nil
	}
	chat, err := d.judge(rec.Experiment.Judge, getenv)
	if err != nil {
		return err
	}
	j := &bench.Judge{Chat: chat}
	var errs []error
	for _, r := range todo {
		s, err := j.Score(ctx, rec.Benchmark, r)
		if err == nil {
			err = db.SaveScore(ctx, rec.ID, s, r.Findings)
		}
		if err != nil {
			say(stderr, "buggrit-bench: %v\n", err)
			errs = append(errs, err)
			continue
		}
		say(stderr, "buggrit-bench: scored run %s (%s): recall %.2f\n", r.ID, r.Label, s.Recall())
	}
	if len(errs) > 0 {
		return fmt.Errorf("%d run(s) left unscored: %w", len(errs), errors.Join(errs...))
	}
	return nil
}

func runURL(getenv func(string) string, w bench.WorkflowRun) string {
	server, repo := getenv("GITHUB_SERVER_URL"), getenv("GITHUB_REPOSITORY")
	if server == "" || repo == "" {
		return ""
	}
	return fmt.Sprintf("%s/%s/actions/runs/%d/attempts/%d", server, repo, w.ID, w.Attempt)
}

// maxComment is under GitHub's 65,536-character limit on a comment.
const maxComment = 65000

func postComment(ctx context.Context, d deps, getenv func(string) string, apiURL, repoName string, issue int, report string) error {
	token := getenv("GITHUB_TOKEN")
	if token == "" {
		return usagef("GITHUB_TOKEN is not set, so the report can't be posted")
	}
	repo, err := github.ParseRepo(repoName)
	if err != nil {
		return usagef("--issue-repo: %v", err)
	}
	if len(report) > maxComment {
		cut := strings.LastIndex(report[:maxComment], "\n<details>")
		if cut < 0 {
			cut = maxComment
		}
		report = report[:cut] + "\n\n_The report was cut to fit a comment; the job summary has all of it._\n"
	}
	if _, err := d.github(token, apiURL).CreateIssueComment(ctx, repo, issue, report); err != nil {
		return fmt.Errorf("post the report: %w", err)
	}
	return nil
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
