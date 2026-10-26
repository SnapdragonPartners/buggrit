package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/buggrit/internal/config"
	"github.com/SnapdragonPartners/buggrit/internal/github"
	"github.com/SnapdragonPartners/buggrit/internal/github/githubtest"
	"github.com/SnapdragonPartners/buggrit/internal/state"
	"github.com/SnapdragonPartners/buggrit/internal/store"
)

type fakeStore struct{ closed bool }

func (f *fakeStore) Close() error { f.closed = true; return nil }

// dbEnv drives openDatabase against the fake GitHub's PR 7.
type dbEnv struct {
	fake    *githubtest.Server
	store   *fakeStore
	openErr error
	opened  []string
	stderr  bytes.Buffer
	cfg     *config.Config
}

func newDBEnv(t *testing.T) *dbEnv {
	t.Helper()
	fake := githubtest.New(t, "acme/widgets")
	fake.AddPR(7, head, base, diff)
	cfg, err := config.Parse([]byte(cfgJSON))
	if err != nil {
		t.Fatal(err)
	}
	return &dbEnv{fake: fake, cfg: cfg}
}

// open runs openDatabase with the given environment and returns what it
// opened.
func (e *dbEnv) open(t *testing.T, args reviewArgs) io.Closer {
	t.Helper()
	e.stderr.Reset()
	repo, _ := github.ParseRepo("acme/widgets")
	r := &reviewer{
		args: &args, repo: repo, stderr: &e.stderr, stdout: io.Discard,
		gh: github.NewClient(githubtest.DefaultToken, github.WithBaseURL(e.fake.URL())),
		deps: reviewDeps{openStore: func(_ context.Context, url string) (io.Closer, error) {
			e.opened = append(e.opened, url)
			if e.openErr != nil {
				return nil, e.openErr
			}
			e.store = &fakeStore{}
			return e.store, nil
		}},
	}
	db := r.openDatabase(context.Background(), e.cfg, 7)
	if (r.dbProblem != "") != strings.Contains(e.stderr.String(), "can't be used") {
		t.Errorf("dbProblem %q but stderr %q", r.dbProblem, e.stderr.String())
	}
	return db
}

// warnings returns the bodies of the PR's database warnings by anyone.
func (e *dbEnv) warnings() []string {
	var out []string
	for _, c := range e.fake.PR(7).IssueComments {
		if strings.Contains(c.Body, "couldn't use its database") {
			out = append(out, c.Body)
		}
	}
	return out
}

func TestDatabaseNotConfigured(t *testing.T) {
	e := newDBEnv(t)
	if db := e.open(t, reviewArgs{}); db != nil || len(e.opened) != 0 || len(e.warnings()) != 0 {
		t.Fatalf("db = %v, opened %v, warnings %v", db, e.opened, e.warnings())
	}
}

func TestDatabaseWarningLifecycle(t *testing.T) {
	e := newDBEnv(t)
	var ops []string
	e.fake.OnRequest = func(op string) int {
		ops = append(ops, op)
		return 0
	}
	args := reviewArgs{databaseURL: "postgres://runner@127.0.0.1/buggrit"}

	// Unusable: the run goes on without it, with one warning carrying the
	// message as is.
	e.openErr = errors.New("failed to connect: ```dial``` refused")
	if db := e.open(t, args); db != nil {
		t.Fatalf("db = %v", db)
	}
	w := e.warnings()
	if len(w) != 1 || !strings.Contains(w[0], "````\nfailed to connect: ```dial``` refused\n````") {
		t.Fatalf("warnings = %q", w)
	}
	if m, err := state.Parse(w[0]); err != nil || m.ID != databaseWarningID {
		t.Fatalf("marker = %+v, %v", m, err)
	}

	// The same problem again changes nothing.
	ops = nil
	e.open(t, args)
	if strings.Join(ops, ",") != "GET issue comments" {
		t.Errorf("ops = %v", ops)
	}

	// A different problem edits the comment in place.
	e.openErr = errors.New("the buggrit schema must be v2 or newer, found v1; run `buggrit db migrate`")
	ops = nil
	e.open(t, args)
	if w := e.warnings(); len(w) != 1 || !strings.Contains(w[0], "found v1") || !strings.Contains(strings.Join(ops, ","), "PATCH issue comment") {
		t.Fatalf("warnings = %q, ops = %v", w, ops)
	}

	// Working again: the warning goes, and the store is returned open.
	e.openErr = nil
	db := e.open(t, args)
	if db != e.store || len(e.warnings()) != 0 {
		t.Fatalf("db = %v, warnings = %q", db, e.warnings())
	}
	if e.opened[len(e.opened)-1] != args.databaseURL {
		t.Errorf("opened %v", e.opened)
	}
}

// The workflow reports a setup failure, so buggrit doesn't try to connect.
func TestDatabaseSetupError(t *testing.T) {
	e := newDBEnv(t)
	db := e.open(t, reviewArgs{databaseError: "no OIDC token", databaseURL: "postgres://x"})
	if db != nil || len(e.opened) != 0 {
		t.Fatalf("db = %v, opened = %v", db, e.opened)
	}
	if w := e.warnings(); len(w) != 1 || !strings.Contains(w[0], "no OIDC token") {
		t.Fatalf("warnings = %q", w)
	}
}

