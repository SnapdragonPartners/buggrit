package review

import (
	"context"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"

	"github.com/SnapdragonPartners/buggrit/internal/config"
	"github.com/SnapdragonPartners/buggrit/internal/github"
	"github.com/SnapdragonPartners/buggrit/internal/github/githubtest"
	"github.com/SnapdragonPartners/buggrit/internal/glob"
	"github.com/SnapdragonPartners/buggrit/internal/state"
)

const engineCompleted = "cccccccccccccccccccccccccccccccccccccccc"

// sinceDiff is what changed after the completed pass: the fix commit added
// Put (cache.go line 3), a merge from the base branch brought in main.go and
// a go.sum bump (excluded), and old.txt was deleted.
const sinceDiff = `diff --git a/internal/cache/cache.go b/internal/cache/cache.go
--- a/internal/cache/cache.go
+++ b/internal/cache/cache.go
@@ -1,4 +1,5 @@
 package cache
 type Cache struct{ entries map[string]int }
+func (c *Cache) Put(k string, v int) { c.entries[k] = v }
 func New() *Cache { return &Cache{} }
 // end
diff --git a/main.go b/main.go
--- a/main.go
+++ b/main.go
@@ -1 +1,2 @@
 package main
+func main() {}
diff --git a/go.sum b/go.sum
--- a/go.sum
+++ b/go.sum
@@ -1 +1 @@
-a v1
+a v2
diff --git a/old.txt b/old.txt
deleted file mode 100644
--- a/old.txt
+++ /dev/null
@@ -1,2 +0,0 @@
-first
-second
`

