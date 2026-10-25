package publish

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"

	"github.com/SnapdragonPartners/buggrit/internal/config"
	"github.com/SnapdragonPartners/buggrit/internal/github"
	"github.com/SnapdragonPartners/buggrit/internal/github/githubtest"
	"github.com/SnapdragonPartners/buggrit/internal/review"
	"github.com/SnapdragonPartners/buggrit/internal/state"
)

// The multi-pass scenarios of design §11: passes run Review and Publish
// against the fake GitHub, with humans scripted between them.

const nilMap = `{"path": "cache.go", "line": 2, "severity": "%s", "category": "correctness", "confidence": "high",
  "title": "Nil map write", "problem": "entries is never initialized", "fix": "initialize it in New"}`

// world is a PR with a history of heads.
type world struct {
	t    *testing.T
	fake *githubtest.Server
	gh   *github.Client
	cfg  config.Config
	ws   fstest.MapFS
	// heads are the PR's heads so far, oldest first, and changes[i] the
	// files the push to heads[i] changed.
	heads   []string
	changes [][]string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	fake := githubtest.New(t, "acme/widgets")
	fake.AddPR(7, head, base, diff)
	return &world{
		t: t, fake: fake, gh: fake.Client(githubtest.DefaultToken), cfg: config.Defaults(),
		ws:    fstest.MapFS{"cache.go": {Data: []byte("package cache\nfunc (c *Cache) Put(k string, v int) { c.entries[k] = v }\n// end\n")}},
		heads: []string{head}, changes: [][]string{nil},
	}
}

func (w *world) head() string { return w.heads[len(w.heads)-1] }

// push moves the head to a new commit that changes files. The PR's own diff
// stays the same, so later passes have nothing new in scope for the finder
// unless a test says otherwise, and every earlier head compares as an
// ancestor with the files changed since it.
func (w *world) push(files ...string) {
	sha := strings.Repeat(fmt.Sprint(len(w.heads)+1), 40)
	w.fake.Push(7, sha, diff)
	w.heads, w.changes = append(w.heads, sha), append(w.changes, files)
	for i, h := range w.heads[:len(w.heads)-1] {
		var d strings.Builder
		for _, ch := range w.changes[i+1:] {
			for _, f := range ch {
				fmt.Fprintf(&d, "diff --git a/%s b/%s\n--- a/%s\n+++ b/%s\n@@ -1 +1 @@\n-x\n+y\n", f, f, f, f)
			}
		}
		w.fake.SetCompare(h, sha, githubtest.Comparison{Status: github.CompareAhead, Diff: d.String()})
	}
}

// model is a fake that makes one terminal call, or fails if called when
// args is "".
func model(tool, args string) *testllm.FakeChatClient {
	if args == "" {
		return &testllm.FakeChatClient{Err: errors.New("unexpected model call")}
	}
	return &testllm.FakeChatClient{Responses: []llms.ChatResponse{testllm.ToolCallResponse("1", tool, args)}}
}

type passResult struct {
	res            *review.Result
	posted         *Posted
	find, verifier *testllm.FakeChatClient
	err            error
}

// pass runs one review and publishes it. find is the finder's
// submit_findings arguments and verify the verifier's submit_verdicts; ""
// means that stage must not run.
func (w *world) pass(find, verify string) passResult {
	w.t.Helper()
	p := passResult{find: model("submit_findings", find), verifier: model("submit_verdicts", verify)}
	ref := review.PRRef{Repo: ref.Repo, Number: 7, HeadSHA: w.head()}
	res, err := review.Review(context.Background(), ref, w.ws, w.gh, &review.Options{Config: &w.cfg, Find: p.find, Verify: p.verifier})
	if err != nil {
		w.t.Fatalf("review: %v", err)
	}
	p.res = res
	p.posted, p.err = Publish(context.Background(), w.gh, ref, res)
	if find == "" && len(p.find.Calls()) > 0 {
		w.t.Error("the finder ran")
	}
	if verify == "" && len(p.verifier.Calls()) > 0 {
		w.t.Error("the verifier ran")
	}
	return p
}

