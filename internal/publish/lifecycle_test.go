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

func TestLifecycleFalseResolve(t *testing.T) {
	w := newWorld(t)
	th, id := w.raise("P1")
	w.fake.SetResolved(7, th.ID, true)

	w.push("cache.go")
	p := w.pass("", verdict(id, "open", "entries is still nil in New."))
	if p.err != nil || th.Resolved || lastTransition(t, th).Kind != state.KindReopened || len(th.Comments) != 2 {
		t.Fatalf("false resolve: %+v, %v", p.posted, p.err)
	}

	// Resolved again without a word, at the same head: the head's pass is
	// complete, so this run only reconciles. It reopens without repeating
	// itself.
	w.fake.SetResolved(7, th.ID, true)
	p = w.pass("", verdict(id, "open", "Still nil."))
	if p.err != nil || !p.res.ReconcileOnly || th.Resolved || len(th.Comments) != 2 || p.posted.Unresolved != 1 {
		t.Fatalf("second false resolve: %+v, %v; %d comments", p.posted, p.err, len(th.Comments))
	}
	if len(w.fake.PR(7).Reviews) != 2 {
		t.Error("a reconcile-only run posted a review")
	}
}

func TestLifecycleRebuttals(t *testing.T) {
	w := newWorld(t)
	th, id := w.raise("P1")

	// Each reply triggers a reconcile-only run at the same head.
	w.fake.Reply(7, th.ID, "dan", "This is intentional.")
	p := w.pass("", verdict(id, "open", "Nothing documents that; Put panics."))
	if p.err != nil || lastTransition(t, th).Kind != state.KindRebutted || th.Resolved {
		t.Fatalf("first rebuttal: %+v, %v", p.posted, p.err)
	}
	// A run with nothing new checks the open finding again (§6) and says
	// nothing.
	before := len(th.Comments)
	if p = w.pass("", verdict(id, "open", "Same.")); p.err != nil || len(th.Comments) != before || !strings.Contains(Status(p.res), "1 unchanged") {
		t.Fatalf("idle run: %+v, %v; status %s", p.posted, p.err, Status(p.res))
	}

	w.fake.Reply(7, th.ID, "dan", "Still intentional.")
	w.fake.SetResolved(7, th.ID, true)
	p = w.pass("", verdict(id, "open", "Same answer."))
	m := lastTransition(t, th)
	if p.err != nil || m.Kind != state.KindNeedsHuman || th.Resolved {
		t.Fatalf("rebuttals used up: %+v, %v", m, p.err)
	}
	if want := th.Comments[len(th.Comments)-2].DatabaseID; m.Seen != want {
		t.Errorf("seen = %d, want the reply %d", m.Seen, want)
	}

	// Any later reply settles it, without a model.
	w.fake.Reply(7, th.ID, "erin", "Accepting the risk.")
	p = w.pass("", "")
	m = lastTransition(t, th)
	if p.err != nil || m.Kind != state.KindSettled || m.To != state.StateDismissed || !th.Resolved {
		t.Fatalf("settle: %+v, %v", m, p.err)
	}
	if !strings.Contains(th.Comments[len(th.Comments)-1].Body, "settled by erin's reply") {
		t.Errorf("settle reply = %q", th.Comments[len(th.Comments)-1].Body)
	}

	// Dismissed is terminal: later replies change nothing.
	w.fake.Reply(7, th.ID, "dan", "thanks")
	if p = w.pass("", ""); !p.res.Skipped {
		t.Errorf("after dismissal: %+v", p.res)
	}
}

func TestLifecycleNeedsHumanSettledByResolution(t *testing.T) {
	w := newWorld(t)
	w.cfg.Convergence.MaxRebuttals = 0
	th, id := w.raise("P0")
	w.fake.Reply(7, th.ID, "dan", "No.")
	if p := w.pass("", verdict(id, "open", "Still wrong.")); lastTransition(t, th).Kind != state.KindNeedsHuman {
		t.Fatalf("not sent to a human: %+v", p.posted)
	}
	w.fake.SetResolved(7, th.ID, true)
	p := w.pass("", "")
	if p.err != nil || lastTransition(t, th).Kind != state.KindSettled || !th.Resolved || p.posted.Resolved != 0 {
		t.Fatalf("settle by resolution: %+v, %v", p.posted, p.err)
	}
	if !strings.Contains(th.Comments[len(th.Comments)-1].Body, "settled by resolving the thread") {
		t.Errorf("reply = %q", th.Comments[len(th.Comments)-1].Body)
	}
}

