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

// screen drops every comment from an author outside the team from threads,
// before anything reads them: replay, the verifier and the finder's notes
// see only the team's comments, and a thread an outsider started is gone
// entirely. It returns the dropped comments.
//
// On a private repository everyone who can comment is on the team (§1), so
// nothing is dropped.
//
// Bots count as the team, since only an admin can install an App on the
// repository, and so does buggrit itself. Anyone else is on the team if
// their association says so, or else if their permission on the repository
// is write or admin. A deleted ("ghost") author isn't. When GitHub won't
// say (a 404 or 403), the author isn't on the team; any other error fails
// the pass rather than drop a teammate's reply.
func screen(ctx context.Context, gh GitHub, repo github.Repo, private bool, threads []github.Thread) ([]github.Thread, []Ignored, error) {
	if private {
		return threads, nil, nil
	}
	permitted := map[string]bool{}
	trusted := func(c *github.Comment) (bool, error) {
		switch {
		case c.Author == "":
			return false, nil
		case strings.HasSuffix(c.Author, "[bot]"), trustedAssociation(c.Association):
			return true, nil
		}
		login := strings.ToLower(c.Author)
		if ok, seen := permitted[login]; seen {
			return ok, nil
		}
		perm, err := gh.Permission(ctx, repo, c.Author)
		switch {
		case github.IsNotFound(err), github.IsForbidden(err):
			perm = ""
		case err != nil:
			return false, fmt.Errorf("review: %w", err)
		}
		permitted[login] = trustedPermission(perm)
		return permitted[login], nil
	}
	var ignored []Ignored
	out := make([]github.Thread, 0, len(threads))
	for i := range threads {
		t := threads[i]
		kept := make([]github.Comment, 0, len(t.Comments))
		for j := range t.Comments {
			c := &t.Comments[j]
			ok, err := trusted(c)
			if err != nil {
				return nil, nil, err
			}
			if !ok {
				ignored = append(ignored, Ignored{Author: c.Author, URL: c.URL})
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
