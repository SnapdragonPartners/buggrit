package publish

import (
	"context"
	"errors"
	"flag"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/buggrit/internal/github"
	"github.com/SnapdragonPartners/buggrit/internal/github/githubtest"
	"github.com/SnapdragonPartners/buggrit/internal/review"
	"github.com/SnapdragonPartners/buggrit/internal/state"
)

var update = flag.Bool("update", false, "rewrite golden files")

const (
	head = "1111111111111111111111111111111111111111"
	base = "0000000000000000000000000000000000000000"
)

const diff = `diff --git a/cache.go b/cache.go
--- a/cache.go
+++ b/cache.go
@@ -1,2 +1,3 @@
 package cache
+func (c *Cache) Put(k string, v int) { c.entries[k] = v }
 // end
`

var ref = review.PRRef{Repo: github.Repo{Owner: "acme", Name: "widgets"}, Number: 7, HeadSHA: head}

func classFinding() review.Candidate {
	return review.Candidate{
		ID: "f-7f3a2c", FP: "7f3a2c0011223344",
		Anchor: github.Anchor{Path: "cache.go", Side: github.SideRight, Line: 2},
		Finding: review.Finding{
			Path: "cache.go", Side: "RIGHT", Line: 2, Severity: "P1", Category: "correctness", Confidence: "high",
			Title:      "Nil map write when cache is cold",
			Problem:    "`New` leaves `c.entries` nil, so the first `Put` panics.",
			Impact:     "Any request before warm-up crashes the worker.",
			Fix:        "Initialize `c.entries` in `New`.",
			Suggestion: "func (c *Cache) Put(k string, v int) {\n\tif c.entries == nil {\n\t\tc.entries = map[string]int{}\n\t}\n\tc.entries[k] = v\n}",
			Class:      "uninitialized map on constructor path",
			Instances:  []review.Location{{Path: "lru.go", Line: 88}, {Path: "ttl.go"}},
		},
	}
}

