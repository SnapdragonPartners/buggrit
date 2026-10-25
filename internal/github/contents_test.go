package github_test

import (
	"context"
	"testing"

	"github.com/SnapdragonPartners/buggrit/internal/github"
)

func TestFileContent(t *testing.T) {
	fake, c := setup(t)
	fake.SetFile("main", ".github/buggrit.json", `{"version": 1}`)
	got, err := c.FileContent(context.Background(), repo, ".github/buggrit.json", "main")
	if err != nil || string(got) != `{"version": 1}` {
		t.Fatalf("FileContent = %q, %v", got, err)
	}
	_, err = c.FileContent(context.Background(), repo, ".github/buggrit.json", "other")
	if !github.IsNotFound(err) {
		t.Errorf("missing file: err = %v, want not found", err)
	}
}

func TestReviews(t *testing.T) {
	_, c := setup(t)
	postFindings(t, c)
	reviews, err := c.Reviews(context.Background(), repo, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(reviews) != 1 || reviews[0].Author != "buggrit-bot[bot]" || reviews[0].CommitSHA != head || reviews[0].ID == 0 || reviews[0].Body != "buggrit pass 1" ||
		reviews[0].SubmittedAt.IsZero() {
		t.Fatalf("reviews = %+v", reviews)
	}
}
