package github_test

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SnapdragonPartners/buggrit/internal/github"
	"github.com/SnapdragonPartners/buggrit/internal/review"
)

// TestLive is the build-step-2 live check (docs/design.md §3, §12): with a
// real token on a real PR, it posts a line thread and a file-level thread,
// replies, resolves and unresolves, and updates a conversation comment, then
// deletes what it created. It answers whether a given token kind (App
// installation token, Actions GITHUB_TOKEN, personal token) can do
// everything a pass needs.
//
// It is skipped unless these are set:
//
//	BUGGRIT_LIVE_REPO   owner/name
//	BUGGRIT_LIVE_PR     pull request number
//	BUGGRIT_LIVE_TOKEN  the token under test
//	BUGGRIT_LIVE_LOGIN  optional: the login the token should post as
//	BUGGRIT_LIVE_MEMBER optional: a teammate the token should see as one,
//	                    by association or by write permission (design §1)
//
// .github/workflows/livecheck.yml runs it with the Actions and App tokens.
func TestLive(t *testing.T) {
	repoName, prNum, token := os.Getenv("BUGGRIT_LIVE_REPO"), os.Getenv("BUGGRIT_LIVE_PR"), os.Getenv("BUGGRIT_LIVE_TOKEN")
	if repoName == "" || prNum == "" || token == "" {
		t.Skip("set BUGGRIT_LIVE_REPO, BUGGRIT_LIVE_PR and BUGGRIT_LIVE_TOKEN to run the live check")
	}
	repo, err := github.ParseRepo(repoName)
	if err != nil {
		t.Fatal(err)
	}
	number, err := strconv.Atoi(prNum)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c := github.NewClient(token)

	pr, err := c.PullRequest(ctx, repo, number)
	if err != nil {
		t.Fatal(err)
	}
	d, err := c.Diff(ctx, repo, number)
	if err != nil {
		t.Fatal(err)
	}
	// Incremental scope compares commits (design §7 rule 2). The base
	// branch's tip may be ahead of the PR's merge base, so any status will do.
	status, err := c.CompareStatus(ctx, repo, pr.BaseSHA, pr.HeadSHA)
	if err != nil {
		t.Fatalf("compare status: %v", err)
	}
	if _, err = c.CompareDiff(ctx, repo, pr.BaseSHA, pr.HeadSHA); err != nil {
		t.Fatalf("compare diff: %v", err)
	}
	t.Logf("compare base...head: %s", status)

	anchor, ok := firstAddedLine(d)
	if !ok {
		t.Fatal("the PR adds no lines to comment on")
	}
	t.Logf("PR %s#%d head %s; commenting on %s:%d", repo, number, pr.HeadSHA[:12], anchor.Path, anchor.Line)

	nonce := fmt.Sprintf("live-%d", time.Now().UnixNano())
	note := "buggrit live check (" + nonce + "); this comment deletes itself when the check finishes."
	// Registered before anything is posted, and found by nonce rather than
	// by remembered IDs, so a failure at any step still cleans up.
	t.Cleanup(func() { sweep(t, c, repo, number, nonce) })

	_, err = c.CreateReview(ctx, repo, number, &github.Review{
		CommitID: pr.HeadSHA,
		Comments: []github.ReviewComment{
			{Body: note + " [line]", Anchor: anchor},
			{Body: note + " [file]", Anchor: github.Anchor{Path: anchor.Path}},
		},
	})
	if err != nil {
		t.Fatalf("create review: %v", err)
	}

	threads, err := c.ReviewThreads(ctx, repo, number)
	if err != nil {
		t.Fatalf("read threads: %v", err)
	}
	var line, file *github.Thread
	for i := range threads {
		root := threads[i].Root()
		switch {
		case root == nil || !strings.Contains(root.Body, nonce):
		case strings.HasSuffix(root.Body, "[line]"):
			line = &threads[i]
		case strings.HasSuffix(root.Body, "[file]"):
			file = &threads[i]
		}
	}
	if line == nil || file == nil {
		t.Fatalf("posted threads not found (line %v, file %v)", line != nil, file != nil)
	}
	lineRootID := line.Root().DatabaseID
	if !file.FileLevel || line.FileLevel || line.Line != anchor.Line {
		t.Errorf("thread shapes: line %+v, file level %v", line, file.FileLevel)
	}
	author := line.Root().Author
	t.Logf("posting as %s", author)
	if want := os.Getenv("BUGGRIT_LIVE_LOGIN"); want != "" && !strings.EqualFold(author, want) {
		t.Errorf("posted as %q, want %q", author, want)
	}
	if member := os.Getenv("BUGGRIT_LIVE_MEMBER"); member != "" {
		checkMember(ctx, t, c, repo, threads, member)
	}

	if _, err := c.Reply(ctx, repo, number, lineRootID, note+" [reply]"); err != nil {
		t.Fatalf("reply: %v", err)
	}

	checkResolve := func(resolve bool) {
		t.Helper()
		op, call := "unresolve", c.UnresolveThread
		if resolve {
			op, call = "resolve", c.ResolveThread
		}
		if err := call(ctx, line.ID); err != nil {
			if github.IsForbidden(err) {
				t.Errorf("%s: refused for this token; buggrit would list the thread as \"resolve manually\" (design §3): %v", op, err)
			} else {
				t.Errorf("%s: %v", op, err)
			}
			return
		}
		got, err := c.ReviewThreads(ctx, repo, number)
		if err != nil {
			t.Fatalf("reread threads: %v", err)
		}
		for _, th := range got {
			if th.ID == line.ID && th.IsResolved != resolve {
				t.Errorf("%s: thread isResolved = %v afterwards", op, th.IsResolved)
			}
		}
		t.Logf("%s: ok", op)
	}
	checkResolve(true)
	checkResolve(false)

	summary, err := c.CreateIssueComment(ctx, repo, number, note+" [summary]")
	if err != nil {
		t.Fatalf("create conversation comment: %v", err)
	}
	if _, err := c.UpdateIssueComment(ctx, repo, summary.DatabaseID, note+" [summary, updated]"); err != nil {
		t.Errorf("update conversation comment: %v", err)
	}
	t.Log("summary comment create and update: ok")
}

