package github_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/buggrit/internal/github"
	"github.com/SnapdragonPartners/buggrit/internal/github/githubtest"
)

const (
	head = "1111111111111111111111111111111111111111"
	base = "0000000000000000000000000000000000000000"
)

var repo = github.Repo{Owner: "acme", Name: "widgets"}

func fixtureDiff(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "git.diff"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func setup(t *testing.T) (*githubtest.Server, *github.Client) {
	t.Helper()
	fake := githubtest.New(t, "acme/widgets")
	fake.AddPR(7, head, base, fixtureDiff(t))
	return fake, fake.Client(githubtest.DefaultToken)
}

func TestPullRequestAndDiff(t *testing.T) {
	_, c := setup(t)
	ctx := context.Background()
	pr, err := c.PullRequest(ctx, repo, 7)
	if err != nil {
		t.Fatal(err)
	}
	if pr.HeadSHA != head || pr.BaseSHA != base || pr.Number != 7 || pr.BaseRef != "main" {
		t.Errorf("pr = %+v", pr)
	}
	d, err := c.Diff(ctx, repo, 7)
	if err != nil {
		t.Fatal(err)
	}
	if d.File("modified.go") == nil || len(d.Files) != 10 {
		t.Errorf("diff has %d files", len(d.Files))
	}
	if _, err := c.PullRequest(ctx, repo, 8); !github.IsNotFound(err) {
		t.Errorf("missing PR error = %v, want not found", err)
	}
}

func TestBadToken(t *testing.T) {
	fake, _ := setup(t)
	_, err := fake.Client("wrong").PullRequest(context.Background(), repo, 7)
	var apiErr *github.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized {
		t.Errorf("error = %v, want 401", err)
	}
}

// postFindings posts a review with a line comment, a range comment and a
// file-level comment, the three anchor kinds a pass produces.
func postFindings(t *testing.T, c *github.Client) *github.PostedReview {
	t.Helper()
	d, err := c.Diff(context.Background(), repo, 7)
	if err != nil {
		t.Fatal(err)
	}
	anchor := func(path string, start, end int) github.Anchor {
		a, err := d.AnchorFor(path, start, end)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	posted, err := c.CreateReview(context.Background(), repo, 7, &github.Review{
		CommitID: head,
		Body:     "buggrit pass 1",
		Comments: []github.ReviewComment{
			{Body: "line finding", Anchor: anchor("modified.go", 0, 2)},
			{Body: "range finding", Anchor: anchor("modified.go", 1, 4)},
			{Body: "file finding", Anchor: anchor("blob.bin", 0, 1)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return posted
}

func TestCreateReviewAndReadThreads(t *testing.T) {
	fake, c := setup(t)
	posted := postFindings(t, c)
	if len(posted.FileComments) != 1 || posted.FileComments[0].DatabaseID == 0 {
		t.Fatalf("file comments = %+v", posted.FileComments)
	}
	if got := fake.PR(7).Reviews; got != 1 {
		t.Errorf("reviews = %d, want 1 (line comments go in one review)", got)
	}

	threads, err := c.ReviewThreads(context.Background(), repo, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 3 {
		t.Fatalf("got %d threads, want 3", len(threads))
	}
	byBody := map[string]github.Thread{}
	for _, th := range threads {
		byBody[th.Root().Body] = th
	}
	line := byBody["line finding"]
	if line.Path != "modified.go" || line.Line != 2 || line.Side != github.SideRight || line.FileLevel {
		t.Errorf("line thread = %+v", line)
	}
	if r := byBody["range finding"]; r.StartLine != 1 || r.Line != 4 {
		t.Errorf("range thread = %+v", r)
	}
	if f := byBody["file finding"]; !f.FileLevel || f.Path != "blob.bin" || f.Line != 0 {
		t.Errorf("file thread = %+v", f)
	}
	// GraphQL reports bots without "[bot]"; the client normalizes it.
	if got := line.Root().Author; got != githubtest.DefaultLogin {
		t.Errorf("author = %q, want %q", got, githubtest.DefaultLogin)
	}
	if line.Root().CommitSHA != head || line.Root().DatabaseID == 0 || line.Root().ID == "" {
		t.Errorf("root comment = %+v", line.Root())
	}
}

func TestCreateReviewRejectsUnresolvableLine(t *testing.T) {
	fake, c := setup(t)
	_, err := c.CreateReview(context.Background(), repo, 7, &github.Review{
		CommitID: head,
		Comments: []github.ReviewComment{{Body: "x", Anchor: github.Anchor{Path: "modified.go", Line: 15, Side: github.SideRight}}},
	})
	var apiErr *github.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnprocessableEntity {
		t.Fatalf("error = %v, want 422", err)
	}
	if n := len(fake.PR(7).Threads); n != 0 {
		t.Errorf("%d threads created by a rejected review", n)
	}
}

func TestCreateReviewOnlyFileComments(t *testing.T) {
	fake, c := setup(t)
	posted, err := c.CreateReview(context.Background(), repo, 7, &github.Review{
		CommitID: head,
		Comments: []github.ReviewComment{{Body: "file finding", Anchor: github.Anchor{Path: "blob.bin"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if posted.ID != 0 || len(posted.FileComments) != 1 || fake.PR(7).Reviews != 0 {
		t.Errorf("posted = %+v, reviews = %d", posted, fake.PR(7).Reviews)
	}
}

func TestReplyToReplyRejected(t *testing.T) {
	_, c := setup(t)
	ctx := context.Background()
	postFindings(t, c)
	threads, err := c.ReviewThreads(ctx, repo, 7)
	if err != nil {
		t.Fatal(err)
	}
	reply, err := c.Reply(ctx, repo, 7, threads[0].Root().DatabaseID, "first")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Reply(ctx, repo, 7, reply.DatabaseID, "second"); err == nil {
		t.Error("reply to a reply was accepted")
	}
}

func TestCreateReviewNeedsCommit(t *testing.T) {
	_, c := setup(t)
	if _, err := c.CreateReview(context.Background(), repo, 7, &github.Review{}); err == nil {
		t.Fatal("review without commit ID was accepted")
	}
}

func TestReplyResolveUnresolve(t *testing.T) {
	fake, c := setup(t)
	ctx := context.Background()
	postFindings(t, c)
	threads, err := c.ReviewThreads(ctx, repo, 7)
	if err != nil {
		t.Fatal(err)
	}
	th := threads[0]

	// A human reply, then buggrit's reply to the thread.
	fake.Reply(7, th.ID, "dratner", "fixed in abc")
	if _, err := c.Reply(ctx, repo, 7, th.Root().DatabaseID, "verified"); err != nil {
		t.Fatal(err)
	}
	if err := c.ResolveThread(ctx, th.ID); err != nil {
		t.Fatal(err)
	}
	got := readThread(t, c, th.ID)
	if !got.IsResolved || len(got.Comments) != 3 {
		t.Fatalf("after resolve: resolved=%v comments=%d", got.IsResolved, len(got.Comments))
	}
	if a := got.Comments[1].Author; a != "dratner" {
		t.Errorf("human reply author = %q", a)
	}
	if err := c.UnresolveThread(ctx, th.ID); err != nil {
		t.Fatal(err)
	}
	if readThread(t, c, th.ID).IsResolved {
		t.Error("thread still resolved after unresolve")
	}
}

func readThread(t *testing.T, c *github.Client, id string) github.Thread {
	t.Helper()
	threads, err := c.ReviewThreads(context.Background(), repo, 7)
	if err != nil {
		t.Fatal(err)
	}
	for _, th := range threads {
		if th.ID == id {
			return th
		}
	}
	t.Fatalf("thread %s not found", id)
	return github.Thread{}
}

func TestResolveForbidden(t *testing.T) {
	fake, c := setup(t)
	postFindings(t, c)
	threads, err := c.ReviewThreads(context.Background(), repo, 7)
	if err != nil {
		t.Fatal(err)
	}
	fake.ForbidResolve = true
	err = c.ResolveThread(context.Background(), threads[0].ID)
	if !github.IsForbidden(err) {
		t.Errorf("error = %v, want IsForbidden", err)
	}
	if err := c.UnresolveThread(context.Background(), threads[0].ID); !github.IsForbidden(err) {
		t.Errorf("unresolve error = %v, want IsForbidden", err)
	}
	fake.ForbidResolve = false
	if err := c.ResolveThread(context.Background(), "PRRT_missing"); err == nil || github.IsForbidden(err) {
		t.Errorf("forbidden reported for every error: %v", err)
	}
}

func TestReviewThreadsPaginates(t *testing.T) {
	fake, c := setup(t)
	fake.ThreadsPageSize, fake.CommentsPageSize = 2, 2
	var ids []string
	for i := range 5 {
		ids = append(ids, fake.AddThread(7, "copilot-pull-request-reviewer[bot]", "modified.go", 2, fmt.Sprintf("thread %d", i)))
	}
	for i := range 4 {
		fake.Reply(7, ids[3], "dratner", fmt.Sprintf("reply %d", i))
	}
	threads, err := c.ReviewThreads(context.Background(), repo, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 5 {
		t.Fatalf("got %d threads, want 5", len(threads))
	}
	long := threads[3]
	if len(long.Comments) != 5 || long.Comments[4].Body != "reply 3" {
		t.Errorf("long thread has %d comments", len(long.Comments))
	}
	if a := threads[0].Root().Author; a != "copilot-pull-request-reviewer[bot]" {
		t.Errorf("other reviewer's author = %q", a)
	}
}

func TestIssueComments(t *testing.T) {
	fake, c := setup(t)
	ctx := context.Background()
	fake.IssuePageSize = 2
	var last *github.Comment
	for i := range 5 {
		cm, err := c.CreateIssueComment(ctx, repo, 7, fmt.Sprintf("comment %d", i))
		if err != nil {
			t.Fatal(err)
		}
		last = cm
	}
	all, err := c.IssueComments(ctx, repo, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 5 || all[4].Body != "comment 4" || all[0].Author != githubtest.DefaultLogin {
		t.Fatalf("listed %d comments: %+v", len(all), all)
	}
	updated, err := c.UpdateIssueComment(ctx, repo, last.DatabaseID, "summary v2")
	if err != nil || updated.Body != "summary v2" {
		t.Fatalf("update = %+v, %v", updated, err)
	}

	fake.AddToken("human", "dratner")
	if _, err := fake.Client("human").UpdateIssueComment(ctx, repo, last.DatabaseID, "x"); !github.IsForbidden(err) {
		t.Errorf("editing another's comment: %v, want forbidden", err)
	}
}

func TestDeleteReviewComment(t *testing.T) {
	fake, c := setup(t)
	posted := postFindings(t, c)
	if err := c.DeleteReviewComment(context.Background(), repo, posted.FileComments[0].DatabaseID); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.PR(7).Threads); n != 2 {
		t.Errorf("%d threads after deleting the file comment, want 2", n)
	}
}

func TestFence(t *testing.T) {
	fake, c := setup(t)
	ctx := context.Background()
	f := github.NewFence(c, repo, 7, head)
	if err := f.Check(ctx); err != nil {
		t.Fatalf("fence on unchanged head: %v", err)
	}
	fake.Push(7, "2222222222222222222222222222222222222222", fixtureDiff(t))
	err := f.Check(ctx)
	if !errors.Is(err, github.ErrHeadMoved) {
		t.Fatalf("error = %v, want ErrHeadMoved", err)
	}
	var moved *github.HeadMovedError
	if !errors.As(err, &moved) || moved.Reviewed != head || !strings.HasPrefix(moved.Current, "2222") {
		t.Errorf("moved = %+v", moved)
	}
}

// The head moves between the review and the resolve that follows it: the
// fence before the mutation catches it (design §2, §11).
func TestFenceStopsMutationAfterHeadMoves(t *testing.T) {
	fake, c := setup(t)
	ctx := context.Background()
	f := github.NewFence(c, repo, 7, head)
	fake.OnRequest = func(op string) int {
		if op == "POST review" {
			fake.Push(7, "3333333333333333333333333333333333333333", fixtureDiff(t))
		}
		return 0
	}
	postFindings(t, c)
	if err := f.Check(ctx); !errors.Is(err, github.ErrHeadMoved) {
		t.Fatalf("fence before resolve: %v, want ErrHeadMoved", err)
	}
}

func TestFakeReportsEachGraphQLOperation(t *testing.T) {
	fake, c := setup(t)
	postFindings(t, c)
	var ops []string
	fake.OnRequest = func(op string) int {
		if strings.HasPrefix(op, "graphql ") {
			ops = append(ops, op)
		}
		return 0
	}
	threads, err := c.ReviewThreads(context.Background(), repo, 7)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ResolveThread(context.Background(), threads[0].ID); err != nil {
		t.Fatal(err)
	}
	if want := []string{"graphql ReviewThreads", "graphql ResolveThread"}; !slices.Equal(ops, want) {
		t.Errorf("ops = %v, want %v", ops, want)
	}
}

func TestInjectedFailureSurfaces(t *testing.T) {
	fake, _ := setup(t)
	c := github.NewClient(githubtest.DefaultToken, github.WithBaseURL(fake.URL()), github.WithRetries(1))
	fake.OnRequest = func(op string) int {
		if op == "graphql ReviewThreads" {
			return http.StatusBadGateway
		}
		return 0
	}
	if _, err := c.ReviewThreads(context.Background(), repo, 7); err == nil {
		t.Fatal("ReviewThreads succeeded through a 502")
	}
}