// raise runs the first pass, which posts one finding of severity sev, and
// returns its thread.
func (w *world) raise(sev string) (*githubtest.Thread, string) {
	w.t.Helper()
	p := w.pass(`{"coverage": "complete", "findings": [`+fmt.Sprintf(nilMap, sev)+`]}`, "")
	if p.err != nil || len(p.res.Findings) != 1 {
		w.t.Fatalf("first pass: %+v, %v", p.res, p.err)
	}
	return w.fake.PR(7).Threads[0], p.res.Findings[0].ID
}

func verdict(id, st, reason string, evidence ...string) string {
	ev := ""
	if len(evidence) > 0 {
		ev = `, "evidence_files": ["` + strings.Join(evidence, `", "`) + `"]`
	}
	return fmt.Sprintf(`{"verdicts": [{"id": %q, "state": %q, "reason": %q%s}]}`, id, st, reason, ev)
}

const noFindings = `{"coverage": "complete", "findings": []}`

// lastTransition is the marker on the thread's last comment, which must be
// a buggrit transition.
func lastTransition(t *testing.T, th *githubtest.Thread) *state.Marker {
	t.Helper()
	c := th.Comments[len(th.Comments)-1]
	m, err := state.Parse(c.Body)
	if err != nil || c.Author != githubtest.DefaultLogin || !m.IsTransition() {
		t.Fatalf("last comment is not a buggrit transition: %q by %s (%v)", c.Body, c.Author, err)
	}
	return m
}

func TestLifecycleFixThenRegress(t *testing.T) {
	w := newWorld(t)
	th, id := w.raise("P1")

	w.push("cache.go")
	p := w.pass("", verdict(id, "fixed", "New now initializes entries.", "cache.go", "new.go"))
	if p.err != nil || p.posted.Replies != 1 || p.posted.Resolved != 1 || !th.Resolved {
		t.Fatalf("fix: %+v, %v; resolved %v", p.posted, p.err, th.Resolved)
	}
	m := lastTransition(t, th)
	if m.Kind != state.KindFixed || m.SHA != w.head() || len(m.Evidence) != 2 {
		t.Errorf("fixed marker = %+v", m)
	}
	if !strings.Contains(th.Comments[1].Body, "**Fixed** at `2222222`. New now initializes entries.") {
		t.Errorf("fixed reply = %q", th.Comments[1].Body)
	}
	if !strings.Contains(Status(p.res), "complete at 2222222: no new findings; earlier findings: 1 fixed") {
		t.Errorf("status = %s", Status(p.res))
	}

	// A push that touches none of the evidence leaves the verdict standing:
	// no model, no mutation.
	w.push("other.go")
	p = w.pass("", "")
	if p.err != nil || len(p.res.Checks) != 0 || p.posted.Replies+p.posted.Resolved+p.posted.Unresolved != 0 {
		t.Fatalf("unrelated push: %+v, %v", p.posted, p.err)
	}

	// Touching an evidence file in another file re-verifies it, and the
	// defect is back.
	w.push("new.go")
	p = w.pass("", verdict(id, "open", "The initialization was removed from New."))
	if p.err != nil || th.Resolved || lastTransition(t, th).Kind != state.KindRegressed {
		t.Fatalf("regression: %+v, %v", p.posted, p.err)
	}
	if !strings.Contains(p.res.Checks[0].Why, "new.go") {
		t.Errorf("why = %q", p.res.Checks[0].Why)
	}
}