func TestLifecycleValidRebuttalAndAdvisoryReply(t *testing.T) {
	w := newWorld(t)
	th, id := w.raise("P1")
	w.fake.Reply(7, th.ID, "dan", "See design §4: this map is filled by load().")
	p := w.pass("", verdict(id, "dismissed", "Right: load() runs first."))
	if p.err != nil || lastTransition(t, th).Kind != state.KindDismissed || !th.Resolved {
		t.Fatalf("valid rebuttal: %+v, %v", p.posted, p.err)
	}

	// An advisory deferral holds up (§7 rule 4).
	w2 := newWorld(t)
	th2, id2 := w2.raise("P2")
	w2.fake.Reply(7, th2.ID, "dan", "Deferred to #12.")
	p = w2.pass("", verdict(id2, "dismissed", "Deferred, fine."))
	if p.err != nil || lastTransition(t, th2).Kind != state.KindDismissed || !th2.Resolved {
		t.Fatalf("advisory deferral: %+v, %v", p.posted, p.err)
	}

	// A fix claim the code disproves is rebutted; once rebuttals are used
	// up, the author's word stands, and an advisory finding never goes to
	// a human (§7 rule 5).
	w3 := newWorld(t)
	th3, id3 := w3.raise("P2")
	w3.fake.Reply(7, th3.ID, "dan", "Fixed in 1111111.")
	if p = w3.pass("", verdict(id3, "open", "1111111 doesn't touch New.")); lastTransition(t, th3).Kind != state.KindRebutted {
		t.Fatalf("failed fix claim: %+v", p.posted)
	}
	w3.fake.Reply(7, th3.ID, "dan", "We'll leave it.")
	if p = w3.pass("", verdict(id3, "open", "Still there.")); lastTransition(t, th3).Kind != state.KindDismissed || !th3.Resolved {
		t.Fatalf("advisory after its rebuttal: %+v", p.posted)
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

// A needs_human transition on a resolved thread unresolves it first. If the
// run dies there, nothing was posted, and the old resolution can't pass for
// a human settling the finding (§6).
func TestLifecycleNeedsHumanUnresolvesFirst(t *testing.T) {
	w := newWorld(t)
	w.cfg.Convergence.MaxRebuttals = 0
	th, id := w.raise("P1")
	w.fake.Reply(7, th.ID, "dan", "Disagree.")
	w.fake.SetResolved(7, th.ID, true)
	w.fake.OnRequest = func(op string) int {
		if op == "graphql UnresolveThread" {
			return http.StatusBadGateway
		}
		return 0
	}
	if p := w.pass("", verdict(id, "open", "Still wrong.")); p.err == nil || len(th.Comments) != 2 {
		t.Fatalf("killed run: err %v, %d comments", p.err, len(th.Comments))
	}
	w.fake.OnRequest = nil
	p := w.pass("", verdict(id, "open", "Still wrong."))
	if p.err != nil || lastTransition(t, th).Kind != state.KindNeedsHuman || th.Resolved {
		t.Fatalf("rerun: %+v, %v", p.posted, p.err)
	}
	// Nobody has acted since, so nothing is settled.
	if p = w.pass("", verdict(id, "open", "Still wrong.")); lastTransition(t, th).Kind != state.KindNeedsHuman {
		t.Fatal("settled without a human")
	}
}

// When GitHub won't unresolve, the needs_human marker says the thread was
// left resolved, and only a reply settles it.
func TestLifecycleNeedsHumanLeftResolved(t *testing.T) {
	w := newWorld(t)
	w.cfg.Convergence.MaxRebuttals = 0
	th, id := w.raise("P1")
	w.fake.Reply(7, th.ID, "dan", "Disagree.")
	w.fake.SetResolved(7, th.ID, true)
	w.fake.ForbidResolve = true
	p := w.pass("", verdict(id, "open", "Still wrong."))
	m := lastTransition(t, th)
	if p.err != nil || m.Kind != state.KindNeedsHuman || !m.LeftResolved || len(p.posted.Manual) != 1 {
		t.Fatalf("forbidden: %+v, %+v, %v", m, p.posted, p.err)
	}
	if !strings.Contains(th.Comments[len(th.Comments)-1].Body, "The next reply, by anyone, settles") {
		t.Errorf("reply promises a resolution settles it: %q", th.Comments[len(th.Comments)-1].Body)
	}
	if p = w.pass("", verdict(id, "open", "Still wrong.")); lastTransition(t, th).Kind != state.KindNeedsHuman || len(p.posted.Manual) != 1 {
		t.Fatal("the old resolution settled the finding, or the unresolve wasn't retried")
	}
	// With permission back, the next run unresolves the thread.
	w.fake.ForbidResolve = false
	if p = w.pass("", verdict(id, "open", "Still wrong.")); th.Resolved || p.posted.Unresolved != 1 || lastTransition(t, th).Kind != state.KindNeedsHuman {
		t.Fatalf("unresolve not repaired: %+v", p.posted)
	}
	w.fake.Reply(7, th.ID, "erin", "Ship it.")
	if p = w.pass("", ""); lastTransition(t, th).Kind != state.KindSettled {
		t.Fatalf("a reply didn't settle it: %+v", p.posted)
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

// Someone resolving the thread while the verifier runs doesn't settle the
// finding it then sends to a human: the unresolve goes out regardless of
// the thread as first read.
func TestLifecycleNeedsHumanResolvedMidRun(t *testing.T) {
	w := newWorld(t)
	w.cfg.Convergence.MaxRebuttals = 0
	th, id := w.raise("P1")
	w.fake.Reply(7, th.ID, "dan", "Disagree.")
	verifier := &testllm.FakeChatClient{Func: func(context.Context, llms.ChatRequest) (llms.ChatResponse, error) {
		w.fake.SetResolved(7, th.ID, true) // the author resolves mid-run
		return testllm.ToolCallResponse("1", "submit_verdicts", verdict(id, "open", "Still wrong.")), nil
	}}
	pref := review.PRRef{Repo: ref.Repo, Number: 7, HeadSHA: w.head()}
	res, err := review.Review(context.Background(), pref, w.ws, w.gh, &review.Options{Config: &w.cfg, Find: model("submit_findings", ""), Verify: verifier})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Publish(context.Background(), w.gh, pref, res); err != nil {
		t.Fatal(err)
	}
	if th.Resolved || lastTransition(t, th).Kind != state.KindNeedsHuman {
		t.Fatalf("resolved %v", th.Resolved)
	}
	if p := w.pass("", verdict(id, "open", "Still wrong.")); lastTransition(t, th).Kind != state.KindNeedsHuman {
		t.Fatalf("settled without a human: %+v", p.posted)
	}
}

// A reply that lands while buggrit decides to escalate answered the earlier
// state, not the escalation, so it doesn't settle the finding.
func TestLifecycleReplyBeforeEscalationDoesNotSettle(t *testing.T) {
	w := newWorld(t)
	w.cfg.Convergence.MaxRebuttals = 0
	th, id := w.raise("P1")
	w.fake.Reply(7, th.ID, "dan", "Disagree.")
	verifier := &testllm.FakeChatClient{Func: func(context.Context, llms.ChatRequest) (llms.ChatResponse, error) {
		w.fake.Reply(7, th.ID, "dan", "And another thing.") // unseen by this run
		return testllm.ToolCallResponse("1", "submit_verdicts", verdict(id, "open", "Still wrong.")), nil
	}}
	pref := review.PRRef{Repo: ref.Repo, Number: 7, HeadSHA: w.head()}
	res, err := review.Review(context.Background(), pref, w.ws, w.gh, &review.Options{Config: &w.cfg, Find: model("submit_findings", ""), Verify: verifier})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Publish(context.Background(), w.gh, pref, res); err != nil {
		t.Fatal(err)
	}
	if lastTransition(t, th).Kind != state.KindNeedsHuman {
		t.Fatal("not escalated")
	}
	// The unseen reply is still unhandled, so the finding is checked again,
	// but nothing settles it.
	p := w.pass("", verdict(id, "open", "Still wrong."))
	if p.err != nil || lastTransition(t, th).Kind != state.KindNeedsHuman || len(p.res.Checks) != 1 {
		t.Fatalf("settled by a reply from before the escalation: %+v, %v", p.posted, p.err)
	}
	w.fake.Reply(7, th.ID, "erin", "Accepting it.")
	if p = w.pass("", ""); lastTransition(t, th).Kind != state.KindSettled {
		t.Fatalf("a later reply didn't settle it: %+v", p.posted)
	}
}
