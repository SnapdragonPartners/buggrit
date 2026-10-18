package review

import (
	"context"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/buggrit/internal/github"
	"github.com/SnapdragonPartners/buggrit/internal/github/githubtest"
	"github.com/SnapdragonPartners/buggrit/internal/state"
)

func fixedAt(t *testing.T, id, sha string, evidence ...string) state.Tracked {
	t.Helper()
	tr := tracked(t, id, "P1", state.StateFixed)
	tr.FixSHA, tr.Evidence = sha, evidence
	return *tr
}

func TestSelectChecks(t *testing.T) {
	fake, gh, ref := engineSetup(t)
	const (
		ancestor  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		rewritten = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		gone      = "cccccccccccccccccccccccccccccccccccccccc"
		huge      = "dddddddddddddddddddddddddddddddddddddddd"
	)
	fake.SetCompare(ancestor, engineHead, githubtest.Comparison{Status: github.CompareAhead,
		Diff: "diff --git a/other.go b/other.go\n--- a/other.go\n+++ b/other.go\n@@ -1 +1 @@\n-x\n+y\n"})
	fake.SetCompare(rewritten, engineHead, githubtest.Comparison{Status: github.CompareDiverged})
	fake.SetCompare(huge, engineHead, githubtest.Comparison{Status: github.CompareAhead, DiffTooLarge: true})

	resolved := tracked(t, "f-resolv", "P1", state.StateOpen)
	resolved.Thread.IsResolved = true
	waiting := tracked(t, "f-waitin", "P1", state.StateNeedsHuman)
	settled := tracked(t, "f-settle", "P1", state.StateNeedsHuman, "fine, ship it")
	replied := fixedAt(t, "f-replid", engineHead, "internal/cache/cache.go")
	replied.Pending = []github.Comment{{DatabaseID: 99, Author: "dan", Body: "not really fixed"}}
	all := []state.Tracked{
		*tracked(t, "f-open00", "P1", state.StateOpen),
		*resolved,
		*tracked(t, "f-replyo", "P2", state.StateOpen, "deferred"),
		*waiting,
		*settled,
		*tracked(t, "f-dismis", "P1", state.StateDismissed, "late reply"),
		fixedAt(t, "f-athead", engineHead, "internal/cache/cache.go"),
		fixedAt(t, "f-untouc", ancestor, "internal/cache/cache.go"),
		fixedAt(t, "f-touchd", ancestor, "internal/cache/cache.go", "other.go"),
		fixedAt(t, "f-rewrit", rewritten, "internal/cache/cache.go"),
		fixedAt(t, "f-goneee", gone, "internal/cache/cache.go"),
		fixedAt(t, "f-hugeee", huge, "internal/cache/cache.go"),
		replied,
	}
	why := map[string]string{}
	for _, c := range selectChecks(context.Background(), gh, ref, all, false, nil) {
		why[c.Tracked.ID] = c.Why
	}
	want := map[string]string{
		"f-open00": "it is open",
		"f-resolv": "its thread was resolved",
		"f-replyo": "new reply",
		"f-waitin": "waiting for a human",
		"f-touchd": "have changed since: other.go",
		"f-rewrit": "no longer in the branch's history",
		"f-goneee": "gone from the repository",
		"f-hugeee": "could not be read",
		"f-replid": "has a new reply",
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

	// At a head that already has a complete pass, untouched open and
	// needs_human findings are left alone unless that pass didn't reach
	// them.
	why = map[string]string{}
	for _, c := range selectChecks(context.Background(), gh, ref, all, true, []string{"f-waitin"}) {
		why[c.Tracked.ID] = c.Why
	}
	if _, ok := why["f-open00"]; ok {
		t.Error("an untouched open finding was rechecked at a reviewed head")
	}
	for _, id := range []string{"f-resolv", "f-replyo", "f-waitin", "f-touchd", "f-replid"} {
		if why[id] == "" {
			t.Errorf("%s not selected in a reconcile-only run", id)
		}
	}
}

func TestTransitionsSkipsNoOps(t *testing.T) {
	open := tracked(t, "f-open00", "P1", state.StateOpen)
	fixedUnresolved := fixedAt(t, "f-fixedu", engineHead, "internal/cache/cache.go")
	fixedResolved := fixedAt(t, "f-fixedr", engineHead, "internal/cache/cache.go")
	fixedResolved.Thread.IsResolved = true
	all := []state.Tracked{*open, fixedUnresolved, fixedResolved}
	got := transitions(all, []Check{{Tracked: &all[0]}}, map[string]*state.Verdict{"f-open00": {State: state.StateOpen, Reason: "r"}}, 1)
	if len(got) != 1 || got[0].Tracked.ID != "f-fixedu" || got[0].Kind != "" || !got[0].Repair() {
		t.Fatalf("transitions = %+v", got)
	}
}