// Resolving a thread settles its finding: it's never verified again, and
// buggrit leaves the thread alone. Unresolving it opens it again (§6).
func TestLifecycleResolutionSettles(t *testing.T) {
	w := newWorld(t)
	th, id := w.raise("P1")
	w.fake.SetResolved(7, th.ID, true)

	w.push("cache.go")
	p := w.pass("", "")
	if p.err != nil || len(p.res.Checks) != 0 || !th.Resolved || len(th.Comments) != 1 || p.posted.Unresolved != 0 {
		t.Fatalf("settled: %+v, %v", p.posted, p.err)
	}

	w.fake.SetResolved(7, th.ID, false)
	w.push("cache.go")
	p = w.pass("", verdict(id, "open", "entries is still nil in New."))
	if p.err != nil || len(p.res.Checks) != 1 || th.Resolved || len(th.Comments) != 1 {
		t.Fatalf("unsettled: %+v, %v", p.posted, p.err)
	}
}

// A reply carries no state: a finding still present gets no reply, and
// the thread stays open (§6).
func TestLifecycleRepliesChangeNothing(t *testing.T) {
	w := newWorld(t)
	th, id := w.raise("P2")
	w.fake.Reply(7, th.ID, "dan", "Won't fix: deferred to #123.")
	w.push("cache.go")
	p := w.pass("", verdict(id, "open", "Still nil."))
	if p.err != nil || len(th.Comments) != 2 || th.Resolved || p.posted.Replies != 0 {
		t.Fatalf("reply: %+v, %v; %d comments", p.posted, p.err, len(th.Comments))
	}
	if !strings.Contains(Status(p.res), "earlier findings: 1 unchanged") {
		t.Errorf("status = %s", Status(p.res))
	}
}

// A thread resolved while the run that judged it fixed was still going
// stays settled: buggrit posts its fixed reply, but doesn't resolve the
// thread again, so the resolution stays the person's (§6).
func TestLifecycleSettledMidRun(t *testing.T) {
	w := newWorld(t)
	th, id := w.raise("P1")
	w.push("cache.go")
	verifier := &testllm.FakeChatClient{Func: func(context.Context, llms.ChatRequest) (llms.ChatResponse, error) {
		w.fake.SetResolved(7, th.ID, true)
		return testllm.ToolCallResponse("1", "submit_verdicts", verdict(id, "fixed", "Initialized in New.", "cache.go")), nil
	}}
	pref := review.PRRef{Repo: ref.Repo, Number: 7, HeadSHA: w.head()}
	res, err := review.Review(context.Background(), pref, w.ws, w.gh, &review.Options{Config: &w.cfg, Find: model("submit_findings", ""), Verify: verifier})
	if err != nil {
		t.Fatal(err)
	}
	posted, err := Publish(context.Background(), w.gh, pref, res)
	if err != nil || posted.Resolved != 0 || th.ResolvedBy != "dratner" || lastTransition(t, th).Kind != state.KindFixed {
		t.Fatalf("resolved %d, by %q, err %v", posted.Resolved, th.ResolvedBy, err)
	}
	w.push("cache.go")
	if p := w.pass("", ""); p.err != nil || len(p.res.Checks) != 0 {
		t.Fatalf("a settled finding was verified: %+v", p.res.Checks)
	}
}

