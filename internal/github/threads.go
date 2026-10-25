package github

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Comment is a review-thread or conversation comment. Author is the REST-form
// login: bots end in "[bot]" (for example "buggrit-bot[bot]").
type Comment struct {
	CreatedAt time.Time
	// ID is the GraphQL node ID.
	ID     string
	Author string
	// Association is the author's relation to the repository, as GraphQL's
	// authorAssociation reports it: OWNER, MEMBER, COLLABORATOR,
	// CONTRIBUTOR, FIRST_TIME_CONTRIBUTOR, FIRST_TIMER, MANNEQUIN or NONE.
	// Empty for comments read over REST.
	Association string
	Body        string
	URL         string
	// CommitSHA is the commit a review comment was made on (empty for
	// conversation comments and for replies read over REST).
	CommitSHA string
	// DiffHunk is the diff excerpt a review comment was made on, read from
	// GraphQL. For a thread's root it is the original snippet, which the
	// verifier needs once the anchor is outdated (design §6).
	DiffHunk string
	// DatabaseID is the REST ID, used to reply and to edit.
	DatabaseID int64
}

// Thread is a review thread with all its comments, oldest first. It is the
// unit buggrit replays to get a finding's state (design §6).
type Thread struct {
	// ID is the GraphQL node ID, used to resolve and unresolve.
	ID       string
	Path     string
	Side     Side
	Comments []Comment
	// Line is the thread's current line, 0 when outdated or file-level.
	Line int
	// OriginalLine is the line on the commit the thread was started on.
	OriginalLine int
	StartLine    int
	IsResolved   bool
	IsOutdated   bool
	FileLevel    bool
	// ResolvedBy is the REST-form login of whoever resolved the thread, or
	// "" if it's unresolved or GitHub doesn't say (a deleted account).
	// buggrit settles a finding by who resolved its thread (design §6).
	ResolvedBy string
}

// Root is the thread's first comment, or nil for an empty thread.
func (t *Thread) Root() *Comment {
	if len(t.Comments) == 0 {
		return nil
	}
	return &t.Comments[0]
}

const threadsQuery = `query ReviewThreads($owner: String!, $name: String!, $number: Int!, $after: String) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviewThreads(first: 50, after: $after) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id
          isResolved
          resolvedBy { __typename login }
          isOutdated
          path
          line
          originalLine
          startLine
          diffSide
          subjectType
          comments(first: 100) {
            pageInfo { hasNextPage endCursor }
            nodes { ...threadComment }
          }
        }
      }
    }
  }
}
` + commentFragment

const threadCommentsQuery = `query ThreadComments($id: ID!, $after: String) {
  node(id: $id) {
    ... on PullRequestReviewThread {
      comments(first: 100, after: $after) {
        pageInfo { hasNextPage endCursor }
        nodes { ...threadComment }
      }
    }
  }
}
` + commentFragment

const commentFragment = `fragment threadComment on PullRequestReviewComment {
  id
  databaseId
  body
  url
  diffHunk
  createdAt
  authorAssociation
  author { __typename login }
  commit { oid }
}
`

type gqlPageInfo struct {
	EndCursor   string `json:"endCursor"`
	HasNextPage bool   `json:"hasNextPage"`
}

// gqlActor is a comment's author or a thread's resolver.
type gqlActor struct {
	Typename string `json:"__typename"`
	Login    string `json:"login"`
}

// login is the actor's login in REST form. GraphQL gives a bot's login
// without the "[bot]" suffix REST uses, which is what config's
// identity.bot_login holds. A nil actor is a deleted ("ghost") user.
func (a *gqlActor) login() string {
	if a == nil {
		return ""
	}
	if a.Typename == "Bot" && !strings.HasSuffix(a.Login, "[bot]") {
		return a.Login + "[bot]"
	}
	return a.Login
}

type gqlComment struct {
	CreatedAt time.Time `json:"createdAt"`
	Author    *gqlActor `json:"author"`
	Commit    *struct {
		OID string `json:"oid"`
	} `json:"commit"`
	ID          string `json:"id"`
	Association string `json:"authorAssociation"`
	Body        string `json:"body"`
	URL         string `json:"url"`
	DiffHunk    string `json:"diffHunk"`
	DatabaseID  int64  `json:"databaseId"`
}

type gqlComments struct {
	Nodes    []gqlComment `json:"nodes"`
	PageInfo gqlPageInfo  `json:"pageInfo"`
}

type gqlThread struct {
	ResolvedBy   *gqlActor   `json:"resolvedBy"`
	Line         *int        `json:"line"`
	OriginalLine *int        `json:"originalLine"`
	StartLine    *int        `json:"startLine"`
	ID           string      `json:"id"`
	Path         string      `json:"path"`
	DiffSide     Side        `json:"diffSide"`
	SubjectType  string      `json:"subjectType"`
	Comments     gqlComments `json:"comments"`
	IsResolved   bool        `json:"isResolved"`
	IsOutdated   bool        `json:"isOutdated"`
}

