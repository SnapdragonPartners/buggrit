package review

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"

	"github.com/SnapdragonPartners/buggrit/internal/config"
	"github.com/SnapdragonPartners/buggrit/internal/github"
	"github.com/SnapdragonPartners/buggrit/internal/github/githubtest"
	"github.com/SnapdragonPartners/buggrit/internal/state"
)

const (
	engineHead = "1111111111111111111111111111111111111111"
	engineBase = "0000000000000000000000000000000000000000"
)

var engineRepo = github.Repo{Owner: "acme", Name: "widgets"}

func engineSetup(t *testing.T) (*githubtest.Server, *github.Client, PRRef) {
	t.Helper()
	fake := githubtest.New(t, "acme/widgets")
	fake.AddPR(7, engineHead, engineBase, testDiff)
	return fake, fake.Client(githubtest.DefaultToken), PRRef{Repo: engineRepo, Number: 7, HeadSHA: engineHead}
}

func engineOptions(client llms.ChatClient) *Options {
	cfg := config.Defaults()
	return &Options{Config: &cfg, Find: client}
}

// submitting returns a fake model that submits args at once.
func submitting(args string) *testllm.FakeChatClient {
	return &testllm.FakeChatClient{Responses: []llms.ChatResponse{testllm.ToolCallResponse("1", "submit_findings", args)}}
}

func TestReviewFullPass(t *testing.T) {
	fake, gh, ref := engineSetup(t)
	fake.AddThread(7, "copilot-pull-request-reviewer[bot]", "internal/cache/cache.go", 2, "Consider a constructor.")
	low := strings.Replace(validFinding, `"high"`, `"low"`, 1)
	fileLevel := `{"path": "internal/cache/cache.go", "line": 0, "severity": "P2", "category": "docs-drift",
	  "confidence": "high", "title": "README doesn't mention Put", "problem": "p", "fix": "f"}`
	model := submitting(`{"summary": "One real bug.", "coverage": "complete", "findings": [` + validFinding + `, ` + validFinding + `, ` + low + `, ` + fileLevel + `]}`)

	res, err := Review(context.Background(), ref, testWorkspace(), gh, engineOptions(model))
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped || !res.Complete || res.Pass != 1 || res.Summary != "One real bug." {
		t.Fatalf("result = %+v", res)
	}
	// The duplicate is dropped, and low confidence is under the default
	// min_confidence of medium.
	if len(res.Findings) != 2 || res.Duplicates != 1 || res.BelowThreshold != 1 {
		t.Fatalf("findings = %d, duplicates = %d, below threshold = %d", len(res.Findings), res.Duplicates, res.BelowThreshold)
	}
	line := res.Findings[0]
	if line.Anchor != (github.Anchor{Path: "internal/cache/cache.go", Side: github.SideRight, Line: 3}) || !strings.HasPrefix(line.ID, "f-") {
		t.Errorf("line finding = %+v", line)
	}
	if !res.Findings[1].Anchor.FileLevel() {
		t.Errorf("file-level finding anchored at %+v", res.Findings[1].Anchor)
	}
	// The finder was told about the other reviewer's thread.
	if msg := model.Calls()[0].Messages[0].Content[0].Text; !strings.Contains(msg, "Consider a constructor.") {
		t.Error("other reviewer's thread missing from the opening message")
	}
}