// sweep deletes every comment on the PR that carries nonce: review comments
// (replies before roots, which removes the threads) and conversation
// comments.
func sweep(t *testing.T, c *github.Client, repo github.Repo, number int, nonce string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	threads, err := c.ReviewThreads(ctx, repo, number)
	if err != nil {
		t.Errorf("cleanup: read threads: %v; delete comments containing %q by hand", err, nonce)
	}
	for _, th := range threads {
		for i := len(th.Comments) - 1; i >= 0; i-- {
			if cm := th.Comments[i]; strings.Contains(cm.Body, nonce) {
				if err := c.DeleteReviewComment(ctx, repo, cm.DatabaseID); err != nil {
					t.Errorf("cleanup: %v", err)
				}
			}
		}
	}
	comments, err := c.IssueComments(ctx, repo, number)
	if err != nil {
		t.Errorf("cleanup: list conversation comments: %v; delete comments containing %q by hand", err, nonce)
	}
	for _, cm := range comments {
		if strings.Contains(cm.Body, nonce) {
			if err := c.DeleteIssueComment(ctx, repo, cm.DatabaseID); err != nil {
				t.Errorf("cleanup: %v", err)
			}
		}
	}
}

// firstAddedLine returns the first added line in a text file.
func firstAddedLine(d *github.Diff) (github.Anchor, bool) {
	for i := range d.Files {
		f := &d.Files[i]
		if lines := f.AddedLines(); len(lines) > 0 && !f.Binary {
			return github.Anchor{Path: f.Path(), Line: lines[0], Side: github.SideRight}, true
		}
	}
	return github.Anchor{}, false
}

// checkMember checks that the token sees a teammate as one, by the rule the
// engine applies (design §1, "Public repositories"): the association on
// their comments, or else their permission. Both are logged.
func checkMember(ctx context.Context, t *testing.T, c *github.Client, repo github.Repo, threads []github.Thread, member string) {
	t.Helper()
	association := ""
	for i := range threads {
		for _, cm := range threads[i].Comments {
			if association == "" && strings.EqualFold(cm.Author, member) {
				association = cm.Association
			}
		}
	}
	t.Logf("%s's comments show association %q", member, association)
	if perm, err := c.Permission(ctx, repo, member); err != nil {
		t.Logf("%s's permission: %v", member, err)
	} else {
		t.Logf("%s's permission: %s", member, perm)
	}
	ok, err := review.NewTeam(c, repo).Has(ctx, member, association)
	switch {
	case err != nil:
		t.Errorf("check %s: %v", member, err)
	case !ok:
		t.Errorf("buggrit would count %s as outside the team and ignore their replies", member)
	}
}