// A person settling a thread buggrit resolved, while the run that finds
// it regressed is still going, keeps their resolution: buggrit unresolves
// only a thread still resolved by buggrit (§6).
func TestLifecycleSettledDuringRegression(t *testing.T) {
	w := newWorld(t)
	th, id := w.raise("P1")
	w.push("cache.go")
	if p := w.pass("", verdict(id, "fixed", "Initialized in New.", "cache.go")); p.err != nil || th.ResolvedBy != githubtest.DefaultLogin {
		t.Fatalf("fix: %v, resolved by %q", p.err, th.ResolvedBy)
	}
	w.push("cache.go")
	verifier := &testllm.FakeChatClient{Func: func(context.Context, llms.ChatRequest) (llms.ChatResponse, error) {
		w.fake.ResolveAs(7, th.ID, true, "dratner") // unresolved and resolved again by a person
		return testllm.ToolCallResponse("1", "submit_verdicts", verdict(id, "open", "The initialization is gone.")), nil
	}}
	pref := review.PRRef{Repo: ref.Repo, Number: 7, HeadSHA: w.head()}
	res, err := review.Review(context.Background(), pref, w.ws, w.gh, &review.Options{Config: &w.cfg, Find: model("submit_findings", ""), Verify: verifier})
	if err != nil {
		t.Fatal(err)
	}
	posted, err := Publish(context.Background(), w.gh, pref, res)
	if err != nil || posted.Unresolved != 0 || !th.Resolved || th.ResolvedBy != "dratner" || lastTransition(t, th).Kind != state.KindRegressed {
		t.Fatalf("unresolved %d, resolved %v by %q, err %v", posted.Unresolved, th.Resolved, th.ResolvedBy, err)
	}
	w.push("cache.go")
	if p := w.pass("", ""); p.err != nil || len(p.res.Checks) != 0 || p.posted.Unresolved != 0 {
		t.Fatalf("the settled finding was touched: %+v, %+v", p.res.Checks, p.posted)
	}
}

// If buggrit can't read a thread's resolution before resolving it, it
// doesn't resolve blind, which could take a settler's resolution (§6).
func TestLifecycleResolveNeedsFreshRead(t *testing.T) {
	w := newWorld(t)
	th, id := w.raise("P1")
	w.push("cache.go")
	resolves := 0
	w.fake.OnRequest = func(op string) int {
		switch op {
		case "graphql ThreadResolution":
			return http.StatusBadGateway
		case "graphql ResolveThread":
			resolves++
		}
		return 0
	}
	p := w.pass("", verdict(id, "fixed", "Initialized in New.", "cache.go"))
	if p.err == nil || resolves != 0 || th.Resolved {
		t.Fatalf("err %v, %d resolves, resolved %v", p.err, resolves, th.Resolved)
	}
}

// A run killed between a regressed reply and its unresolve leaves an open
// finding on a thread buggrit resolved. The next run, even a sync, reopens
// it without a second reply (§6).
func TestLifecycleRepairsKilledRegression(t *testing.T) {
	w := newWorld(t)
	th, id := w.raise("P1")
	w.push("cache.go")
	if p := w.pass("", verdict(id, "fixed", "Initialized in New.", "cache.go")); p.err != nil || th.ResolvedBy != githubtest.DefaultLogin {
		t.Fatalf("fix: %v, resolved by %q", p.err, th.ResolvedBy)
	}
	w.push("cache.go")
	w.fake.OnRequest = func(op string) int {
		if op == "graphql UnresolveThread" {
			return http.StatusBadGateway
		}
		return 0
	}
	if p := w.pass("", verdict(id, "open", "The initialization is gone.")); p.err == nil || !th.Resolved {
		t.Fatalf("killed run: err %v, resolved %v", p.err, th.Resolved)
	}
	w.fake.OnRequest = nil
	comments := len(th.Comments)
	pref := review.PRRef{Repo: ref.Repo, Number: 7, HeadSHA: w.head()}
	res, err := review.Review(context.Background(), pref, nil, w.gh, &review.Options{Config: &w.cfg, Sync: true})
	if err != nil {
		t.Fatal(err)
	}
	posted, err := Publish(context.Background(), w.gh, pref, res)
	if err != nil || th.Resolved || len(th.Comments) != comments || posted.Unresolved != 1 || posted.ReviewID != 0 {
		t.Fatalf("sync repair: %+v, %v; resolved %v", posted, err, th.Resolved)
	}
	if got := Status(res); got != "synced at 3333333: earlier findings: 1 repaired" {
		t.Errorf("status = %s", got)
	}
}

