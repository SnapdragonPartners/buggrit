package review

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"

	"github.com/SnapdragonPartners/buggrit/internal/github"
	"github.com/SnapdragonPartners/buggrit/internal/github/githubtest"
	"github.com/SnapdragonPartners/buggrit/internal/state"
)

// tracked builds an earlier finding on internal/cache/cache.go:3.
func tracked(t *testing.T, id, sev string, st state.FindingState, replies ...string) *state.Tracked {
	t.Helper()
	root, err := state.Append("**"+sev+" · correctness** · confidence: high — Nil map write\n\nPut writes a nil map.", &state.Marker{ID: id, FP: id + "fp", Severity: sev, Pass: 1, SHA: engineHead})
	if err != nil {
		t.Fatal(err)
	}
	th := &github.Thread{ID: "T-" + id, Path: "internal/cache/cache.go", Line: 3, OriginalLine: 2, Comments: []github.Comment{
		{DatabaseID: 1, Author: githubtest.DefaultLogin, Body: root, DiffHunk: "@@ -1,4 +1,5 @@\n+func (c *Cache) Put"},
	}}
	tr := &state.Tracked{Thread: th, Root: &th.Comments[0], ID: id, Severity: sev, State: st}
	for i, r := range replies {
		c := github.Comment{DatabaseID: int64(10 + i), Author: "dan", Body: r}
		th.Comments = append(th.Comments, c)
		tr.Pending = append(tr.Pending, c)
	}
	tr.Root = &th.Comments[0]
	return tr
}

func verifyInput(t *testing.T, client llms.ChatClient, checks ...Check) *VerifyInput {
	t.Helper()
	return &VerifyInput{
		Client: client, Workspace: testWorkspace(), Diff: testDiffParsed(t), MaxRebuttals: 1,
		PR:     Context{Number: 7, Title: "Add Put", BaseRef: "main", HeadSHA: engineHead, Body: "Adds Put."},
		Checks: checks, InstructionFiles: []string{"CLAUDE.md"},
	}
}

func TestVerifyHappyPath(t *testing.T) {
	a := tracked(t, "f-aaaaaa", "P1", state.StateOpen, "Fixed in 1111111.")
	b := tracked(t, "f-bbbbbb", "P2", state.StateOpen)
	fake := &testllm.FakeChatClient{Responses: []llms.ChatResponse{
		testllm.ToolCallResponse("1", "get_thread", `{"id": "f-aaaaaa"}`),
		testllm.ToolCallResponse("2", "submit_verdicts", `{"verdicts": [
		  {"id": "f-aaaaaa", "state": "fixed", "reason": "New initializes it.", "evidence_files": ["./internal/cache/cache.go", "internal/cache/cache.go"]},
		  {"id": "f-bbbbbb", "state": "open", "reason": "Still there."}]}`),
	}}
	res, err := Verify(context.Background(), verifyInput(t, fake, Check{Tracked: a, Why: "it has a new reply"}, Check{Tracked: b, Why: "it is open"}))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Complete || len(res.Verdicts) != 2 || res.Verdicts["f-aaaaaa"].State != state.StateFixed ||
		len(res.Verdicts["f-aaaaaa"].Evidence) != 1 || res.Reason != "" {
		t.Fatalf("result = %+v", res)
	}
	calls := fake.Calls()
	if calls[0].System[0].Text != verifySystemPrompt || len(calls[0].Tools) != 6 {
		t.Errorf("request setup: %d tools", len(calls[0].Tools))
	}
	msg := calls[0].Messages[0].Content[0].Text
	for _, want := range []string{
		"## Findings to verify (2)",
		"### f-aaaaaa (P1, blocking; state open)",
		"Anchor: internal/cache/cache.go:3 (line 2 when raised). Checked because it has a new reply.",
		"Put writes a nil map.",
		"+func (c *Cache) Put",
		"- dan: Fixed in 1111111.",
		"rebutted replies on this finding 0 of 1 times",
		"### f-bbbbbb (P2, advisory; state open)",
		"Always initialize maps in constructors.",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("opening message missing %q", want)
		}
	}
	if strings.Contains(msg, "buggrit:v1") {
		t.Error("the opening message shows a marker")
	}
	thread := calls[1].Messages[len(calls[1].Messages)-1].Content[0].ToolResult.Content
	if !strings.Contains(thread, "dan (new reply)\nFixed in 1111111.") || !strings.Contains(thread, "(the finding)") {
		t.Errorf("get_thread = %s", thread)
	}
}