func advisoryFileFinding() review.Candidate {
	return review.Candidate{
		ID: "f-0a1b2c", FP: "0a1b2c3344556677",
		Anchor: github.Anchor{Path: "cache.go"},
		Finding: review.Finding{
			Path: "cache.go", Side: "RIGHT", Severity: "P2", Category: "docs-drift", Confidence: "medium",
			Title: "README still says the cache is read-only", Problem: "README.md describes Get only.", Fix: "Document Put.",
		},
	}
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("%s differs (run go test -update to accept):\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}

func TestFindingBodyGolden(t *testing.T) {
	for name, c := range map[string]review.Candidate{"blocking-class.md": classFinding(), "advisory-file.md": advisoryFileFinding()} {
		body, err := FindingBody(&c, 2, head)
		if err != nil {
			t.Fatal(err)
		}
		golden(t, name, body)
		m, err := state.Parse(body)
		if err != nil || m.ID != c.ID || m.FP != c.FP || m.Pass != 2 || m.SHA != head || m.Severity != c.Severity {
			t.Errorf("%s marker = %+v, %v", name, m, err)
		}
	}
}

func TestFindingBodyFencesSuggestion(t *testing.T) {
	c := classFinding()
	c.Suggestion = "// example:\n// ```go\n// x := 1\n// ```"
	body, err := FindingBody(&c, 1, head)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "````suggestion\n") {
		t.Errorf("suggestion containing ``` needs a longer fence:\n%s", body)
	}
	c.Anchor.Line = 3 // narrowed: the suggestion would replace the wrong lines
	if body, _ = FindingBody(&c, 1, head); strings.Contains(body, "suggestion") {
		t.Error("suggestion posted on a moved anchor")
	}
}

func TestBodiesCapped(t *testing.T) {
	c := classFinding()
	c.Problem = strings.Repeat("é", 50000)
	body, err := FindingBody(&c, 1, head)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > 65536 || !strings.Contains(body, "[truncated]") {
		t.Errorf("body is %d bytes", len(body))
	}
	if _, err := state.Parse(body); err != nil {
		t.Errorf("marker lost when capping: %v", err)
	}
}

func TestReviewBodyGolden(t *testing.T) {
	cases := map[string]*review.Result{
		"review-complete.md": {HeadSHA: head, Pass: 2, Complete: true, Summary: "Adds Put; one real bug.",
			Findings: []review.Candidate{classFinding(), advisoryFileFinding()}, Duplicates: 1, BelowThreshold: 2},
		"review-clean.md":      {HeadSHA: head, Pass: 1, Complete: true},
		"review-incomplete.md": {HeadSHA: head, Pass: 3, Reason: "deadline reached"},
		"review-partial.md": {HeadSHA: head, Pass: 4, Reason: "deadline reached; submitted 1 finding found before it",
			Findings: []review.Candidate{classFinding()}},
	}
	for name, res := range cases {
		body, err := ReviewBody(res)
		if err != nil {
			t.Fatal(err)
		}
		golden(t, name, body)
		m, err := state.Parse(body)
		want := state.PassComplete
		if !res.Complete {
			want = state.PassIncomplete
		}
		if err != nil || !m.IsPass() || m.Status != want || m.Pass != res.Pass || m.SHA != head {
			t.Errorf("%s marker = %+v, %v", name, m, err)
		}
	}
}

func setup(t *testing.T) (*githubtest.Server, *github.Client) {
	t.Helper()
	fake := githubtest.New(t, "acme/widgets")
	fake.AddPR(7, head, base, diff)
	return fake, fake.Client(githubtest.DefaultToken)
}

func result() *review.Result {
	return &review.Result{HeadSHA: head, Pass: 1, Complete: true, Findings: []review.Candidate{classFinding(), advisoryFileFinding()}}
}

func TestPublish(t *testing.T) {
	fake, gh := setup(t)
	posted, err := Publish(context.Background(), gh, ref, result())
	if err != nil {
		t.Fatal(err)
	}
	if posted.Threads != 2 || posted.ReviewID == 0 || posted.FellBack {
		t.Errorf("posted = %+v", posted)
	}
	pr := fake.PR(7)
	if len(pr.Threads) != 2 || len(pr.Reviews) != 1 {
		t.Fatalf("threads = %d, reviews = %d", len(pr.Threads), len(pr.Reviews))
	}
	// The file-level thread goes first; the review, with the pass marker,
	// last.
	if !pr.Threads[0].FileLevel || pr.Threads[1].Line != 2 {
		t.Errorf("threads = %+v, %+v", pr.Threads[0], pr.Threads[1])
	}
	if m, err := state.Parse(pr.Reviews[0].Body); err != nil || m.Status != state.PassComplete {
		t.Errorf("review marker = %+v, %v", m, err)
	}
}

func TestPublishSkippedPostsNothing(t *testing.T) {
	fake, gh := setup(t)
	if _, err := Publish(context.Background(), gh, ref, &review.Result{Skipped: true, HeadSHA: head}); err != nil {
		t.Fatal(err)
	}
	if len(fake.PR(7).Reviews) != 0 {
		t.Error("a skipped pass posted a review")
	}
}

func TestPublishFenced(t *testing.T) {
	fake, gh := setup(t)
	fake.Push(7, "2222222222222222222222222222222222222222", diff)
	_, err := Publish(context.Background(), gh, ref, result())
	if !errors.Is(err, github.ErrHeadMoved) {
		t.Fatalf("err = %v, want head moved", err)
	}
	if pr := fake.PR(7); len(pr.Threads) != 0 || len(pr.Reviews) != 0 {
		t.Error("a fenced pass posted")
	}
}

// If GitHub rejects a line anchor, the review call posts nothing; the line
// findings are posted at file level instead, each once.
func TestPublishFallsBackToFileLevel(t *testing.T) {
	fake, gh := setup(t)
	rejected := false
	fake.OnRequest = func(op string) int {
		if op == "POST review" && !rejected {
			rejected = true
			return http.StatusUnprocessableEntity
		}
		return 0
	}
	posted, err := Publish(context.Background(), gh, ref, result())
	if err != nil {
		t.Fatal(err)
	}
	pr := fake.PR(7)
	if !posted.FellBack || posted.Threads != 2 || len(pr.Threads) != 2 || len(pr.Reviews) != 1 {
		t.Fatalf("posted = %+v, threads = %d, reviews = %d", posted, len(pr.Threads), len(pr.Reviews))
	}
	for _, th := range pr.Threads {
		if !th.FileLevel {
			t.Errorf("thread %+v is not file level", th)
		}
		if strings.Contains(th.Comments[0].Body, "```suggestion") {
			t.Error("a file-level fallback kept its suggestion, which has no lines to apply to")
		}
	}
}