func (g *gqlComment) comment() Comment {
	c := Comment{
		ID:          g.ID,
		DatabaseID:  g.DatabaseID,
		Association: g.Association,
		Body:        g.Body,
		URL:         g.URL,
		DiffHunk:    g.DiffHunk,
		CreatedAt:   g.CreatedAt,
		Author:      g.Author.login(),
	}
	if g.Commit != nil {
		c.CommitSHA = g.Commit.OID
	}
	return c
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// ReviewThreads reads every review thread on a PR with all of its comments,
// including other reviewers' threads (design §6).
func (c *Client) ReviewThreads(ctx context.Context, repo Repo, number int) ([]Thread, error) {
	var out []Thread
	var after *string
	for {
		var data struct {
			Repository *struct {
				PullRequest *struct {
					ReviewThreads struct {
						Nodes    []gqlThread `json:"nodes"`
						PageInfo gqlPageInfo `json:"pageInfo"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		vars := map[string]any{"owner": repo.Owner, "name": repo.Name, "number": number, "after": after}
		if err := c.graphQL(ctx, "ReviewThreads", threadsQuery, vars, &data); err != nil {
			return nil, fmt.Errorf("read review threads on %s#%d: %w", repo, number, err)
		}
		if data.Repository == nil || data.Repository.PullRequest == nil {
			return nil, fmt.Errorf("read review threads on %s#%d: %w", repo, number,
				&APIError{Type: "NOT_FOUND", Message: "pull request not found"})
		}
		page := data.Repository.PullRequest.ReviewThreads
		for i := range page.Nodes {
			t, err := c.thread(ctx, &page.Nodes[i])
			if err != nil {
				return nil, fmt.Errorf("read review threads on %s#%d: %w", repo, number, err)
			}
			out = append(out, t)
		}
		if !page.PageInfo.HasNextPage {
			return out, nil
		}
		cursor := page.PageInfo.EndCursor
		after = &cursor
	}
}

// thread converts a GraphQL thread, fetching any comments past the first page.
func (c *Client) thread(ctx context.Context, g *gqlThread) (Thread, error) {
	t := Thread{
		ID:           g.ID,
		Path:         g.Path,
		Side:         g.DiffSide,
		Line:         deref(g.Line),
		OriginalLine: deref(g.OriginalLine),
		StartLine:    deref(g.StartLine),
		IsResolved:   g.IsResolved,
		IsOutdated:   g.IsOutdated,
		FileLevel:    g.SubjectType == "FILE",
	}
	if g.IsResolved {
		t.ResolvedBy = g.ResolvedBy.login()
	}
	comments := g.Comments
	for {
		for i := range comments.Nodes {
			t.Comments = append(t.Comments, comments.Nodes[i].comment())
		}
		if !comments.PageInfo.HasNextPage {
			return t, nil
		}
		var data struct {
			Node *struct {
				Comments gqlComments `json:"comments"`
			} `json:"node"`
		}
		vars := map[string]any{"id": g.ID, "after": comments.PageInfo.EndCursor}
		if err := c.graphQL(ctx, "ThreadComments", threadCommentsQuery, vars, &data); err != nil {
			return Thread{}, fmt.Errorf("thread %s comments: %w", g.ID, err)
		}
		if data.Node == nil {
			return Thread{}, fmt.Errorf("thread %s comments: %w", g.ID,
				&APIError{Type: "NOT_FOUND", Message: "thread not found"})
		}
		comments = data.Node.Comments
	}
}

const threadResolutionQuery = `query ThreadResolution($id: ID!) {
  node(id: $id) { ... on PullRequestReviewThread { isResolved resolvedBy { __typename login } } }
}`

// Resolution is whether a review thread is resolved now, and by whom: the
// REST-form login, or "" if it's unresolved or GitHub doesn't say.
type Resolution struct {
	By       string
	Resolved bool
}

// ThreadResolution reads one review thread's resolution now. Publishing
// reads it before changing a thread, so it never undoes a resolution made
// since the threads were read, and after GitHub refuses a change, to learn
// whether the change was needed at all.
func (c *Client) ThreadResolution(ctx context.Context, threadID string) (Resolution, error) {
	var data struct {
		Node *struct {
			ResolvedBy *gqlActor `json:"resolvedBy"`
			IsResolved bool      `json:"isResolved"`
		} `json:"node"`
	}
	if err := c.graphQL(ctx, "ThreadResolution", threadResolutionQuery, map[string]any{"id": threadID}, &data); err != nil {
		return Resolution{}, fmt.Errorf("read thread %s: %w", threadID, err)
	}
	if data.Node == nil {
		return Resolution{}, fmt.Errorf("read thread %s: %w", threadID, &APIError{Type: "NOT_FOUND", Message: "thread not found"})
	}
	r := Resolution{Resolved: data.Node.IsResolved}
	if r.Resolved {
		r.By = data.Node.ResolvedBy.login()
	}
	return r, nil
}

const resolveMutation = `mutation ResolveThread($id: ID!) {
  resolveReviewThread(input: {threadId: $id}) { thread { id isResolved } }
}`

const unresolveMutation = `mutation UnresolveThread($id: ID!) {
  unresolveReviewThread(input: {threadId: $id}) { thread { id isResolved } }
}`

// ResolveThread resolves a review thread. An integration token may be refused
// (check IsForbidden); callers then list the thread as "resolve manually"
// and carry on (design §3).
func (c *Client) ResolveThread(ctx context.Context, threadID string) error {
	if err := c.graphQL(ctx, "ResolveThread", resolveMutation, map[string]any{"id": threadID}, nil); err != nil {
		return fmt.Errorf("resolve thread %s: %w", threadID, err)
	}
	return nil
}

// UnresolveThread reopens a review thread.
func (c *Client) UnresolveThread(ctx context.Context, threadID string) error {
	if err := c.graphQL(ctx, "UnresolveThread", unresolveMutation, map[string]any{"id": threadID}, nil); err != nil {
		return fmt.Errorf("unresolve thread %s: %w", threadID, err)
	}
	return nil
}
