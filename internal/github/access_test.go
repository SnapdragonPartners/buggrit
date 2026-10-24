package github_test

import (
	"context"
	"testing"

	"github.com/SnapdragonPartners/buggrit/internal/github"
	"github.com/SnapdragonPartners/buggrit/internal/github/githubtest"
)

func TestPermissionAndAssociation(t *testing.T) {
	fake := githubtest.New(t, "acme/widgets")
	fake.AddPR(8, "head", "base", "")
	fake.SetAssociation("outsider", "NONE")
	fake.SetPermission("dan", "admin")
	fake.SetPermission("gone", "")
	c := fake.Client(githubtest.DefaultToken)
	repo := github.Repo{Owner: "acme", Name: "widgets"}
	ctx := context.Background()

	for login, want := range map[string]string{"dan": "admin", "outsider": "read"} {
		if got, err := c.Permission(ctx, repo, login); err != nil || got != want {
			t.Errorf("Permission(%s) = %q, %v; want %q", login, got, err, want)
		}
	}
	if _, err := c.Permission(ctx, repo, "gone"); !github.IsNotFound(err) {
		t.Errorf("Permission(gone) error = %v, want not found", err)
	}

	id := fake.AddThread(8, "dan", "a.go", 1, "root")
	fake.Reply(8, id, "outsider", "reply")
	fake.Reply(8, id, "copilot[bot]", "bot")
	threads, err := c.ReviewThreads(ctx, repo, 8)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"MEMBER", "NONE", "NONE"} {
		if got := threads[0].Comments[i].Association; got != want {
			t.Errorf("comment %d association = %q, want %q", i, got, want)
		}
	}

	for _, private := range []bool{false, true} {
		fake.Private = private
		pr, err := c.PullRequest(ctx, repo, 8)
		if err != nil || pr.Private != private {
			t.Errorf("Private = %v, %v; want %v", pr.Private, err, private)
		}
	}
}
