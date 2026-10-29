package main

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"

	"github.com/SnapdragonPartners/buggrit/internal/config"
	"github.com/SnapdragonPartners/buggrit/internal/github"
	"github.com/SnapdragonPartners/buggrit/internal/github/githubtest"
	"github.com/SnapdragonPartners/buggrit/internal/llm"
	"github.com/SnapdragonPartners/buggrit/internal/publish"
	"github.com/SnapdragonPartners/buggrit/internal/review"
	"github.com/SnapdragonPartners/buggrit/internal/state"
)

const (
	head = "1111111111111111111111111111111111111111"
	base = "0000000000000000000000000000000000000000"
	diff = `diff --git a/cache.go b/cache.go
--- a/cache.go
+++ b/cache.go
@@ -1,2 +1,3 @@
 package cache
+func (c *Cache) Put(k string, v int) { c.entries[k] = v }
 // end
`
	finding = `{"coverage": "complete", "findings": [{"path": "cache.go", "line": 2, "severity": "P1", "category": "correctness",
	  "confidence": "high", "title": "Nil map write", "problem": "entries is nil.", "fix": "Initialize it."}]}`
	cfgJSON = `{"version": 1, "models": {"default": {"provider": "openai-compatible", "base_url": "https://llm.example/v1", "model": "glm-5.3"}}}`
)

type reviewEnv struct {
	deps  reviewDeps
	fake  *githubtest.Server
	env   map[string]string
	model *testllm.FakeChatClient
	// verifier is the verify stage's client; nil until a test needs one.
	verifier *testllm.FakeChatClient
	summary  string
}