func parseDiff(t *testing.T, text string) *github.Diff {
	t.Helper()
	d, err := github.ParseDiff(text)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func defaultScope() glob.Filter {
	d := config.Defaults().Review
	return glob.Filter{Include: d.Include, Exclude: d.Exclude}
}

func TestNewIncremental(t *testing.T) {
	inc := newIncremental(engineCompleted, testDiffParsed(t), parseDiff(t, sinceDiff), defaultScope().InScope)
	if got := strings.Join(inc.paths(), " "); got != "internal/cache/cache.go old.txt" {
		t.Errorf("paths = %q, want cache.go and old.txt only", got)
	}
	if got := inc.ranges("internal/cache/cache.go"); got != "3" {
		t.Errorf("cache.go ranges = %q, want 3", got)
	}
	if got := inc.ranges("old.txt"); got != "the deletion" {
		t.Errorf("old.txt ranges = %q", got)
	}
	if inc.Empty() {
		t.Error("scope is empty")
	}
}

// A line the changes add that the PR's diff doesn't (base-branch content
// from a merge) is out of scope; a deletion puts its neighbors in scope.
func TestNewIncrementalLines(t *testing.T) {
	const since = `diff --git a/internal/cache/cache.go b/internal/cache/cache.go
--- a/internal/cache/cache.go
+++ b/internal/cache/cache.go
@@ -1,6 +1,5 @@
 package cache
 type Cache struct{ entries map[string]int }
 func (c *Cache) Put(k string, v int) { c.entries[k] = v }
-// stale
 func New() *Cache { return &Cache{} }
-// gone
+// end
`
	inc := newIncremental(engineCompleted, testDiffParsed(t), parseDiff(t, since), defaultScope().InScope)
	// Line 5 replaces a line since the completed pass but is context in
	// the PR, so neither it nor the replaced line's neighbors are in scope.
	if got := inc.ranges("internal/cache/cache.go"); got != "3-4" {
		t.Errorf("ranges = %q, want 3-4 (the neighbors of the deleted line)", got)
	}

	onlyMerge := newIncremental(engineCompleted, testDiffParsed(t), parseDiff(t, `diff --git a/main.go b/main.go
--- a/main.go
+++ b/main.go
@@ -1 +1,2 @@
 package main
+func main() {}
`), defaultScope().InScope)
	if !onlyMerge.Empty() {
		t.Errorf("a merge from the base branch put %v in scope", onlyMerge.paths())
	}
	var whole *Incremental
	if whole.Empty() {
		t.Error("a nil scope (the whole PR) reports empty")
	}
}

func TestIncrementalRanges(t *testing.T) {
	inc := &Incremental{lines: map[string]map[int]bool{"a": {1: true, 2: true, 3: true, 7: true, 9: true, 10: true}}}
	if got := inc.ranges("a"); got != "1-3, 7, 9-10" {
		t.Errorf("ranges = %q", got)
	}
}

func TestSubmitIncremental(t *testing.T) {
	s := testSubmit(t)
	s.incremental = newIncremental(engineCompleted, testDiffParsed(t), parseDiff(t, sinceDiff), defaultScope().InScope)
	tools := []toolloop.Tool{s.tool()}
	submit := func(finding string) toolloop.ToolResult {
		return call(t, tools, "submit_findings", `{"coverage": "complete", "findings": [`+finding+`]}`)
	}

	// Line 3 changed since the completed pass; its instances may be anywhere.
	wantOK(t, submit(validFinding))
	// A range that reaches a changed line is in scope.
	wantOK(t, submit(strings.Replace(validFinding, `"line": 3`, `"start_line": 2, "line": 3`, 1)))
	// File level, in a file with changes.
	wantOK(t, submit(strings.Replace(validFinding, `"line": 3`, `"line": 0`, 1)))
	// A deleted file's deleted line.
	wantOK(t, submit(`{"path": "old.txt", "side": "LEFT", "line": 1, "severity": "P2", "category": "correctness",
	  "confidence": "high", "title": "t", "problem": "p", "fix": "f"}`))

	wantErr(t, submit(strings.Replace(validFinding, `"line": 3`, `"line": 2`, 1)),
		"line 2 of internal/cache/cache.go hasn't changed since ccccccc, the last completed review. This pass reports new findings only on lines changed since then; in internal/cache/cache.go those are 3")

	// An invalid anchor gets only its own problem, not the scope's too.
	res := submit(strings.Replace(validFinding, `"line": 3`, `"line": 99`, 1))
	wantErr(t, res, "line 99 does not exist")
	if strings.Contains(res.Content, "hasn't changed") {
		t.Errorf("scope problem on an invalid anchor: %s", res.Content)
	}

	// No scope: anything the PR changes.
	s.incremental = nil
	wantOK(t, submit(strings.Replace(validFinding, `"line": 3`, `"line": 2`, 1)))
}

func TestSubmitIncrementalUnchangedFile(t *testing.T) {
	s := testSubmit(t)
	s.incremental = newIncremental(engineCompleted, testDiffParsed(t), parseDiff(t, `diff --git a/old.txt b/old.txt
deleted file mode 100644
--- a/old.txt
+++ /dev/null
@@ -1,2 +0,0 @@
-first
-second
`), defaultScope().InScope)
	res := call(t, []toolloop.Tool{s.tool()}, "submit_findings", `{"coverage": "complete", "findings": [`+validFinding+`]}`)
	wantErr(t, res, "internal/cache/cache.go has no changes since ccccccc, the last completed review. This pass reports new findings only on lines changed since then, in: old.txt")
}

// completePass records a complete pass at sha, as publishing does.
func completePass(t *testing.T, gh *github.Client, pass int, sha string) {
	t.Helper()
	body, err := state.Append("done", &state.Marker{ID: "pass", Pass: pass, SHA: sha, Status: state.PassComplete})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gh.CreateReview(context.Background(), engineRepo, 7, &github.Review{Body: body, CommitID: sha}); err != nil {
		t.Fatal(err)
	}
}

// incrementalSetup is a PR whose pass 1 completed at engineCompleted; the
// PR is now at engineHead.
func incrementalSetup(t *testing.T) (*githubtest.Server, *github.Client, PRRef) {
	t.Helper()
	fake := githubtest.New(t, "acme/widgets")
	fake.AddPR(7, engineCompleted, engineBase, testDiff)
	gh := fake.Client(githubtest.DefaultToken)
	completePass(t, gh, 1, engineCompleted)
	fake.Push(7, engineHead, testDiff)
	return fake, gh, PRRef{Repo: engineRepo, Number: 7, HeadSHA: engineHead}
}

func TestReviewIncremental(t *testing.T) {
	fake, gh, ref := incrementalSetup(t)
	fake.SetCompare(engineCompleted, engineHead, githubtest.Comparison{Status: github.CompareAhead, Diff: sinceDiff})
	model := &testllm.FakeChatClient{Responses: []llms.ChatResponse{
		testllm.ToolCallResponse("1", "submit_findings", `{"coverage": "complete", "findings": [`+strings.Replace(validFinding, `"line": 3`, `"line": 2`, 1)+`]}`),
		testllm.ToolCallResponse("2", "submit_findings", `{"coverage": "complete", "findings": [`+validFinding+`]}`),
	}}
	res, err := Review(context.Background(), ref, testWorkspace(), gh, engineOptions(model))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Complete || res.Pass != 2 || res.Scope != "lines changed since ccccccc" || len(res.Findings) != 1 {
		t.Fatalf("result = %+v", res)
	}
	calls := model.Calls()
	msg := calls[0].Messages[0].Content[0].Text
	for _, want := range []string{"## Scope of this pass", "through commit ccccccc", "- internal/cache/cache.go: 3\n", "- old.txt: the deletion\n"} {
		if !strings.Contains(msg, want) {
			t.Errorf("opening message missing %q", want)
		}
	}
	// The out-of-scope finding was refused, and the model told why.
	if len(calls) != 2 || !strings.Contains(lastToolResult(calls[1].Messages), "hasn't changed since ccccccc") {
		t.Errorf("out-of-scope finding wasn't refused with a reason")
	}
}

func lastToolResult(messages []llms.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		for _, c := range messages[i].Content {
			if c.ToolResult != nil {
				return c.ToolResult.Content
			}
		}
	}
	return ""
}