func TestSubmitVerdictsValidation(t *testing.T) {
	open := tracked(t, "f-aaaaaa", "P1", state.StateOpen)
	replied := tracked(t, "f-bbbbbb", "P1", state.StateOpen, "intentional")
	tool := &verdictTool{checks: []Check{{Tracked: open}, {Tracked: replied}}}
	tools := []toolloop.Tool{tool.tool()}
	res := call(t, tools, "submit_verdicts", `{"verdicts": [
	  {"id": "f-aaaaaa", "state": "dismissed", "reason": "r"},
	  {"id": "f-aaaaaa", "state": "open", "reason": "r"},
	  {"id": "f-zzzzzz", "state": "open", "reason": "r"}]}`)
	for _, want := range []string{
		"verdicts[0] (f-aaaaaa): dismissed is only for a finding with a new reply",
		"verdicts[1]: f-aaaaaa has more than one verdict",
		`verdicts[2]: "f-zzzzzz" is not a listed finding`,
		"no verdict for f-bbbbbb",
	} {
		wantErr(t, res, want)
	}
	res = call(t, tools, "submit_verdicts", `{"verdicts": [
	  {"id": "f-aaaaaa", "state": "fixed", "reason": "", "evidence_files": ["other.go", "../x"]},
	  {"id": "f-bbbbbb", "state": "closed", "reason": "r", "evidence_files": ["a.go"]}]}`)
	for _, want := range []string{
		"including the anchor file internal/cache/cache.go",
		"evidence_files: ",
		"(f-aaaaaa): reason is required",
		`state must be fixed, open or dismissed, not "closed"`,
		"evidence_files is only for a fixed verdict",
	} {
		wantErr(t, res, want)
	}
	if tool.result != nil {
		t.Fatal("an invalid submission was accepted")
	}

	// At wrap-up, a partial submission is accepted.
	tool.partialOK = true
	res = call(t, tools, "submit_verdicts", `{"verdicts": [{"id": "f-bbbbbb", "state": "dismissed", "reason": "Fair."}]}`)
	wantOK(t, res, "accepted 1 verdicts")
	if !res.Stop || len(tool.result) != 1 {
		t.Errorf("result = %+v", tool.result)
	}
}

func TestGetThreadUnknownID(t *testing.T) {
	g := &threadTool{checks: []Check{{Tracked: tracked(t, "f-aaaaaa", "P1", state.StateOpen)}}}
	wantErr(t, call(t, []toolloop.Tool{g.tool()}, "get_thread", `{"id": "f-nope"}`), "the IDs are f-aaaaaa")
}

// The whole opening message is held to maxPromptDiffBytes: findings are
// added only while they fit, each shows at most maxPendingShown replies, and
// the ones left out are named for get_thread.
func TestVerifyOpeningBudget(t *testing.T) {
	in := verifyInput(t, &testllm.FakeChatClient{})
	ws := testWorkspace()
	in.InstructionFiles = nil
	for i := range 3 {
		name := fmt.Sprintf("docs/rules%d.md", i)
		ws[name] = &fstest.MapFile{Data: []byte(strings.Repeat("Always check errors.\n", maxInstructionBytes/20))}
		in.InstructionFiles = append(in.InstructionFiles, name)
	}
	in.Workspace = ws
	in.PR.Body = strings.Repeat("A long description. ", 20<<10)
	for i := range 5000 {
		p := fmt.Sprintf("gen/file%04d.go", i)
		in.Diff.Files = append(in.Diff.Files, github.FileDiff{OldPath: p, NewPath: p, Status: github.StatusModified})
	}
	replies := make([]string, maxPendingShown+3)
	for i := range replies {
		replies[i] = fmt.Sprintf("Reply %d. ", i) + strings.Repeat("I disagree. ", maxCheckText/10)
	}
	for i := range 4000 {
		tr := tracked(t, fmt.Sprintf("f-%06d", i), "P1", state.StateOpen, replies...)
		tr.Root.Body += strings.Repeat(" More detail.", maxCheckText/10)
		in.Checks = append(in.Checks, Check{Tracked: tr, Why: "it has new replies"})
	}
	msg := verifyOpening(in)
	if len(msg) > maxPromptDiffBytes {
		t.Errorf("opening message is %d bytes, over the %d budget", len(msg), maxPromptDiffBytes)
	}
	for _, want := range []string{
		"### f-000000 (P1, blocking; state open)",
		"New replies to judge, oldest first:\n\n- (3 earlier not shown: get_thread f-000000)\n- dan: Reply 3. ",
		fmt.Sprintf("- dan: Reply %d. ", len(replies)-1),
		"Not shown, for space; read each with get_thread: ",
		"more\n\nVerify every finding",
		"Verify every finding, then call submit_verdicts once with a verdict for each.\n",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("opening message missing %q", want)
		}
	}
	if n := strings.Count(msg, "\n- dan: "); n == 0 || n%maxPendingShown != 0 {
		t.Errorf("%d replies shown, want a multiple of %d", n, maxPendingShown)
	}
	if strings.Contains(msg, "- dan: Reply 2. ") {
		t.Error("an older reply is shown in place of a newer one")
	}
	if !regexp.MustCompile(`f-\d{6}, and \d+ more\n`).MatchString(msg) {
		t.Error("the list of findings not shown doesn't end with \"f-NNNNNN, and N more\"")
	}
	// The first finding left out is named, not dropped.
	shown := strings.Count(msg, "\n### f-")
	if !strings.Contains(msg, fmt.Sprintf("get_thread: f-%06d, ", shown)) {
		t.Errorf("%d findings shown; the list of the rest doesn't start at the next one", shown)
	}
}

