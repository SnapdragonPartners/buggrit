package review

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"

	"github.com/SnapdragonPartners/buggrit/internal/config"
	"github.com/SnapdragonPartners/buggrit/internal/github"
	"github.com/SnapdragonPartners/buggrit/internal/glob"
)

func findInput(t *testing.T, client llms.ChatClient) *FindInput {
	t.Helper()
	d := config.Defaults().Review
	return &FindInput{
		Client:           client,
		Workspace:        testWorkspace(),
		Diff:             testDiffParsed(t),
		Scope:            glob.Filter{Include: d.Include, Exclude: d.Exclude},
		InstructionFiles: d.InstructionFiles,
		Instructions:     "Pay attention to map initialization.",
		PR: Context{
			Number: 7, Title: "Add Put to Cache", BaseRef: "main", HeadSHA: "abc123",
			Body: "Adds Put.\n\n## What this PR does NOT do\n- No eviction.",
		},
		OtherThreads: []ThreadNote{{Path: "internal/cache/cache.go", Line: 4, Author: "copilot-pull-request-reviewer[bot]", Body: "Consider documenting New."}},
	}
}

const submitOne = `{"findings": [` + validFinding + `]}`

func TestFindHappyPath(t *testing.T) {
	fake := &testllm.FakeChatClient{Responses: []llms.ChatResponse{
		testllm.ToolCallResponse("1", "read_file", `{"path": "internal/cache/lru.go"}`),
		testllm.ToolCallResponse("2", "submit_findings", submitOne),
	}}
	res, err := Find(context.Background(), findInput(t, fake))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Complete || len(res.Findings) != 1 || res.Iterations != 2 {
		t.Fatalf("result = %+v", res)
	}
	calls := fake.Calls()
	if len(calls) != 2 {
		t.Fatalf("%d model calls, want 2", len(calls))
	}
	// The second call carries the read_file result.
	last := calls[1].Messages[len(calls[1].Messages)-1]
	if !strings.Contains(last.Content[0].ToolResult.Content, "same map bug") {
		t.Errorf("tool result not fed back: %+v", last)
	}
	if calls[0].System[0].Text != systemPrompt || len(calls[0].Tools) != 5 {
		t.Errorf("request setup: %d tools", len(calls[0].Tools))
	}
}

func TestFindOpeningMessage(t *testing.T) {
	msg := openingMessage(findInput(t, &testllm.FakeChatClient{}))
	for _, want := range []string{
		"# Pull request #7: Add Put to Cache",
		"## What this PR does NOT do",
		"### CLAUDE.md\n\nAlways initialize maps in constructors.",
		"### Review instructions\n\nPay attention to map initialization.",
		"- internal/cache/cache.go:4 (copilot-pull-request-reviewer[bot], open): Consider documenting New.",
		"go.sum (modified, +1 -1, excluded from review)",
		"     3 + func (c *Cache) Put",
		"=== old.txt (deleted)",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("opening message missing %q", want)
		}
	}
	if strings.Contains(msg, "=== go.sum") {
		t.Error("excluded file's diff was included")
	}
	if strings.Contains(msg, "AGENTS.md") {
		t.Error("missing instruction file was mentioned")
	}
}

func TestFindLargeDiffDefersToTools(t *testing.T) {
	in := findInput(t, &testllm.FakeChatClient{})
	h := &in.Diff.Files[0].Hunks[0]
	for i := range maxPromptDiffBytes / 20 {
		h.Lines = append(h.Lines, github.DiffLine{Kind: github.LineAdded, New: 100 + i, Text: "padding padding pad"})
	}
	second := in.Diff.Files[0]
	second.NewPath, second.OldPath = "internal/cache/cache2.go", "internal/cache/cache2.go"
	in.Diff.Files = append(in.Diff.Files, second)
	msg := openingMessage(in)
	// The first large file is shown up to the per-file cap, with a hint to
	// page on; the second no longer fits and is deferred to get_diff.
	if !strings.Contains(msg, `call get_diff with path "internal/cache/cache.go" and offset`) {
		t.Error("truncated file diff has no continuation hint")
	}
	if !strings.Contains(msg, "Call get_diff for each of these files:\ninternal/cache/cache2.go") {
		t.Error("file past the prompt budget was not deferred to get_diff")
	}
	if len(msg) > maxPromptDiffBytes+64<<10 {
		t.Errorf("opening message is %d bytes", len(msg))
	}
}