// Only a merge from the base branch since the completed pass: nothing to
// review, and the pass completes without the model.
func TestReviewIncrementalNothingChanged(t *testing.T) {
	fake, gh, ref := incrementalSetup(t)
	fake.SetCompare(engineCompleted, engineHead, githubtest.Comparison{Status: github.CompareAhead, Diff: `diff --git a/main.go b/main.go
--- a/main.go
+++ b/main.go
@@ -1 +1,2 @@
 package main
+func main() {}
`})
	model := &testllm.FakeChatClient{}
	res, err := Review(context.Background(), ref, testWorkspace(), gh, engineOptions(model))
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped || !res.Complete || res.Find != nil || len(res.Findings) != 0 || !strings.Contains(res.Summary, "Nothing reviewable changed since ccccccc") {
		t.Errorf("result = %+v", res)
	}
	if len(model.Calls()) != 0 {
		t.Error("the model was called with nothing in scope")
	}
}

// A force-push or rebase: the completed head isn't an ancestor of HEAD, or
// GitHub no longer has it. The pass reviews the whole PR.
func TestReviewIncrementalFallsBack(t *testing.T) {
	for name, tc := range map[string]struct {
		cmp  *githubtest.Comparison
		want string
	}{
		"diverged": {&githubtest.Comparison{Status: github.CompareDiverged}, "the whole pull request, since ccccccc, the last completed review's head, is no longer in its history"},
		"gone":     {nil, "the whole pull request, since GitHub no longer has ccccccc"},
	} {
		t.Run(name, func(t *testing.T) {
			fake, gh, ref := incrementalSetup(t)
			if tc.cmp != nil {
				fake.SetCompare(engineCompleted, engineHead, *tc.cmp)
			}
			model := submitting(`{"coverage": "complete", "findings": [` + strings.Replace(validFinding, `"line": 3`, `"line": 2`, 1) + `]}`)
			res, err := Review(context.Background(), ref, testWorkspace(), gh, engineOptions(model))
			if err != nil {
				t.Fatal(err)
			}
			if !res.Complete || len(res.Findings) != 1 || !strings.HasPrefix(res.Scope, tc.want) {
				t.Errorf("result = %+v", res)
			}
			if msg := model.Calls()[0].Messages[0].Content[0].Text; strings.Contains(msg, "## Scope of this pass") {
				t.Error("a whole-PR pass was given an incremental scope")
			}
		})
	}
}

func TestReviewIncrementalCompareError(t *testing.T) {
	fake, gh, ref := incrementalSetup(t)
	fake.OnRequest = func(op string) int {
		if op == "GET compare" {
			return 401
		}
		return 0
	}
	if _, err := Review(context.Background(), ref, testWorkspace(), gh, engineOptions(&testllm.FakeChatClient{})); err == nil {
		t.Error("a failed compare didn't fail the pass")
	}
}

// Files with changes since the completed pass come first in the diff, so
// they're the last deferred to get_diff when the diff doesn't fit.
func TestDiffOrderIncremental(t *testing.T) {
	in := &FindInput{Diff: testDiffParsed(t), Scope: defaultScope()}
	in.Incremental = newIncremental(engineCompleted, in.Diff, parseDiff(t, `diff --git a/old.txt b/old.txt
deleted file mode 100644
--- a/old.txt
+++ /dev/null
@@ -1,2 +0,0 @@
-first
-second
`), in.Scope.InScope)
	var got []string
	for _, f := range diffOrder(in) {
		got = append(got, f.Path())
	}
	if strings.Join(got, " ") != "old.txt internal/cache/cache.go" {
		t.Errorf("order = %v", got)
	}
}
