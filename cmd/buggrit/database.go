package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/SnapdragonPartners/buggrit/internal/config"
	"github.com/SnapdragonPartners/buggrit/internal/github"
	"github.com/SnapdragonPartners/buggrit/internal/state"
)

// databaseWarningID is the marker ID of the PR comment that says the
// configured database couldn't be used.
const databaseWarningID = "database"

// openTimeout bounds connecting to the database and checking its schemas.
// The Auth Proxy fetches an IAM token on the first connection.
const openTimeout = 30 * time.Second

// openDatabase opens the optional database (design §6, "The optional
// database"), used when BUGGRIT_DATABASE_URL is set. When it's configured
// but can't be used, the run goes on without it, and says why in one
// comment on the PR, edited in place, and in the job summary. That comment
// is removed once the database works again or is no longer configured. It
// returns nil when there's no database to use.
func (r *reviewer) openDatabase(ctx context.Context, cfg *config.Config, number int) io.Closer {
	var db io.Closer
	switch {
	case r.args.databaseError != "":
		r.dbProblem = r.args.databaseError
	case r.args.databaseURL != "":
		octx, cancel := context.WithTimeout(ctx, openTimeout)
		var err error
		db, err = r.deps.openStore(octx, r.args.databaseURL)
		cancel()
		if err != nil {
			db, r.dbProblem = nil, redactURL(err.Error(), r.args.databaseURL)
		}
	}
	if r.dbProblem != "" {
		say(r.stderr, "buggrit: warning: the database can't be used, so this run goes without it: %s\n", r.dbProblem)
	}
	if r.args.dryRun {
		return db
	}
	if err := r.syncDatabaseWarning(ctx, cfg.Identity.BotLogin, number); err != nil {
		say(r.stderr, "buggrit: warning: %v\n", err)
	}
	return db
}

// syncDatabaseWarning makes the PR's warning comment match r.dbProblem:
// one comment while there's a problem, none otherwise.
func (r *reviewer) syncDatabaseWarning(ctx context.Context, botLogin string, number int) error {
	comments, err := r.gh.IssueComments(ctx, r.repo, number)
	if err != nil {
		return wrapWarningErr(err)
	}
	var own []github.Comment
	for i := range comments {
		if m, ok := state.OwnMarker(&comments[i], botLogin); ok && m.ID == databaseWarningID {
			own = append(own, comments[i])
		}
	}
	errs := make([]error, 0, len(own))
	if r.dbProblem != "" {
		body, err := databaseWarning(r.dbProblem)
		if err != nil {
			return err
		}
		if len(own) == 0 {
			_, err = r.gh.CreateIssueComment(ctx, r.repo, number, body)
			return wrapWarningErr(err)
		}
		if own[0].Body != body {
			_, err = r.gh.UpdateIssueComment(ctx, r.repo, own[0].DatabaseID, body)
			errs = append(errs, err)
		}
		own = own[1:]
	}
	for i := range own {
		errs = append(errs, r.gh.DeleteIssueComment(ctx, r.repo, own[i].DatabaseID))
	}
	return wrapWarningErr(errors.Join(errs...))
}

// redactURL takes the database URL, and any password in it, out of a
// failure message before it's shown (design §10). pgx masks the password
// when it quotes a URL it can't parse, but only on a best-effort basis.
func redactURL(msg, rawURL string) string {
	if rawURL == "" {
		return msg
	}
	msg = strings.ReplaceAll(msg, rawURL, "$BUGGRIT_DATABASE_URL")
	// The password runs from the user's colon to the last @ before any query
	// or fragment, found by hand since a URL that fails to parse is the
	// case that matters.
	_, rest, ok := strings.Cut(rawURL, "://")
	if !ok {
		return msg
	}
	if i := strings.IndexAny(rest, "?#"); i >= 0 {
		rest = rest[:i]
	}
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		if _, pw, ok := strings.Cut(rest[:at], ":"); ok && pw != "" {
			msg = strings.ReplaceAll(msg, pw, "xxxxx")
		}
	}
	return msg
}

func wrapWarningErr(err error) error {
	if err != nil {
		return fmt.Errorf("can't update the database warning on the PR: %w", err)
	}
	return nil
}

// databaseWarning renders the PR comment, with the failure message as is.
func databaseWarning(problem string) (string, error) {
	body := "> [!WARNING]\n> buggrit couldn't use its database, so this run went without it. " +
		"Reviews don't depend on the database. This comment goes away once it works again.\n\n" +
		codeBlock(problem)
	return state.Append(body, &state.Marker{ID: databaseWarningID}) //nolint:wrapcheck // a fixed, valid marker
}

// databaseSummary is the job summary's note of the same problem.
func databaseSummary(problem string) string {
	return "\n### Database\n\nThe database couldn't be used, so this run went without it:\n\n" + codeBlock(problem)
}

// codeBlock fences text, with a fence longer than any backtick run in it.
func codeBlock(text string) string {
	fence := "```"
	for strings.Contains(text, fence) {
		fence += "`"
	}
	return fence + "\n" + strings.TrimRight(text, "\n") + "\n" + fence + "\n"
}