// The sections before the diff have their own budgets, so a large
// description, many instruction files and a huge file list can't push the
// opening message past maxPromptDiffBytes.
func TestFindOpeningMessageBudgets(t *testing.T) {
	in := findInput(t, &testllm.FakeChatClient{})
	ws := testWorkspace()
	in.InstructionFiles = nil
	for i := range 7 {
		name := fmt.Sprintf("docs/rules%d.md", i)
		ws[name] = &fstest.MapFile{Data: []byte(strings.Repeat("Always check errors.\n", maxInstructionBytes/25))}
		in.InstructionFiles = append(in.InstructionFiles, name)
	}
	in.Workspace = ws
	in.Instructions = strings.Repeat("Be thorough. ", 150)
	in.PR.Body = strings.Repeat("A long description. ", 20<<10)
	for i := range 5000 {
		p := fmt.Sprintf("gen/file%04d.go", i)
		in.Diff.Files = append(in.Diff.Files, github.FileDiff{OldPath: p, NewPath: p, Status: github.StatusModified})
	}
	msg := openingMessage(in)
	if len(msg) > maxPromptDiffBytes {
		t.Errorf("opening message is %d bytes, over the %d budget", len(msg), maxPromptDiffBytes)
	}
	for _, want := range []string{
		"### docs/rules0.md",
		"### docs/rules1.md",
		"[truncated; read_file docs/rules2.md for the rest]",
		"Not shown, for space; read them with read_file: docs/rules3.md, docs/rules4.md",
		"### Review instructions",
		"more files; call get_diff with no path and offset=",
		"more; page through the full list with get_diff and no path)",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("opening message missing %q", want)
		}
	}
}

func TestFindResubmitsAfterValidationError(t *testing.T) {
	bad := strings.Replace(submitOne, `"P1"`, `"P7"`, 1)
	fake := &testllm.FakeChatClient{Responses: []llms.ChatResponse{
		testllm.ToolCallResponse("1", "submit_findings", bad),
		testllm.ToolCallResponse("2", "submit_findings", submitOne),
	}}
	res, err := Find(context.Background(), findInput(t, fake))
	if err != nil || !res.Complete || res.Findings[0].Severity != "P1" {
		t.Fatalf("result = %+v, %v", res, err)
	}
	second := fake.Calls()[1].Messages
	if tr := second[len(second)-1].Content[0].ToolResult; !tr.IsError || !strings.Contains(tr.Content, "severity must be") {
		t.Errorf("validation error not shown to the model: %+v", tr)
	}
}

