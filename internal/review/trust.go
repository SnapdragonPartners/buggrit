package review

import (
	"context"
	"fmt"
	"strings"

	"github.com/SnapdragonPartners/buggrit/internal/github"
)

// trustedAssociation reports whether an author association puts a
// commenter on the team (design §1, "Public repositories").
func trustedAssociation(a string) bool {
	return a == "OWNER" || a == "MEMBER" || a == "COLLABORATOR"
}

// trustedPermission reports whether a repository permission does, for an
// author whose association doesn't. An org member whose membership is
// private can show as CONTRIBUTOR or NONE, but still has write access. On
// a public repository everyone has "read", so that doesn't count.
func trustedPermission(p string) bool { return p == "admin" || p == "write" }

// Ignored is a comment from someone outside the team, which the pass
// treated as if it weren't there (design §1).
type Ignored struct {
	Author string
	URL    string
}

// Team decides who is on the team (design §1, "Public repositories"),
// reading each author's permission at most once.
type Team struct {
	gh        PermissionReader
	permitted map[string]bool
	repo      github.Repo
}

// PermissionReader reads a user's permission on a repository.
type PermissionReader interface {
	Permission(ctx context.Context, repo github.Repo, login string) (string, error)
}

// NewTeam returns a Team for repo.
func NewTeam(gh PermissionReader, repo github.Repo) *Team {
	return &Team{gh: gh, repo: repo, permitted: map[string]bool{}}
}

// Has reports whether login, whose author association is association, is
// on the team. Bots are, since only an admin can install an App on the
// repository, and so is buggrit itself. Anyone else is if their
// association says so, or else if their permission on the repository is
// write or admin. A deleted ("ghost") author, with no login, isn't, and
// neither is one GitHub doesn't know (a 404). Any other error is returned,
// so the caller fails rather than drop a teammate's comment: a 403 is about
// the token, not the author, and a rate limit passes.
func (t *Team) Has(ctx context.Context, login, association string) (bool, error) {
	switch {
	case login == "":
		return false, nil
	case strings.HasSuffix(login, "[bot]"), trustedAssociation(association):
		return true, nil
	}
	key := strings.ToLower(login)
	if ok, seen := t.permitted[key]; seen {
		return ok, nil
	}
	perm, err := t.gh.Permission(ctx, t.repo, login)
	switch {
	case github.IsNotFound(err):
		perm = ""
	case err != nil:
		return false, fmt.Errorf("check whether %s is on the team: %w", login, err)
	}
	t.permitted[key] = trustedPermission(perm)
	return t.permitted[key], nil
}

// screen drops every comment from an author outside the team from threads,
// before anything reads them: replay and the finder's notes see only the
// team's comments, and the verifier sees only buggrit's own (design §6). A
// thread an outsider started is gone entirely, replies and all, since
// keeping the replies would make the first of them the thread's root. It
// returns the dropped comments.
//
// On a private repository everyone who can comment is on the team (§1), so
// nothing is dropped.
func screen(ctx context.Context, team *Team, private bool, threads []github.Thread) ([]github.Thread, []Ignored, error) {
	if private {
		return threads, nil, nil
	}
	var ignored []Ignored
	out := make([]github.Thread, 0, len(threads))
	for i := range threads {
		t := threads[i]
		kept := make([]github.Comment, 0, len(t.Comments))
		rootOK := true
		for j := range t.Comments {
			c := &t.Comments[j]
			ok, err := team.Has(ctx, c.Author, c.Association)
			if err != nil {
				return nil, nil, fmt.Errorf("review: %w", err)
			}
			if j == 0 {
				rootOK = ok
			}
			if !ok {
				ignored = append(ignored, Ignored{Author: c.Author, URL: c.URL})
			}
			if !ok || !rootOK {
				// A teammate's reply in an outsider's thread goes with the
				// thread; it isn't listed, since its author is on the team.
				continue
			}
			kept = append(kept, *c)
		}
		if len(kept) == 0 {
			continue
		}
		t.Comments = kept
		out = append(out, t)
	}
	return out, ignored, nil
}