// A fixed finding found still fixed gets no reply, but the pass marker
// records the re-verification with that verdict's evidence files, which
// become its baseline (§6, "Evidence baseline").
func TestLifecycleReverificationMovesBaseline(t *testing.T) {
	w := newWorld(t)
	th, id := w.raise("P1")
	w.push("cache.go")
	if p := w.pass("", verdict(id, "fixed", "Initialized in New.", "cache.go", "old.go")); p.err != nil {
		t.Fatal(p.err)
	}
	comments := len(th.Comments)

	// old.go changed; the fix now lives in cache.go alone.
	w.push("old.go")
	p := w.pass("", verdict(id, "fixed", "Still initialized in New.", "cache.go"))
	if p.err != nil || len(th.Comments) != comments || !th.Resolved {
		t.Fatalf("re-verified: %+v, %v", p.posted, p.err)
	}
	reviews := w.fake.PR(7).Reviews
	m, err := state.Parse(reviews[len(reviews)-1].Body)
	if err != nil || len(m.Verified[id]) != 1 || m.Verified[id][0] != "cache.go" || m.SHA != w.head() {
		t.Fatalf("pass marker = %+v, %v", m, err)
	}

	// old.go no longer matters.
	w.push("old.go")
	if p = w.pass("", ""); p.err != nil || len(p.res.Checks) != 0 {
		t.Fatalf("re-verified again: %+v", p.res.Checks)
	}
	w.push("cache.go")
	if p = w.pass("", verdict(id, "fixed", "Still initialized in New.", "cache.go")); p.err != nil || len(p.res.Checks) != 1 {
		t.Fatalf("not re-verified: %+v, %v", p.res.Checks, p.err)
	}
}

// A reconcile-only run that re-verifies a fixed finding posts a short
// review to record it, which the baseline doesn't count as a pass.
func TestLifecycleReconcileRecordsReverification(t *testing.T) {
	w := newWorld(t)
	_, id := w.raise("P1")
	w.push("cache.go")
	if p := w.pass("", verdict(id, "fixed", "Initialized in New.", "cache.go")); p.err != nil {
		t.Fatal(p.err)
	}
	w.push("cache.go")
	pref := review.PRRef{Repo: ref.Repo, Number: 7, HeadSHA: w.head()}
	silent := &testllm.FakeChatClient{Text: "I would rather chat."}
	res, err := review.Review(context.Background(), pref, w.ws, w.gh, &review.Options{Config: &w.cfg, Find: model("submit_findings", ""), Verify: silent})
	if err != nil {
		t.Fatal(err)
	}
	if _, perr := Publish(context.Background(), w.gh, pref, res); perr != nil || len(res.Unverified) != 1 {
		t.Fatalf("unverified pass: %v, %v", res.Unverified, perr)
	}

	p := w.pass("", verdict(id, "fixed", "Still initialized in New.", "cache.go"))
	reviews := w.fake.PR(7).Reviews
	m, err := state.Parse(reviews[len(reviews)-1].Body)
	if p.err != nil || !p.res.ReconcileOnly || err != nil || m.IsPass() || len(m.Verified[id]) != 1 {
		t.Fatalf("reconcile review: %+v, %v; marker %+v", p.posted, p.err, m)
	}
	if !strings.Contains(reviews[len(reviews)-1].Body, "Re-checked 1 fixed finding whose files changed; still fixed.") {
		t.Errorf("review body = %s", reviews[len(reviews)-1].Body)
	}
	if p = w.pass("", ""); p.err != nil || !p.res.Skipped || p.res.Pass != 4 {
		t.Fatalf("after the record: %+v, %v", p.res, p.err)
	}
}

// A thread resolved by an account GitHub no longer reports isn't settled,
// so its finding is still verified, but buggrit never unresolves it; the
// review asks a person (§6).
func TestLifecycleUnknownResolver(t *testing.T) {
	w := newWorld(t)
	th, id := w.raise("P1")
	w.fake.ResolveAs(7, th.ID, true, "")
	w.push("cache.go")
	p := w.pass("", verdict(id, "open", "Still nil."))
	if p.err != nil || !th.Resolved || p.posted.Unresolved != 0 || len(p.res.UnknownResolver) != 1 {
		t.Fatalf("unknown resolver: %+v, %v", p.posted, p.err)
	}
	reviews := w.fake.PR(7).Reviews
	if body := reviews[len(reviews)-1].Body; !strings.Contains(body, "GitHub doesn't say by whom") || !strings.Contains(body, "- "+id+" ") {
		t.Errorf("review body:\n%s", body)
	}
}