// Only buggrit's own warnings count: a copy by someone else stays, and an
// extra one of buggrit's (from a race) goes.
func TestDatabaseWarningOwnOnly(t *testing.T) {
	e := newDBEnv(t)
	body, err := databaseWarning("old problem")
	if err != nil {
		t.Fatal(err)
	}
	pr := e.fake.PR(7)
	pr.IssueComments = append(pr.IssueComments,
		&githubtest.Comment{DatabaseID: 9001, Author: "mallory", Body: body},
		&githubtest.Comment{DatabaseID: 9002, Author: githubtest.DefaultLogin, Body: body},
		&githubtest.Comment{DatabaseID: 9003, Author: githubtest.DefaultLogin, Body: body})

	e.open(t, reviewArgs{databaseError: "new problem"})
	var ids []int64
	for _, c := range pr.IssueComments {
		ids = append(ids, c.DatabaseID)
		if c.DatabaseID == 9002 && !strings.Contains(c.Body, "new problem") {
			t.Errorf("not edited in place: %q", c.Body)
		}
	}
	if len(ids) != 2 || ids[0] != 9001 || ids[1] != 9002 {
		t.Fatalf("comments = %v", ids)
	}

	// No longer configured: buggrit's warning goes, the copy stays.
	e.open(t, reviewArgs{})
	if len(pr.IssueComments) != 1 || pr.IssueComments[0].DatabaseID != 9001 {
		t.Fatalf("comments = %+v", pr.IssueComments)
	}
}

func TestDatabaseWarningDryRun(t *testing.T) {
	e := newDBEnv(t)
	e.fake.OnRequest = func(op string) int {
		t.Errorf("dry run called GitHub: %s", op)
		return 0
	}
	e.open(t, reviewArgs{databaseError: "no OIDC token", dryRun: true})
	if !strings.Contains(e.stderr.String(), "no OIDC token") {
		t.Errorf("stderr = %q", e.stderr.String())
	}
}

// Failing to post the warning doesn't fail the run.
func TestDatabaseWarningPostFails(t *testing.T) {
	e := newDBEnv(t)
	e.fake.OnRequest = func(op string) int {
		if op == "POST issue comment" {
			return 500
		}
		return 0
	}
	e.open(t, reviewArgs{databaseError: "no OIDC token"})
	if !strings.Contains(e.stderr.String(), "can't update the database warning") {
		t.Errorf("stderr = %q", e.stderr.String())
	}
}

// A whole run: the database problem doesn't change the review or its exit
// code, and the job summary says what went wrong.
func TestReviewWithoutDatabase(t *testing.T) {
	e := setupReview(t, submit(finding))
	e.env["BUGGRIT_DATABASE_URL"] = "postgres://runner@127.0.0.1/buggrit"
	e.deps.openStore = func(context.Context, string) (io.Closer, error) {
		return nil, errors.New("connection refused")
	}
	code, out, errOut := e.run(t)
	if code != 0 || !strings.Contains(out, "1 new finding") {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	summary, err := os.ReadFile(e.summary)
	if err != nil || !strings.Contains(string(summary), "### Database") || !strings.Contains(string(summary), "connection refused") {
		t.Errorf("job summary = %q, %v", summary, err)
	}
	if len(e.fake.PR(7).IssueComments) != 1 || len(e.fake.PR(7).Threads) != 1 {
		t.Errorf("issue comments = %d, threads = %d", len(e.fake.PR(7).IssueComments), len(e.fake.PR(7).Threads))
	}
}

func TestCodeBlock(t *testing.T) {
	if got := codeBlock("a\n"); got != "```\na\n```\n" {
		t.Errorf("got %q", got)
	}
}

func TestRedactURL(t *testing.T) {
	for _, tc := range []struct{ msg, url, want string }{
		{"dial: refused", "postgres://u:pw@h/db", "dial: refused"},
		{`parse "postgres://u:s3cret@h:x/db": invalid port`, "postgres://u:s3cret@h:x/db", `parse "$BUGGRIT_DATABASE_URL": invalid port`},
		{"cannot parse `postgres://u:a@b@h/db`: failed (a@b)", "postgres://u:a@b@h/db", "cannot parse `$BUGGRIT_DATABASE_URL`: failed (xxxxx)"},
		{"no password: postgres://u@h/db?options=x@y", "postgres://u@h/db?options=x@y", "no password: $BUGGRIT_DATABASE_URL"},
		{"not a URL: host=h", "host=h password=pw", "not a URL: host=h"},
	} {
		if got := redactURL(tc.msg, tc.url); got != tc.want {
			t.Errorf("redactURL(%q, %q) = %q, want %q", tc.msg, tc.url, got, tc.want)
		}
	}
}

// The warning for a URL pgx can't parse doesn't carry its password.
func TestDatabaseWarningHidesPassword(t *testing.T) {
	e := newDBEnv(t)
	const url = "postgres://runner:s3cret@127.0.0.1:notaport/buggrit"
	repo, _ := github.ParseRepo("acme/widgets")
	r := &reviewer{
		args: &reviewArgs{databaseURL: url}, repo: repo, stderr: &e.stderr, stdout: io.Discard,
		gh:   github.NewClient(githubtest.DefaultToken, github.WithBaseURL(e.fake.URL())),
		deps: reviewDeps{openStore: func(ctx context.Context, url string) (io.Closer, error) { return store.Open(ctx, url) }},
	}
	if db := r.openDatabase(context.Background(), e.cfg, 7); db != nil {
		t.Fatal("opened a malformed URL")
	}
	out := r.dbProblem + e.stderr.String() + strings.Join(e.warnings(), "")
	if r.dbProblem == "" || len(e.warnings()) != 1 || strings.Contains(out, "s3cret") {
		t.Errorf("dbProblem %q, warnings %q", r.dbProblem, e.warnings())
	}
}
