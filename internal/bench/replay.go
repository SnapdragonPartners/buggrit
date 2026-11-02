package bench

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/SnapdragonPartners/buggrit/internal/github"
)

// Replay is an open replay pull request: a fresh PR of the target, with no
// threads, so each run is a full first pass.
type Replay struct {
	Repo string `json:"repo"`
	// Base and Head are the replay's branches.
	Base string `json:"base"`
	Head string `json:"head"`
	PR   int    `json:"pr"`
}

// Replayer opens and closes replays. GitHubReplayer is the one there is;
// the interface keeps the harness from depending on where a target lives,
// so another host (a Gitea container, say) can be added without changing
// the rest.
type Replayer interface {
	// Open makes a replay of t, naming its branches with tag, which must
	// be unique to this experiment run.
	Open(ctx context.Context, t *Target, tag string) (*Replay, error)
	// Close closes the replay and deletes its branches. A replay already
	// partly or wholly closed is fine, so Close can be retried.
	Close(ctx context.Context, r *Replay) error
}

// GitHubReplayer replays a target as a draft PR in its own repository,
// between two branches under bench/<tag>/. buggrit's own workflow skips
// drafts, and CI that runs only on PRs to main skips it too.
type GitHubReplayer struct {
	GH *github.Client
}

// BranchPrefix is where replay branches live.
const BranchPrefix = "bench/"

// Open creates the branches and the draft PR. If a step fails it closes
// what it made, and only that: a branch that already existed (a rerun
// prepare's, say) is someone else's to clean up.
func (g *GitHubReplayer) Open(ctx context.Context, t *Target, tag string) (r *Replay, err error) {
	repo, err := github.ParseRepo(t.Repo)
	if err != nil {
		return nil, err //nolint:wrapcheck // names the repo
	}
	r = &Replay{Repo: t.Repo, Base: BranchPrefix + tag + "/base", Head: BranchPrefix + tag + "/head"}
	made := &Replay{Repo: t.Repo}
	defer func() {
		if err != nil {
			err = errors.Join(err, g.Close(context.WithoutCancel(ctx), made))
			r = nil
		}
	}()
	if berr := g.GH.CreateBranch(ctx, repo, r.Base, t.Base); berr != nil {
		return r, berr //nolint:wrapcheck // names the branch
	}
	made.Base = r.Base
	if berr := g.GH.CreateBranch(ctx, repo, r.Head, t.Head); berr != nil {
		return r, berr //nolint:wrapcheck // names the branch
	}
	made.Head = r.Head
	body := t.Body
	if body != "" {
		body += "\n\n"
	}
	body += fmt.Sprintf("<sub>A benchmark replay of %s...%s, opened by buggrit-bench (docs/benchmarking.md). It's closed and its branches deleted when the experiment ends.</sub>", short(t.Base), short(t.Head))
	n, err := g.GH.CreatePull(ctx, repo, &github.NewPull{Title: t.Title, Body: body, Head: r.Head, Base: r.Base, Draft: true})
	if err != nil {
		return r, err //nolint:wrapcheck // names the repo
	}
	r.PR, made.PR = n, n
	return r, nil
}

// Close closes the PR, if there is one, and deletes the branches it names,
// going on past a failure so as much as possible is cleaned up.
func (g *GitHubReplayer) Close(ctx context.Context, r *Replay) error {
	repo, err := github.ParseRepo(r.Repo)
	if err != nil {
		return err //nolint:wrapcheck // names the repo
	}
	var errs []error
	if r.PR > 0 {
		if err := g.GH.ClosePull(ctx, repo, r.PR); err != nil && !github.IsNotFound(err) {
			errs = append(errs, err)
		}
	}
	for _, b := range []string{r.Head, r.Base} {
		if b == "" {
			continue
		}
		if err := g.GH.DeleteBranch(ctx, repo, b); err != nil && !gone(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...) //nolint:wrapcheck // each error names what it was closing
}

// gone reports a branch that's already deleted: GitHub answers 422
// "Reference does not exist" (or 404). Any other 422, such as a refusal
// to delete a protected branch, is a failure.
func gone(err error) bool {
	var e *github.APIError
	return github.IsNotFound(err) || (errors.As(err, &e) && e.Status == http.StatusUnprocessableEntity && strings.Contains(e.Message, "Reference does not exist"))
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// Tag names an experiment run's replay branches: the experiment's name and
// the workflow run and attempt that ran it, so each is unique.
func Tag(experiment string, w WorkflowRun) string {
	return experiment + "/" + strconv.FormatInt(w.ID, 10) + "-" + strconv.Itoa(w.Attempt)
}

// WorkflowRun is the Actions run, and its attempt, that ran an experiment.
// Telemetry records both on each review run (meta.workflow_run), and the
// report reads back the runs of exactly this one.
type WorkflowRun struct {
	ID      int64 `json:"id"`
	Attempt int   `json:"attempt"`
}