// A fixed finding whose thread's resolver GitHub doesn't report, found
// regressed, is open but left resolved, so it's listed for a person too.
func TestLifecycleUnknownResolverRegressed(t *testing.T) {
	w := newWorld(t)
	th, id := w.raise("P1")
	w.push("cache.go")
	if p := w.pass("", verdict(id, "fixed", "Initialized in New.", "cache.go")); p.err != nil {
		t.Fatal(p.err)
	}
	w.fake.ResolveAs(7, th.ID, true, "")
	w.push("cache.go")
	p := w.pass("", verdict(id, "open", "The initialization is gone."))
	if p.err != nil || !th.Resolved || lastTransition(t, th).Kind != state.KindRegressed || len(p.res.UnknownResolver) != 1 {
		t.Fatalf("regressed: %+v, %v; unknown %d", p.posted, p.err, len(p.res.UnknownResolver))
	}
}

// A run killed between the fixed reply and the resolve is repaired by the
// next one, without a second reply (§6, "Repair every pass").
func TestLifecycleRepairsKilledPublish(t *testing.T) {
	w := newWorld(t)
	th, id := w.raise("P1")
	w.push("cache.go")
	w.fake.OnRequest = func(op string) int {
		if op == "graphql ResolveThread" {
			return http.StatusBadGateway
		}
		return 0
	}
	p := w.pass("", verdict(id, "fixed", "Initialized in New.", "cache.go"))
	if p.err == nil || th.Resolved || len(th.Comments) != 2 {
		t.Fatalf("killed publish: err %v, resolved %v, %d comments", p.err, th.Resolved, len(th.Comments))
	}
	w.fake.OnRequest = nil

	// The pass marker never landed, so the next run is a full pass again;
	// the fix is at this head, so it isn't re-verified, only resolved.
	p = w.pass(noFindings, "")
	if p.err != nil || !th.Resolved || len(th.Comments) != 2 || p.posted.Resolved != 1 || p.res.ReconcileOnly {
		t.Fatalf("repair: %+v, %v", p.posted, p.err)
	}
}

// The head moving mid-publish stops the run before its next mutation
// (§2).
func TestLifecycleFenceStopsMidPublish(t *testing.T) {
	w := newWorld(t)
	th, id := w.raise("P1")
	w.push("cache.go")
	w.fake.OnRequest = func(op string) int {
		if op == "POST reply" {
			w.fake.Push(7, strings.Repeat("9", 40), diff)
		}
		return 0
	}
	p := w.pass("", verdict(id, "fixed", "Initialized in New.", "cache.go"))
	if !errors.Is(p.err, github.ErrHeadMoved) || th.Resolved || len(th.Comments) != 2 || len(w.fake.PR(7).Reviews) != 1 {
		t.Fatalf("err %v, resolved %v, %d comments, %d reviews", p.err, th.Resolved, len(th.Comments), len(w.fake.PR(7).Reviews))
	}
}

// Without resolve permission, the state lives on in the markers and the
// review lists what to do by hand (§3).
func TestLifecycleResolveForbidden(t *testing.T) {
	w := newWorld(t)
	th, id := w.raise("P1")
	w.push("cache.go")
	w.fake.ForbidResolve = true
	p := w.pass("", verdict(id, "fixed", "Initialized in New.", "cache.go"))
	if p.err != nil || len(p.posted.Manual) != 1 || !p.posted.Manual[0].Resolve || th.Resolved {
		t.Fatalf("forbidden: %+v, %v", p.posted, p.err)
	}
	rv := w.fake.PR(7).Reviews[1].Body
	if !strings.Contains(rv, "please do it by hand:\n- resolve "+id) {
		t.Errorf("review body:\n%s", rv)
	}
}