// buggrit's own earlier finding, matched by fingerprint, isn't raised again.
func TestReviewDedupesAgainstOwnThreads(t *testing.T) {
	fake, gh, ref := engineSetup(t)
	first, err := Review(context.Background(), ref, testWorkspace(), gh, engineOptions(submitting(`{"coverage": "complete", "findings": [`+validFinding+`]}`)))
	if err != nil || len(first.Findings) != 1 {
		t.Fatalf("first pass: %+v, %v", first, err)
	}
	c := first.Findings[0]
	body, err := state.Append("**P1 · correctness** · confidence: high — "+c.Title+"\n\nbody", &state.Marker{ID: c.ID, FP: c.FP, Severity: c.Severity, Pass: 1, SHA: engineHead})
	if err != nil {
		t.Fatal(err)
	}
	fake.AddThread(7, githubtest.DefaultLogin, c.Path, c.Line, body)

	model := submitting(`{"coverage": "complete", "findings": [` + validFinding + `]}`)
	second, err := Review(context.Background(), ref, testWorkspace(), gh, engineOptions(model))
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Findings) != 0 || second.Duplicates != 1 {
		t.Errorf("second pass: findings = %d, duplicates = %d", len(second.Findings), second.Duplicates)
	}
	// The finder sees the earlier finding by its title line, without the marker.
	msg := model.Calls()[0].Messages[0].Content[0].Text
	if !strings.Contains(msg, c.Title) || strings.Contains(msg, "buggrit:v1") {
		t.Error("own thread not summarized in the opening message")
	}
}

func TestReviewSkipsCompletedHead(t *testing.T) {
	fake, gh, ref := engineSetup(t)
	body, err := state.Append("done", &state.Marker{ID: "pass-1", Pass: 1, SHA: engineHead, Status: state.PassComplete})
	if err != nil {
		t.Fatal(err)
	}
	if _, perr := gh.CreateReview(context.Background(), engineRepo, 7, &github.Review{Body: body, CommitID: engineHead}); perr != nil {
		t.Fatal(perr)
	}
	model := &testllm.FakeChatClient{}
	res, err := Review(context.Background(), ref, testWorkspace(), gh, engineOptions(model))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Skipped || res.Pass != 2 || !strings.Contains(res.SkipReason, "already completed at 1111111") {
		t.Errorf("result = %+v", res)
	}
	if len(model.Calls()) != 0 {
		t.Error("a skipped pass called the model")
	}

	// An incomplete pass at the head doesn't count.
	fake.Push(7, "2222222222222222222222222222222222222222", testDiff)
	ref.HeadSHA = "2222222222222222222222222222222222222222"
	body, err = state.Append("partial", &state.Marker{ID: "pass-2", Pass: 2, SHA: ref.HeadSHA, Status: state.PassIncomplete})
	if err != nil {
		t.Fatal(err)
	}
	if _, perr := gh.CreateReview(context.Background(), engineRepo, 7, &github.Review{Body: body, CommitID: ref.HeadSHA}); perr != nil {
		t.Fatal(perr)
	}
	res, err = Review(context.Background(), ref, testWorkspace(), gh, engineOptions(submitting(`{"coverage": "complete", "findings": []}`)))
	if err != nil || res.Skipped || res.Pass != 3 {
		t.Errorf("after an incomplete pass: %+v, %v", res, err)
	}
}

func TestReviewHeadMoved(t *testing.T) {
	_, gh, ref := engineSetup(t)
	ref.HeadSHA = "9999999999999999999999999999999999999999"
	_, err := Review(context.Background(), ref, testWorkspace(), gh, engineOptions(&testllm.FakeChatClient{}))
	if !errors.Is(err, github.ErrHeadMoved) {
		t.Errorf("err = %v, want head moved", err)
	}
}

func TestReviewIncomplete(t *testing.T) {
	_, gh, ref := engineSetup(t)
	res, err := Review(context.Background(), ref, testWorkspace(), gh, engineOptions(&testllm.FakeChatClient{Err: errors.New("upstream 503")}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Complete || !strings.Contains(res.Reason, "upstream 503") || len(res.Findings) != 0 {
		t.Errorf("result = %+v", res)
	}
}

func TestWrapUpReserve(t *testing.T) {
	for d, want := range map[time.Duration]time.Duration{
		30 * time.Second: 15 * time.Second,
		2 * time.Minute:  time.Minute,
		30 * time.Minute: 7*time.Minute + 30*time.Second,
		2 * time.Hour:    10 * time.Minute,
	} {
		if got := wrapUpReserve(d); got != want {
			t.Errorf("wrapUpReserve(%s) = %s, want %s", d, got, want)
		}
	}
}
