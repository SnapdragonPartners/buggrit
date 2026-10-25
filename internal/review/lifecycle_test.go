package review

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SnapdragonPartners/buggrit/internal/github"
	"github.com/SnapdragonPartners/buggrit/internal/github/githubtest"
	"github.com/SnapdragonPartners/buggrit/internal/state"
)

func fixedAt(t *testing.T, id, sha string, evidence ...string) state.Tracked {
	t.Helper()
	tr := tracked(t, id, "P1", state.StateFixed)
	tr.Fix = &state.Record{At: time.Date(2026, 1, 1, 0, 0, 10, 0, time.UTC), SHA: sha, Files: evidence}
	return *tr
}

func TestSelectChecks(t *testing.T) {
	fake, gh, ref := engineSetup(t)
	const (
		ancestor  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		rewritten = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		gone      = "cccccccccccccccccccccccccccccccccccccccc"
		huge      = "dddddddddddddddddddddddddddddddddddddddd"
		later     = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	)
	other := "diff --git a/other.go b/other.go\n--- a/other.go\n+++ b/other.go\n@@ -1 +1 @@\n-x\n+y\n"
	fake.SetCompare(ancestor, engineHead, githubtest.Comparison{Status: github.CompareAhead, Diff: other})
	fake.SetCompare(rewritten, engineHead, githubtest.Comparison{Status: github.CompareDiverged})
	fake.SetCompare(huge, engineHead, githubtest.Comparison{Status: github.CompareAhead, DiffTooLarge: true})
	fake.SetCompare(later, engineHead, githubtest.Comparison{Status: github.CompareAhead,
		Diff: "diff --git a/third.go b/third.go\n--- a/third.go\n+++ b/third.go\n@@ -1 +1 @@\n-x\n+y\n"})

	settledOpen := tracked(t, "f-settlo", "P1", state.StateOpen)
	settledOpen.Settled = true
	unknown := tracked(t, "f-unknwn", "P1", state.StateOpen)
	unknown.Thread.IsResolved, unknown.ResolverUnknown = true, true
	settledFixed := fixedAt(t, "f-settlf", rewritten, "internal/cache/cache.go")
	settledFixed.Settled = true
	all := []state.Tracked{
		*tracked(t, "f-open00", "P1", state.StateOpen, "a reply changes nothing"),
		*settledOpen,
		*unknown,
		*tracked(t, "f-dismis", "P1", state.StateDismissed),
		settledFixed,
		fixedAt(t, "f-athead", engineHead, "internal/cache/cache.go"),
		fixedAt(t, "f-untouc", ancestor, "internal/cache/cache.go"),
		fixedAt(t, "f-touchd", ancestor, "internal/cache/cache.go", "other.go"),
		fixedAt(t, "f-rewrit", rewritten, "internal/cache/cache.go"),
		fixedAt(t, "f-goneee", gone, "internal/cache/cache.go"),
		fixedAt(t, "f-hugeee", huge, "internal/cache/cache.go"),
		// Re-verified since its fix: the newest record in the head's
		// history is the baseline, with that record's files.
		fixedAt(t, "f-reverd", ancestor, "internal/cache/cache.go", "other.go"),
		fixedAt(t, "f-rever2", ancestor, "internal/cache/cache.go"),
		// Re-verified at a commit a force-push dropped: the fix is the
		// baseline again.
		fixedAt(t, "f-reverg", ancestor, "internal/cache/cache.go", "other.go"),
	}
	b := baselineWith(t, map[string][]string{"f-reverd": {"internal/cache/cache.go"}, "f-rever2": {"third.go"}}, later)
	gb := baselineWith(t, map[string][]string{"f-reverg": {"internal/cache/cache.go"}}, gone)
	why := map[string]string{}
	for _, c := range selectChecks(context.Background(), gh, ref, all[:len(all)-1], &b) {
		why[c.Tracked.ID] = c.Why
	}
	for _, c := range selectChecks(context.Background(), gh, ref, all[len(all)-1:], &gb) {
		why[c.Tracked.ID] = c.Why
	}
	want := map[string]string{
		"f-open00": "it is open",
		"f-unknwn": "it is open",
		"f-touchd": "have changed since: other.go",
		"f-rewrit": "no commit it was verified at is still in the branch's history",
		"f-goneee": "no commit it was verified at is still in the branch's history",
		"f-hugeee": "could not be read",
		"f-rever2": "last verified fixed at eeeeeee, and the files that verdict relied on have changed since: third.go",
		"f-reverg": "last verified fixed at aaaaaaa, and the files that verdict relied on have changed since: other.go",
	}
	for id, w := range want {
		if !strings.Contains(why[id], w) {
			t.Errorf("%s: why = %q, want it to mention %q", id, why[id], w)
		}
	}
	for id := range why {
		if _, ok := want[id]; !ok {
			t.Errorf("%s was selected: %s", id, why[id])
		}
	}
}

// baselineWith is a baseline whose one pass, after every fix in these
// tests, re-verified the given findings at sha.
func baselineWith(t *testing.T, verified map[string][]string, sha string) state.Baseline {
	t.Helper()
	body, err := state.Append("pass", &state.Marker{ID: "pass-2", Pass: 2, SHA: sha, Status: state.PassComplete, Verified: verified})
	if err != nil {
		t.Fatal(err)
	}
	return state.ReadBaseline([]github.ReviewSummary{{
		Author: githubtest.DefaultLogin, Body: body, SubmittedAt: time.Date(2026, 1, 1, 0, 0, 20, 0, time.UTC),
	}}, githubtest.DefaultLogin)
}

func TestTransitionsSkipsNoOps(t *testing.T) {
	open := tracked(t, "f-open00", "P1", state.StateOpen)
	fixedUnresolved := fixedAt(t, "f-fixedu", engineHead, "internal/cache/cache.go")
	fixedResolved := fixedAt(t, "f-fixedr", engineHead, "internal/cache/cache.go")
	fixedResolved.Thread.IsResolved = true
	all := []state.Tracked{*open, fixedUnresolved, fixedResolved}
	got := transitions(all, []Check{{Tracked: &all[0]}}, map[string]*state.Verdict{"f-open00": {State: state.StateOpen, Reason: "r"}})
	if len(got) != 1 || got[0].Tracked.ID != "f-fixedu" || got[0].Kind != "" || !got[0].Repair() {
		t.Fatalf("transitions = %+v", got)
	}
}

// Only a fixed finding found still fixed is recorded as re-verified.
func TestReverified(t *testing.T) {
	open := tracked(t, "f-open00", "P1", state.StateOpen)
	fixed := fixedAt(t, "f-fixed0", engineHead, "a.go")
	regressed := fixedAt(t, "f-regres", engineHead, "a.go")
	checks := []Check{{Tracked: open}, {Tracked: &fixed}, {Tracked: &regressed}}
	got := reverified(checks, map[string]*state.Verdict{
		"f-open00": {State: state.StateFixed, Evidence: []string{"a.go"}},
		"f-fixed0": {State: state.StateFixed, Evidence: []string{"a.go", "b.go"}},
		"f-regres": {State: state.StateOpen},
	})
	if len(got) != 1 || len(got["f-fixed0"]) != 2 {
		t.Errorf("reverified = %v", got)
	}
	if reverified(checks[:1], nil) != nil {
		t.Error("nothing re-verified, but not nil")
	}
}