// setupReview runs review against the fake GitHub with a fake model, a
// workspace holding the PR head, and the config on the base branch.
func setupReview(t *testing.T, model *testllm.FakeChatClient) *reviewEnv {
	t.Helper()
	fake := githubtest.New(t, "acme/widgets")
	fake.AddPR(7, head, base, diff)
	fake.SetFile("main", config.Path, cfgJSON)

	ws := t.TempDir()
	src := "package cache\nfunc (c *Cache) Put(k string, v int) { c.entries[k] = v }\n// end\n"
	if err := os.WriteFile(filepath.Join(ws, "cache.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	summary := filepath.Join(t.TempDir(), "summary.md")
	e := &reviewEnv{fake: fake, model: model, summary: summary, env: map[string]string{
		"GITHUB_TOKEN": githubtest.DefaultToken, "GITHUB_REPOSITORY": "acme/widgets", "BUGGRIT_PR": "7",
		"GITHUB_WORKSPACE": ws, "GITHUB_API_URL": fake.URL(), "GITHUB_STEP_SUMMARY": summary,
	}}

	e.deps = reviewDeps{
		head:      func(string) (string, error) { return head, nil },
		ghOptions: []github.Option{github.WithSleep(func(ctx context.Context, _ time.Duration) error { return ctx.Err() })},
		clients: func(cfg *config.Config, _ modelObserver, _ *llm.Shared) (*llm.Clients, error) {
			if cfg.Stage(config.StageFind).Model != "glm-5.3" {
				t.Errorf("config not read from the base branch: %+v", cfg.Models)
			}
			return &llm.Clients{Find: model, Verify: e.verifier}, nil
		},
	}
	return e
}

func (e *reviewEnv) run(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = runReviewWith(context.Background(), e.deps, args, func(k string) string { return e.env[k] }, &out, &errOut)
	return code, out.String(), errOut.String()
}

func submit(args string) *testllm.FakeChatClient {
	return &testllm.FakeChatClient{Responses: []llms.ChatResponse{testllm.ToolCallResponse("1", "submit_findings", args)}}
}

func TestReviewPostsAndThenSkips(t *testing.T) {
	e := setupReview(t, submit(finding))
	code, out, errOut := e.run(t)
	if code != 0 || !strings.Contains(out, "complete at 1111111: 1 new finding (1 blocking, 0 advisory)") {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	pr := e.fake.PR(7)
	if len(pr.Threads) != 1 || len(pr.Reviews) != 1 || !strings.Contains(pr.Threads[0].Comments[0].Body, "Nil map write") {
		t.Fatalf("threads = %d, reviews = %d", len(pr.Threads), len(pr.Reviews))
	}
	summary, err := os.ReadFile(e.summary)
	if err != nil || !strings.Contains(string(summary), "| find | glm-5.3 | 1 |") {
		t.Errorf("job summary = %q, %v", summary, err)
	}

	// The head now has a completed pass, so the next run only checks the
	// open finding again, posting nothing.
	m, err := state.Parse(pr.Threads[0].Comments[0].Body)
	if err != nil {
		t.Fatal(err)
	}
	e.verifier = &testllm.FakeChatClient{Responses: []llms.ChatResponse{testllm.ToolCallResponse("1", "submit_verdicts",
		`{"verdicts": [{"id": "`+m.ID+`", "state": "open", "reason": "Still nil."}]}`)}}
	code, out, _ = e.run(t)
	if code != 0 || !strings.Contains(out, "complete at 1111111: earlier findings: 1 unchanged") ||
		len(e.fake.PR(7).Reviews) != 1 || len(pr.Threads[0].Comments) != 1 {
		t.Errorf("second run: exit %d, %s", code, out)
	}
}

// The fence can't read the head after the pass's model work: the run
// retries, then fails without posting, and says what the pass found and
// that nothing landed (#43).
func TestReviewUnreadableHeadReportsUnposted(t *testing.T) {
	var answered atomic.Bool
	e := setupReview(t, &testllm.FakeChatClient{Func: func(context.Context, llms.ChatRequest) (llms.ChatResponse, error) {
		answered.Store(true)
		return testllm.ToolCallResponse("1", "submit_findings", finding), nil
	}})
	var reads atomic.Int32
	e.fake.OnRequest = func(op string) int {
		// Once the model has answered, every PR read is the fence's.
		if op == "GET pull" && answered.Load() {
			reads.Add(1)
			return http.StatusNotFound
		}
		return 0
	}
	code, _, errOut := e.run(t)
	want := "It found: complete at 1111111: 1 new finding (1 blocking, 0 advisory). Posted before the failure: nothing."
	if code != 1 || !strings.Contains(errOut, "can't read the pull request head") || !strings.Contains(errOut, want) {
		t.Fatalf("exit %d\nstderr: %s", code, errOut)
	}
	if n := reads.Load(); n != 4 {
		t.Errorf("the fence read the PR %d times, want 4", n)
	}
	if pr := e.fake.PR(7); len(pr.Threads) != 0 || len(pr.Reviews) != 0 {
		t.Error("an unfenced pass posted")
	}
	summary, err := os.ReadFile(e.summary)
	if err != nil || !strings.Contains(string(summary), "Failed: publish: fence: can't read the pull request head") ||
		!strings.Contains(string(summary), want) {
		t.Errorf("job summary = %q, %v", summary, err)
	}
}

func TestReviewDryRunPostsNothing(t *testing.T) {
	e := setupReview(t, submit(finding))
	code, out, errOut := e.run(t, "--dry-run")
	if code != 0 || !strings.Contains(out, "=== cache.go:2 (RIGHT)") || !strings.Contains(out, "buggrit:v1") {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if pr := e.fake.PR(7); len(pr.Threads) != 0 || len(pr.Reviews) != 0 {
		t.Error("dry run posted")
	}
}

// A reply changes nothing: a run at the same head verifies the open
// finding again and posts nothing. Resolving the thread settles the
// finding, and then nothing needs verifying (design §6).
func TestReviewResolvingSettles(t *testing.T) {
	e := setupReview(t, submit(finding))
	if code, out, errOut := e.run(t); code != 0 {
		t.Fatalf("first run: exit %d\n%s\n%s", code, out, errOut)
	}
	th := e.fake.PR(7).Threads[0]
	m, err := state.Parse(th.Comments[0].Body)
	if err != nil {
		t.Fatal(err)
	}
	e.fake.Reply(7, th.ID, "dan", "This is intentional.")
	e.verifier = &testllm.FakeChatClient{Responses: []llms.ChatResponse{testllm.ToolCallResponse("1", "submit_verdicts",
		`{"verdicts": [{"id": "`+m.ID+`", "state": "open", "reason": "Nothing documents it."}]}`)}}
	code, out, _ := e.run(t)
	if code != 0 || !strings.Contains(out, "complete at 1111111: earlier findings: 1 unchanged") || len(th.Comments) != 2 {
		t.Fatalf("second run: exit %d, %s", code, out)
	}
	for _, msg := range e.verifier.Calls()[0].Messages {
		for _, part := range msg.Content {
			if strings.Contains(part.Text, "intentional") {
				t.Error("the reply reached the verifier")
			}
		}
	}
	summary, err := os.ReadFile(e.summary)
	if err != nil || !strings.Contains(string(summary), "| verify | glm-5.3 | 1 |") || !strings.Contains(string(summary), "only reconciled earlier findings") {
		t.Errorf("job summary = %s, %v", summary, err)
	}

	e.fake.SetResolved(7, th.ID, true)
	e.verifier = &testllm.FakeChatClient{Text: "not called"}
	code, out, _ = e.run(t)
	if code != 0 || !strings.Contains(out, "skipped at 1111111") || len(e.verifier.Calls()) != 0 || !e.fake.PR(7).Threads[0].Resolved {
		t.Fatalf("settled run: exit %d, %s", code, out)
	}
}

// A config that still sets convergence.max_rebuttals runs, with a warning.
func TestReviewWarnsAboutMaxRebuttals(t *testing.T) {
	e := setupReview(t, submit(finding))
	e.fake.SetFile("main", config.Path, `{"version": 1, "models": {"default": {"provider": "openai-compatible", "base_url": "https://llm.example/v1", "model": "glm-5.3"}},
	  "convergence": {"max_rebuttals": 1}}`)
	code, _, errOut := e.run(t)
	summary, err := os.ReadFile(e.summary)
	if code != 0 || !strings.Contains(errOut, "warning: convergence.max_rebuttals is ignored") || err != nil ||
		!strings.Contains(string(summary), "> [!WARNING]\n> convergence.max_rebuttals is ignored") {
		t.Errorf("exit %d\n%s\n%s", code, errOut, summary)
	}
}

// Earlier findings left unverified make the run fail, though the finder
// completed.
func TestReviewUnverifiedExitsNonZero(t *testing.T) {
	e := setupReview(t, submit(finding))
	if code, _, _ := e.run(t); code != 0 {
		t.Fatal("first run failed")
	}
	e.verifier = &testllm.FakeChatClient{Text: "just chatting"}
	code, out, _ := e.run(t)
	if code != 1 || !strings.Contains(out, "incomplete at 1111111: verification: the model stopped without calling submit_verdicts; 1 of 1 earlier finding not verified") {
		t.Fatalf("exit %d, %s", code, out)
	}
}

func TestReviewIncompleteExitsNonZero(t *testing.T) {
	e := setupReview(t, &testllm.FakeChatClient{Text: "no tools for me"})
	code, out, _ := e.run(t)
	if code != 1 || !strings.Contains(out, "incomplete at 1111111") {
		t.Fatalf("exit %d, %s", code, out)
	}
	if m := e.fake.PR(7).Reviews; len(m) != 1 || !strings.Contains(m[0].Body, `"status":"incomplete"`) {
		t.Errorf("the incomplete pass wasn't recorded: %+v", m)
	}
}

func TestReviewNothingToDo(t *testing.T) {
	t.Run("not configured", func(t *testing.T) {
		e := setupReview(t, submit(finding))
		fake := githubtest.New(t, "acme/widgets") // no config on the base branch
		fake.AddPR(7, head, base, diff)
		e.env["GITHUB_API_URL"], e.fake = fake.URL(), fake
		code, out, _ := e.run(t)
		if code != 0 || !strings.Contains(out, "buggrit is not configured: no .github/buggrit.json on main") {
			t.Errorf("exit %d, %s", code, out)
		}
	})
	t.Run("checkout is behind the head", func(t *testing.T) {
		e := setupReview(t, submit(finding))
		e.deps.head = func(string) (string, error) { return base, nil }
		code, out, _ := e.run(t)
		if code != 0 || !strings.Contains(out, "a newer run will review it") || len(e.model.Calls()) != 0 {
			t.Errorf("exit %d, %s", code, out)
		}
	})
}

func TestReviewConfigFlag(t *testing.T) {
	e := setupReview(t, submit(`{"coverage": "complete", "findings": []}`))
	path := filepath.Join(t.TempDir(), "local.json")
	if err := os.WriteFile(path, []byte(cfgJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := githubtest.New(t, "acme/widgets") // no config on the base branch
	fake.AddPR(7, head, base, diff)
	e.env["GITHUB_API_URL"] = fake.URL()
	if code, out, errOut := e.run(t, "--config", path, "--dry-run"); code != 0 || !strings.Contains(out, "no new findings") {
		t.Errorf("exit %d\n%s\n%s", code, out, errOut)
	}
}

func TestReviewArgs(t *testing.T) {
	e := setupReview(t, submit(finding))
	for name, mutate := range map[string]func(){
		"no token": func() { delete(e.env, "GITHUB_TOKEN") },
		"no repo":  func() { delete(e.env, "GITHUB_REPOSITORY") },
		"no pr":    func() { delete(e.env, "BUGGRIT_PR") },
		"bad repo": func() { e.env["GITHUB_REPOSITORY"] = "nope" },
	} {
		saved := map[string]string{}
		for k, v := range e.env {
			saved[k] = v
		}
		mutate()
		if code, _, errOut := e.run(t); code != 2 || errOut == "" {
			t.Errorf("%s: exit %d", name, code)
		}
		e.env = saved
	}
}

// The job summary states the pass's scope when it has one (design §7 rule 2).
func TestJobSummaryScope(t *testing.T) {
	cfg := config.Defaults()
	with := jobSummary(&review.Result{HeadSHA: head, Pass: 2, Complete: true, Scope: "lines changed since ccccccc"}, &cfg, nil)
	manual := jobSummary(&review.Result{HeadSHA: head, ReconcileOnly: true, Complete: true}, &cfg,
		[]publish.Manual{{ID: "f-abc123", URL: "https://example/1", Resolve: true}})
	if !strings.Contains(manual, "please do it by hand:\n- resolve f-abc123 https://example/1") {
		t.Errorf("summary without the manual list:\n%s", manual)
	}
	if !strings.Contains(with, "\nScope: lines changed since ccccccc.\n") {
		t.Errorf("summary with a scope:\n%s", with)
	}
	if without := jobSummary(&review.Result{HeadSHA: head, Pass: 1, Skipped: true, SkipReason: "done"}, &cfg, nil); strings.Contains(without, "Scope:") {
		t.Errorf("summary without a scope:\n%s", without)
	}
}

// The job summary lists comments the pass ignored (design §1).
func TestJobSummaryIgnored(t *testing.T) {
	cfg := config.Defaults()
	res := &review.Result{HeadSHA: head, Pass: 1, Skipped: true, Ignored: []review.Ignored{{Author: "outsider", URL: "https://example/2"}}}
	if got := jobSummary(res, &cfg, nil); !strings.Contains(got, "without write access to the repository:\n\n- outsider: https://example/2\n") {
		t.Errorf("summary without the ignored comment:\n%s", got)
	}
}

// The usage table shows cached input, which maestro-llms counts inside
// InputTokens.
func TestJobSummaryUsage(t *testing.T) {
	cfg := config.Defaults()
	cfg.Models.Default.Model = "m"
	res := &review.Result{HeadSHA: head, Pass: 1, Complete: true, Find: &review.FindResult{
		Iterations: 7,
		Usage:      llms.Usage{InputTokens: 1000, CacheReadTokens: 600, OutputTokens: 50, ReasoningTokens: 20},
	}}
	if got := jobSummary(res, &cfg, nil); !strings.Contains(got, "| find | m | 7 | 1000 | 600 | 50 | 20 |\n") {
		t.Errorf("summary:\n%s", got)
	}
}

func TestModelLogReportsReasoning(t *testing.T) {
	var b strings.Builder
	l := modelLog{w: &b}
	l.ObserveReasoning(config.StageFind, "low", &llms.ReasoningConfig{Effort: llms.ReasoningEffortLow})
	l.ObserveReasoning(config.StageVerify, "xhigh", nil)
	want := "buggrit: find reasoning_effort low: sent as low\n" +
		"buggrit: verify reasoning_effort xhigh: not supported by the provider; the model default applies\n"
	if b.String() != want {
		t.Errorf("log = %q, want %q", b.String(), want)
	}
}
