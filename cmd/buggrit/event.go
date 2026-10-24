package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/SnapdragonPartners/buggrit/internal/review"
)

// eventAuthor is who caused a review-comment or review event, read from
// the Actions event payload.
type eventAuthor struct {
	Login       string
	Association string
	Private     bool
}

// readEventAuthor reads the author of the event that started the run, or
// returns nil for events with no comment author (pushes, dispatches) and
// for local runs, which have no event file.
func readEventAuthor(name, path string) (*eventAuthor, error) {
	if path == "" || (name != "pull_request_review_comment" && name != "pull_request_review") {
		return nil, nil //nolint:nilnil // no author is not an error
	}
	data, err := os.ReadFile(path) //nolint:gosec // the runner's own event file
	if err != nil {
		return nil, fmt.Errorf("read the event payload: %w", err)
	}
	type author struct {
		User struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"user"`
		Association string `json:"author_association"`
	}
	var ev struct {
		Comment    *author `json:"comment"`
		Review     *author `json:"review"`
		Repository struct {
			Private bool `json:"private"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return nil, fmt.Errorf("read the event payload: %w", err)
	}
	a := ev.Comment
	if name == "pull_request_review" {
		a = ev.Review
	}
	if a == nil {
		return nil, fmt.Errorf("read the event payload: %s event has no author", name)
	}
	login := a.User.Login
	// Bots count as the team. The payload gives Copilot's login as just
	// "Copilot", so its type says it's a bot.
	if a.User.Type == "Bot" && !strings.HasSuffix(login, "[bot]") {
		login += "[bot]"
	}
	return &eventAuthor{Login: login, Association: a.Association, Private: ev.Repository.Private}, nil
}

// outsideEvent reports why a run started by a comment or review from
// outside the team has nothing to do, or "" if it does (design §1, "Public
// repositories"; §3, "Outside events"). The engine would ignore the comment
// anyway; stopping here saves the model the reconcile.
func (r *reviewer) outsideEvent(ctx context.Context) (string, error) {
	a, err := readEventAuthor(r.args.eventName, r.args.eventPath)
	if err != nil || a == nil || a.Private {
		return "", err
	}
	ok, err := review.NewTeam(r.gh, r.repo).Has(ctx, a.Login, a.Association)
	if err != nil || ok {
		return "", err //nolint:wrapcheck // Has names the author
	}
	return fmt.Sprintf("the run was started by %s, who has no write access to the repository", a.Login), nil
}