func TestFindNudgesOnce(t *testing.T) {
	fake := &testllm.FakeChatClient{Responses: []llms.ChatResponse{
		testllm.TextResponse("Looks good to me!"),
		testllm.ToolCallResponse("1", "submit_findings", `{"findings": []}`),
	}}
	res, err := Find(context.Background(), findInput(t, fake))
	if err != nil || !res.Complete || len(res.Findings) != 0 {
		t.Fatalf("result = %+v, %v", res, err)
	}
	second := fake.Calls()[1].Messages
	if got := second[len(second)-1].Content[0].Text; got != nudge {
		t.Errorf("last message = %q, want the nudge", got)
	}

	fake = &testllm.FakeChatClient{Text: "still just text"}
	res, err = Find(context.Background(), findInput(t, fake))
	if err != nil || res.Complete || !strings.Contains(res.Reason, "without submitting") {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if n := len(fake.Calls()); n != 2 {
		t.Errorf("%d model calls, want 2 (one nudge only)", n)
	}
}

func TestFindIterationCeiling(t *testing.T) {
	fake := &testllm.FakeChatClient{Responses: []llms.ChatResponse{
		testllm.ToolCallResponse("1", "list_dir", `{}`),
	}}
	in := findInput(t, fake)
	in.MaxIterations = 3
	res, err := Find(context.Background(), in)
	if err != nil || res.Complete || res.Reason != "tool iteration ceiling reached (3)" {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if n := len(fake.Calls()); n != 3 {
		t.Errorf("%d model calls, want 3", n)
	}
}

// buggrit's max_tool_iterations 0 means no ceiling; toolloop alone would stop
// at its default of 8.
func TestFindZeroIterationsIsUnlimited(t *testing.T) {
	var responses []llms.ChatResponse
	for i := range 12 {
		responses = append(responses, testllm.ToolCallResponse(fmt.Sprint(i), "list_dir", `{}`))
	}
	responses = append(responses, testllm.ToolCallResponse("s", "submit_findings", `{"findings": []}`))
	in := findInput(t, &testllm.FakeChatClient{Responses: responses})
	in.MaxIterations = 0
	res, err := Find(context.Background(), in)
	if err != nil || !res.Complete || res.Iterations != 13 {
		t.Fatalf("result = %+v, %v", res, err)
	}
}

func TestFindModelError(t *testing.T) {
	in := findInput(t, &testllm.FakeChatClient{Err: errors.New("upstream 503")})
	res, err := Find(context.Background(), in)
	if err != nil || res.Complete || !strings.Contains(res.Reason, "model error: ") || !strings.Contains(res.Reason, "upstream 503") {
		t.Fatalf("result = %+v, %v", res, err)
	}
}

func TestFindDeadline(t *testing.T) {
	fake := &testllm.FakeChatClient{Func: func(ctx context.Context, _ llms.ChatRequest) (llms.ChatResponse, error) {
		<-ctx.Done()
		return llms.ChatResponse{}, ctx.Err()
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	res, err := Find(ctx, findInput(t, fake))
	if err != nil || res.Complete || res.Reason != "deadline reached" {
		t.Fatalf("result = %+v, %v", res, err)
	}
}

// The deadline expiring inside a tool (grep checks the context) is an
// incomplete pass, not an error.
func TestFindDeadlineDuringTool(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := &testllm.FakeChatClient{Func: func(context.Context, llms.ChatRequest) (llms.ChatResponse, error) {
		cancel() // expires before grep's walk runs
		return testllm.ToolCallResponse("1", "grep", `{"pattern": "x"}`), nil
	}}
	res, err := Find(ctx, findInput(t, fake))
	if err != nil || res.Complete || res.Reason != "canceled" {
		t.Fatalf("result = %+v, %v", res, err)
	}

	dctx, dcancel := context.WithTimeout(context.Background(), time.Hour)
	defer dcancel()
	deadlineCtx, stop := context.WithDeadline(dctx, time.Now().Add(30*time.Millisecond))
	defer stop()
	fake = &testllm.FakeChatClient{Func: func(context.Context, llms.ChatRequest) (llms.ChatResponse, error) {
		time.Sleep(50 * time.Millisecond) // the deadline passes before the tool runs
		return testllm.ToolCallResponse("1", "grep", `{"pattern": "x"}`), nil
	}}
	res, err = Find(deadlineCtx, findInput(t, fake))
	if err != nil || res.Complete || res.Reason != "deadline reached" {
		t.Fatalf("result = %+v, %v", res, err)
	}
}

func TestFindUsageSummed(t *testing.T) {
	r1 := testllm.TextResponse("hm")
	r1.Usage = llms.Usage{InputTokens: 10, OutputTokens: 2}
	r2 := testllm.ToolCallResponse("1", "submit_findings", `{"findings": []}`)
	r2.Usage = llms.Usage{InputTokens: 20, OutputTokens: 3}
	res, err := Find(context.Background(), findInput(t, &testllm.FakeChatClient{Responses: []llms.ChatResponse{r1, r2}}))
	if err != nil || res.Usage.InputTokens != 30 || res.Usage.OutputTokens != 5 {
		t.Fatalf("usage = %+v, %v", res.Usage, err)
	}
}

func TestFindRequiresInputs(t *testing.T) {
	if _, err := Find(context.Background(), &FindInput{}); err == nil {
		t.Error("Find accepted an empty input")
	}
}
