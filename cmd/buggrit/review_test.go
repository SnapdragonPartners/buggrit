package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/middleware"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"

	"github.com/SnapdragonPartners/buggrit/internal/config"
	"github.com/SnapdragonPartners/buggrit/internal/github/githubtest"
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
	finding = `{"findings": [{"path": "cache.go", "line": 2, "severity": "P1", "category": "correctness",
	  "confidence": "high", "title": "Nil map write", "problem": "entries is nil.", "fix": "Initialize it."}]}`
	cfgJSON = `{"version": 1, "models": {"default": {"provider": "openai-compatible", "base_url": "https://llm.example/v1", "model": "glm-5.3"}}}`
)

type reviewEnv struct {
	deps    reviewDeps
	fake    *githubtest.Server
	env     map[string]string
	model   *testllm.FakeChatClient
	summary string
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
		head: func(string) (string, error) { return head, nil },
		findClient: func(cfg *config.Config, _ middleware.Observer) (llms.ChatClient, error) {
			if cfg.Stage(config.StageFind).Model != "glm-5.3" {
				t.Errorf("config not read from the base branch: %+v", cfg.Models)
			}
			return model, nil
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

	// The head now has a completed pass, so the next run does nothing.
	code, out, _ = e.run(t)
	if code != 0 || !strings.Contains(out, "skipped at 1111111: a pass already completed") || len(e.fake.PR(7).Reviews) != 1 {
		t.Errorf("second run: exit %d, %s", code, out)
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
	e := setupReview(t, submit(`{"findings": []}`))
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