// get_thread truncates each comment and adds it only if it fits, so a long
// thread stays within maxResultBytes.
func TestGetThreadBudget(t *testing.T) {
	replies := make([]string, 40)
	for i := range replies {
		replies[i] = strings.Repeat("x", 64<<10)
	}
	tr := tracked(t, "f-aaaaaa", "P1", state.StateOpen, replies...)
	tr.Root.DiffHunk = strings.Repeat("+line\n", 20<<10)
	g := &threadTool{checks: []Check{{Tracked: tr}}}
	res := call(t, []toolloop.Tool{g.tool()}, "get_thread", `{"id": "f-aaaaaa"}`)
	if len(res.Content) > maxResultBytes {
		t.Errorf("get_thread returned %d bytes, over %d", len(res.Content), maxResultBytes)
	}
	if !strings.Contains(res.Content, "more comments not shown]") || !strings.Contains(res.Content, "x…\n") {
		t.Errorf("get_thread didn't truncate:\n%.300s", res.Content)
	}
	if n := strings.Count(res.Content, "+line"); n > maxThreadComment/len("+line\n")+1 {
		t.Errorf("get_thread shows %d diff excerpt lines; it didn't truncate the excerpt", n)
	}
}

// Exactly maxPendingShown replies are all shown, with no note about
// earlier ones.
func TestVerifyOpeningShowsUpToMaxPending(t *testing.T) {
	replies := make([]string, maxPendingShown)
	for i := range replies {
		replies[i] = fmt.Sprintf("Reply %d.", i)
	}
	msg := verifyOpening(verifyInput(t, &testllm.FakeChatClient{}, Check{Tracked: tracked(t, "f-aaaaaa", "P1", state.StateOpen, replies...)}))
	if n := strings.Count(msg, "\n- dan: Reply "); n != maxPendingShown || strings.Contains(msg, "earlier not shown") {
		t.Errorf("%d replies shown, or a note about earlier ones:\n%s", n, msg)
	}
}

// Running out of time forces a submission of what the model has; the rest
// stays unverified, with the reason.
func TestVerifyWrapUpIsPartial(t *testing.T) {
	a := tracked(t, "f-aaaaaa", "P1", state.StateOpen)
	b := tracked(t, "f-bbbbbb", "P1", state.StateOpen)
	var wrapReq llms.ChatRequest
	slow := &testllm.FakeChatClient{Func: func(ctx context.Context, req llms.ChatRequest) (llms.ChatResponse, error) {
		if req.Purpose == "buggrit.verify.wrapup" {
			wrapReq = req
			return testllm.ToolCallResponse("w", "submit_verdicts", `{"verdicts": [{"id": "f-aaaaaa", "state": "open", "reason": "Still nil."}]}`), nil
		}
		<-ctx.Done()
		return llms.ChatResponse{}, ctx.Err()
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	in := verifyInput(t, slow, Check{Tracked: a}, Check{Tracked: b})
	in.WrapUp = 1900 * time.Millisecond
	res, err := Verify(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Complete || len(res.Verdicts) != 1 || res.Reason != "deadline reached; 1 of 2 earlier findings not verified" {
		t.Fatalf("result = %+v", res)
	}
	if got := res.Unverified(in.Checks); len(got) != 1 || got[0] != "f-bbbbbb" {
		t.Errorf("unverified = %v", got)
	}
	if wrapReq.ToolChoice.Name != "submit_verdicts" || wrapReq.Messages[len(wrapReq.Messages)-1].Content[0].Text != verifyTimeUp {
		t.Errorf("wrap-up request = %+v", wrapReq.ToolChoice)
	}
}

func TestVerifyNothingToCheck(t *testing.T) {
	res, err := Verify(context.Background(), verifyInput(t, &testllm.FakeChatClient{Err: errors.New("called")}))
	if err != nil || !res.Complete {
		t.Errorf("result = %+v, %v", res, err)
	}
}
