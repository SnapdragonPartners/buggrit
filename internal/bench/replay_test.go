package bench

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/buggrit/internal/github/githubtest"
)

func TestGitHubReplayer(t *testing.T) {
	fake := githubtest.New(t, "o/r")
	r := &GitHubReplayer{GH: fake.Client(githubtest.DefaultToken)}
	target := &Target{Repo: "o/r", Base: shaA, Head: shaB, Title: "The change", Body: "Why."}
	tag := Tag("exp", WorkflowRun{ID: 42, Attempt: 2})

	replay, err := r.Open(context.Background(), target, tag)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Base != "bench/exp/42-2/base" || replay.Head != "bench/exp/42-2/head" || replay.PR <= 0 {
		t.Fatalf("replay = %+v", replay)
	}
	if b := fake.Branches(); b[replay.Base] != shaA || b[replay.Head] != shaB {
		t.Fatalf("branches = %v", b)
	}
	pr := fake.PR(replay.PR)
	if !pr.Draft || pr.Title != "The change" || !strings.HasPrefix(pr.Body, "Why.\n\n") || !strings.Contains(pr.Body, "benchmark replay of 6d36cd0...e619281") ||
		pr.HeadRef != replay.Head || pr.BaseRef != replay.Base || pr.HeadSHA != shaB {
		t.Fatalf("PR = %+v", pr)
	}

	if err := r.Close(context.Background(), replay); err != nil {
		t.Fatal(err)
	}
	if !pr.Closed || len(fake.Branches()) != 0 {
		t.Fatalf("after Close: closed %v, branches %v", pr.Closed, fake.Branches())
	}
	// Closing again is fine, so cleanup can be retried.
	if err := r.Close(context.Background(), replay); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// A replay that fails partway is cleaned up.
func TestGitHubReplayerOpenFailureCleansUp(t *testing.T) {
	fake := githubtest.New(t, "o/r")
	fake.OnRequest = func(op string) int {
		if op == "POST pull" {
			return http.StatusUnprocessableEntity
		}
		return 0
	}
	r := &GitHubReplayer{GH: fake.Client(githubtest.DefaultToken)}
	if _, err := r.Open(context.Background(), &Target{Repo: "o/r", Base: shaA, Head: shaB, Title: "T"}, "exp/1-1"); err == nil {
		t.Fatal("Open succeeded")
	}
	if b := fake.Branches(); len(b) != 0 {
		t.Fatalf("branches left behind: %v", b)
	}
}