// Verification cut short leaves the pass complete for the finder, but the
// run incomplete; the next run at that head checks the finding (§6).
func TestLifecycleUnverifiedCheckedNextRun(t *testing.T) {
	w := newWorld(t)
	th, id := w.raise("P1")
	w.push("cache.go")
	pref := review.PRRef{Repo: ref.Repo, Number: 7, HeadSHA: w.head()}
	silent := &testllm.FakeChatClient{Text: "I would rather chat."}
	res, err := review.Review(context.Background(), pref, w.ws, w.gh, &review.Options{Config: &w.cfg, Find: model("submit_findings", ""), Verify: silent})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Complete || res.Done() || len(res.Unverified) != 1 || !strings.HasPrefix(Status(res), "incomplete at 2222222: verification: the model stopped") {
		t.Fatalf("result: complete %v, unverified %v, status %s", res.Complete, res.Unverified, Status(res))
	}
	if _, perr := Publish(context.Background(), w.gh, pref, res); perr != nil {
		t.Fatal(perr)
	}
	m, err := state.Parse(w.fake.PR(7).Reviews[1].Body)
	if err != nil || m.Status != state.PassComplete {
		t.Fatalf("pass marker = %+v, %v", m, err)
	}

	p := w.pass("", verdict(id, "fixed", "Initialized in New.", "cache.go"))
	if p.err != nil || !p.res.ReconcileOnly || !th.Resolved || !p.res.Done() {
		t.Fatalf("recheck: %+v, %v", p.res, p.err)
	}
}

// A force-push back to a head that already had a complete pass still
// verifies the findings whose state was decided elsewhere since.
func TestLifecycleForcePushBackToReviewedHead(t *testing.T) {
	w := newWorld(t)
	th, id := w.raise("P1")
	w.push("cache.go")
	fixedAt := w.head()
	if p := w.pass("", verdict(id, "fixed", "Initialized in New.", "cache.go")); p.err != nil || !th.Resolved {
		t.Fatalf("fix: %v", p.err)
	}
	w.push("cache.go")
	if p := w.pass("", verdict(id, "open", "The initialization is gone again.")); p.err != nil || th.Resolved {
		t.Fatalf("regression: %v", p.err)
	}
	w.fake.Push(7, fixedAt, diff)
	w.heads = append(w.heads, fixedAt)
	p := w.pass("", verdict(id, "fixed", "Initialized in New.", "cache.go"))
	if p.err != nil || !p.res.ReconcileOnly || !th.Resolved || lastTransition(t, th).Kind != state.KindFixed {
		t.Fatalf("back at the fixed head: %+v, %v", p.res, p.err)
	}
}

// A reconcile-only run posts no pass review, so a resolution GitHub refuses
// there gets a review of its own listing what to do by hand (§3). A repair
// retried on a later run doesn't post it again.
func TestLifecycleReconcileOnlyReportsRefusal(t *testing.T) {
	w := newWorld(t)
	_, id := w.raise("P1")
	w.fake.ForbidResolve = true
	p := w.pass("", verdict(id, "fixed", "Put now makes the map.", "cache.go"))
	reviews := w.fake.PR(7).Reviews
	if p.err != nil || !p.res.ReconcileOnly || len(reviews) != 2 || !strings.Contains(reviews[1].Body, "please do it by hand:\n- resolve "+id) {
		t.Fatalf("refusal not reported: %v; %d reviews", p.err, len(reviews))
	}
	// The notice isn't a pass: the head still has its complete pass.
	p = w.pass("", "")
	if p.err != nil || !p.res.ReconcileOnly || len(p.posted.Manual) != 1 || len(w.fake.PR(7).Reviews) != 2 {
		t.Fatalf("retried repair: %+v, %v; %d reviews", p.posted, p.err, len(w.fake.PR(7).Reviews))
	}
}
