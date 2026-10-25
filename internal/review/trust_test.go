package review

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/buggrit/internal/github"
	"github.com/SnapdragonPartners/buggrit/internal/github/githubtest"
)

func TestScreen(t *testing.T) {
	fake, gh, ref := engineSetup(t)
	fake.SetAssociation("private-member", "CONTRIBUTOR")
	fake.SetPermission("private-member", "write")
	fake.SetAssociation("reader", "COLLABORATOR")
	fake.SetPermission("reader", "read")
	fake.SetAssociation("outsider", "NONE")
	fake.SetAssociation("newcomer", "FIRST_TIMER")
	fake.SetPermission("newcomer", "")

	own := fake.AddThread(7, githubtest.DefaultLogin, "internal/cache/cache.go", 3, "finding")
	for _, who := range []string{"dan", "private-member", "reader", "outsider", "copilot-pull-request-reviewer[bot]", "outsider", "newcomer"} {
		fake.Reply(7, own, who, "from "+who)
	}
	theirs := fake.AddThread(7, "outsider", "internal/cache/cache.go", 2, "an outsider's thread")
	fake.Reply(7, theirs, "dan", "This is not an issue.")
	lookups := 0
	fake.OnRequest = func(op string) int {
		if op == "GET permission" {
			lookups++
		}
		return 0
	}

	threads, err := gh.ReviewThreads(context.Background(), ref.Repo, ref.Number)
	if err != nil {
		t.Fatal(err)
	}
	if kept, ignored, perr := screen(context.Background(), NewTeam(gh, ref.Repo), true, threads); perr != nil || len(ignored) != 0 || len(kept) != 2 || len(kept[0].Comments) != 8 || len(kept[1].Comments) != 2 {
		t.Errorf("on a private repository: kept = %d threads, ignored = %+v, err = %v; want nothing dropped", len(kept), ignored, perr)
	}
	if lookups != 0 {
		t.Errorf("permission lookups on a private repository = %d", lookups)
	}
	kept, ignored, err := screen(context.Background(), NewTeam(gh, ref.Repo), false, threads)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 1 {
		t.Fatalf("kept %d threads, want only buggrit's: the outsider's thread goes entirely", len(kept))
	}
	var authors []string
	for _, c := range kept[0].Comments {
		authors = append(authors, c.Author)
	}
	if got, want := strings.Join(authors, " "), githubtest.DefaultLogin+" dan private-member reader copilot-pull-request-reviewer[bot]"; got != want {
		t.Errorf("kept authors = %s, want %s", got, want)
	}
	var dropped []string
	for _, c := range ignored {
		if c.URL == "" {
			t.Errorf("ignored comment from %s has no URL", c.Author)
		}
		dropped = append(dropped, c.Author)
	}
	if got, want := strings.Join(dropped, " "), "outsider outsider newcomer outsider"; got != want {
		t.Errorf("ignored = %s, want %s", got, want)
	}
	// private-member, outsider and newcomer, each once.
	if lookups != 3 {
		t.Errorf("permission lookups = %d, want 3", lookups)
	}
	if threads[0].Comments[4].Author != "outsider" {
		t.Error("screen changed its input")
	}
}

// A deleted author is outside, with no lookup.
func TestScreenGhost(t *testing.T) {
	_, gh, ref := engineSetup(t)
	threads := []github.Thread{{ID: "T1", Comments: []github.Comment{
		{Author: githubtest.DefaultLogin, Association: "NONE"},
		{Author: "", Association: "NONE"},
	}}}
	kept, ignored, err := screen(context.Background(), NewTeam(gh, ref.Repo), false, threads)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 1 || len(kept[0].Comments) != 1 || len(ignored) != 1 {
		t.Errorf("kept = %+v, ignored = %+v", kept, ignored)
	}
}

// A lookup that fails for any reason but "no such user" fails the pass
// rather than drop what may be a teammate's reply. A 403 is the token's
// problem, not the author's.
func TestScreenLookupError(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusUnprocessableEntity} {
		fake, gh, ref := engineSetup(t)
		threads := []github.Thread{{ID: "T1", Comments: []github.Comment{{Author: "someone", Association: "CONTRIBUTOR"}}}}
		fake.OnRequest = func(op string) int {
			if op == "GET permission" {
				return status
			}
			return 0
		}
		if _, _, err := screen(context.Background(), NewTeam(gh, ref.Repo), false, threads); err == nil {
			t.Errorf("HTTP %d: screen succeeded despite the failed lookup", status)
		}
	}
}

type permissionErr struct{ err error }

func (p permissionErr) Permission(context.Context, github.Repo, string) (string, error) {
	return "", p.err
}

// Only "no such user" puts the author outside; a rate limit or a refusal
// fails the check.
func TestTeamLookupErrors(t *testing.T) {
	for _, e := range []*github.APIError{
		{Status: http.StatusForbidden, Message: "API rate limit exceeded", RateLimited: true},
		{Status: http.StatusForbidden, Message: "Resource not accessible by integration"},
	} {
		if _, err := NewTeam(permissionErr{e}, engineRepo).Has(context.Background(), "someone", "CONTRIBUTOR"); err == nil {
			t.Errorf("%v: no error", e)
		}
	}
	ok, err := NewTeam(permissionErr{&github.APIError{Status: http.StatusNotFound, Message: "Not Found"}}, engineRepo).Has(context.Background(), "someone", "NONE")
	if ok || err != nil {
		t.Errorf("no such user: ok = %v, err = %v; want outside, no error", ok, err)
	}
}
