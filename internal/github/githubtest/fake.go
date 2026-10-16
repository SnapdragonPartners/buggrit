// Package githubtest is an in-memory fake of the GitHub REST and GraphQL
// endpoints buggrit uses, served over httptest (docs/design.md §11). Tests
// drive the real github.Client against it, and script human actions (replies,
// resolutions, pushes) between passes.
//
// GraphQL requests are dispatched on operationName; query text isn't parsed.
// The live check covers the real queries.
package githubtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SnapdragonPartners/buggrit/internal/github"
)

// DefaultToken authenticates as DefaultLogin.
const (
	DefaultToken = "buggrit-token"
	DefaultLogin = "buggrit[bot]"
)

// Comment is a stored review comment.
type Comment struct {
	CreatedAt  time.Time
	Author     string
	Body       string
	CommitSHA  string
	NodeID     string
	DatabaseID int64
}

// Thread is a stored review thread.
type Thread struct {
	ID           string
	Path         string
	Side         github.Side
	Comments     []*Comment
	Line         int
	OriginalLine int
	StartLine    int
	Resolved     bool
	Outdated     bool
	FileLevel    bool
}

// PR is a stored pull request.
type PR struct {
	Diff          string
	HeadSHA       string
	BaseSHA       string
	Threads       []*Thread
	IssueComments []*Comment
	// Diffs holds the PR's diff at every head it has had. New comments may
	// target any of them, as on GitHub.
	Diffs   map[string]string
	Reviews int
	Number  int
}

// Server is the fake. Set its exported fields only while no request is in
// flight; they aren't guarded by its lock.
type Server struct {
	srv *httptest.Server
	// OnRequest, if set, runs before each request is handled, with the
	// operation name ("GET pull", "POST review", "graphql ResolveThread", …).
	// Tests use it to move the head mid-publish or to fail a call.
	OnRequest func(op string) (status int)
	tokens    map[string]string
	prs       map[int]*PR
	owner     string
	repo      string
	mu        sync.Mutex
	nextID    int64
	// ThreadsPageSize and CommentsPageSize force GraphQL pagination.
	ThreadsPageSize  int
	CommentsPageSize int
	// IssuePageSize forces REST pagination of issue comments.
	IssuePageSize int
	// ForbidResolve makes resolve and unresolve fail the way GitHub does for
	// some integration tokens.
	ForbidResolve bool
}

// New starts a fake for repo "owner/name" and closes it when t ends.
func New(t testing.TB, repo string) *Server {
	t.Helper()
	r, err := github.ParseRepo(repo)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		owner:            r.Owner,
		repo:             r.Name,
		prs:              map[int]*PR{},
		tokens:           map[string]string{DefaultToken: DefaultLogin},
		nextID:           1000,
		ThreadsPageSize:  50,
		CommentsPageSize: 100,
		IssuePageSize:    100,
	}
	s.srv = httptest.NewServer(s.routes())
	t.Cleanup(s.srv.Close)
	return s
}

// URL is the API root to pass to github.WithBaseURL.
func (s *Server) URL() string { return s.srv.URL }

// Client returns a github.Client for this fake authenticating with token.
func (s *Server) Client(token string) *github.Client {
	return github.NewClient(token, github.WithBaseURL(s.srv.URL), github.WithHTTPClient(s.srv.Client()))
}

// AddToken registers a token that authenticates as login.
func (s *Server) AddToken(token, login string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[token] = login
}

// AddPR stores a PR whose head is headSHA and whose diff is diff.
func (s *Server) AddPR(number int, headSHA, baseSHA, diff string) *PR {
	s.mu.Lock()
	defer s.mu.Unlock()
	pr := &PR{Number: number, HeadSHA: headSHA, BaseSHA: baseSHA, Diff: diff, Diffs: map[string]string{headSHA: diff}}
	s.prs[number] = pr
	return pr
}

// Push moves a PR's head, as a push would, replacing its diff.
func (s *Server) Push(number int, headSHA, diff string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pr := s.prs[number]
	pr.HeadSHA, pr.Diff = headSHA, diff
	pr.Diffs[headSHA] = diff
}

// PR returns the stored PR itself, not a copy. Read it only while no request
// is in flight.
func (s *Server) PR(number int) *PR {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.prs[number]
}

// Reply adds a comment from author to a thread, as a human or another
// reviewer would.
func (s *Server) Reply(number int, threadID, author, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.prs[number].thread(threadID)
	t.Comments = append(t.Comments, s.newComment(author, body, s.prs[number].HeadSHA))
}

// SetResolved resolves or unresolves a thread, as a human would.
func (s *Server) SetResolved(number int, threadID string, resolved bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prs[number].thread(threadID).Resolved = resolved
}

// AddThread starts a thread from another reviewer and returns its ID.
func (s *Server) AddThread(number int, author, path string, line int, body string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	pr := s.prs[number]
	t := &Thread{
		ID: s.newNodeID("PRRT"), Path: path, Side: github.SideRight,
		Line: line, OriginalLine: line, FileLevel: line == 0,
		Comments: []*Comment{s.newComment(author, body, pr.HeadSHA)},
	}
	pr.Threads = append(pr.Threads, t)
	return t.ID
}

func (pr *PR) thread(id string) *Thread {
	for _, t := range pr.Threads {
		if t.ID == id {
			return t
		}
	}
	panic("githubtest: no thread " + id)
}

func (s *Server) newNodeID(prefix string) string {
	s.nextID++
	return fmt.Sprintf("%s_%d", prefix, s.nextID)
}

func (s *Server) newComment(author, body, sha string) *Comment {
	s.nextID++
	return &Comment{
		DatabaseID: s.nextID,
		NodeID:     fmt.Sprintf("PRRC_%d", s.nextID),
		Author:     author,
		Body:       body,
		CommitSHA:  sha,
		CreatedAt:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(s.nextID) * time.Second),
	}
}

// handler is a request handler that runs under the lock with the caller's
// login and returns a status and a JSON-able body.
type handler func(r *http.Request, login string) (int, any)

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	prefix := "/repos/" + s.owner + "/" + s.repo
	handle := func(pattern, op string, h handler) {
		method, path, _ := strings.Cut(pattern, " ")
		mux.HandleFunc(method+" "+prefix+path, s.wrap(op, h))
	}
	handle("GET /pulls/{number}", "GET pull", s.getPull)
	handle("POST /pulls/{number}/reviews", "POST review", s.postReview)
	handle("POST /pulls/{number}/comments", "POST file comment", s.postFileComment)
	handle("POST /pulls/{number}/comments/{id}/replies", "POST reply", s.postReply)
	handle("DELETE /pulls/comments/{id}", "DELETE review comment", s.deleteReviewComment)
	handle("GET /issues/{number}/comments", "GET issue comments", s.listIssueComments)
	handle("POST /issues/{number}/comments", "POST issue comment", s.postIssueComment)
	handle("PATCH /issues/comments/{id}", "PATCH issue comment", s.patchIssueComment)
	mux.HandleFunc("POST /graphql", s.wrap("graphql", s.graphql))
	return mux
}

// rawBody marks a handler result to be written as-is rather than as JSON.
type rawBody string

func (s *Server) wrap(op string, h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if op == "graphql" {
			op = "graphql " + peekOperation(r)
		}
		if s.OnRequest != nil {
			if status := s.OnRequest(op); status != 0 {
				writeJSON(w, status, map[string]string{"message": "injected failure for " + op})
				return
			}
		}
		s.mu.Lock()
		login, ok := s.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		var status int
		var body any
		if !ok {
			status, body = http.StatusUnauthorized, msg("Bad credentials")
		} else {
			status, body = h(r, login)
		}
		s.mu.Unlock()
		if p, isPaged := body.(paged); isPaged && p.next {
			w.Header().Set("Link", fmt.Sprintf(`<%s%s?per_page=%d&page=%d>; rel="next"`,
				s.srv.URL, p.url, p.size, p.page+1))
		}
		if raw, isRaw := body.(rawBody); isRaw {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(raw))
			return
		}
		writeJSON(w, status, body)
	}
}

func msg(m string) map[string]string { return map[string]string{"message": m} }

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (s *Server) pr(r *http.Request) (*PR, bool) {
	n, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		return nil, false
	}
	pr, ok := s.prs[n]
	return pr, ok
}

func decode(r *http.Request, v any) bool {
	return json.NewDecoder(r.Body).Decode(v) == nil
}

func (s *Server) getPull(r *http.Request, _ string) (int, any) {
	pr, ok := s.pr(r)
	if !ok {
		return http.StatusNotFound, msg("Not Found")
	}
	if r.Header.Get("Accept") == "application/vnd.github.diff" {
		return http.StatusOK, rawBody(pr.Diff)
	}
	return http.StatusOK, map[string]any{
		"number": pr.Number, "state": "open", "title": "fake PR", "node_id": fmt.Sprintf("PR_%d", pr.Number),
		"user": map[string]string{"login": "dratner"},
		"head": map[string]string{"sha": pr.HeadSHA, "ref": "feature"},
		"base": map[string]string{"sha": pr.BaseSHA, "ref": "main"},
	}
}

type reviewCommentIn struct {
	Path      string      `json:"path"`
	Body      string      `json:"body"`
	Side      github.Side `json:"side"`
	StartSide github.Side `json:"start_side"`
	Line      int         `json:"line"`
	StartLine int         `json:"start_line"`
}

func (s *Server) postReview(r *http.Request, login string) (int, any) {
	pr, ok := s.pr(r)
	if !ok {
		return http.StatusNotFound, msg("Not Found")
	}
	var in struct {
		CommitID string            `json:"commit_id"`
		Body     string            `json:"body"`
		Event    string            `json:"event"`
		Comments []reviewCommentIn `json:"comments"`
	}
	if !decode(r, &in) {
		return http.StatusBadRequest, msg("Problems parsing JSON")
	}
	if in.Event != "COMMENT" {
		return http.StatusUnprocessableEntity, msg("fake only supports COMMENT reviews")
	}
	if in.Body == "" && len(in.Comments) == 0 {
		return http.StatusUnprocessableEntity, msg("Review body is required")
	}
	diff, status, body := pr.diffAt(in.CommitID)
	if status != 0 {
		return status, body
	}
	// GitHub rejects the whole review if any comment doesn't resolve.
	threads := make([]*Thread, 0, len(in.Comments))
	for _, c := range in.Comments {
		f := diff.File(c.Path)
		side := c.Side
		if side == "" {
			side = github.SideRight
		}
		okLine := f != nil && f.Commentable(c.Line, side)
		if c.StartLine > 0 {
			okLine = f != nil && c.StartSide == side && f.CommentableRange(c.StartLine, c.Line, side)
		}
		if !okLine {
			return http.StatusUnprocessableEntity, msg(fmt.Sprintf("Line could not be resolved: %s:%d", c.Path, c.Line))
		}
		threads = append(threads, &Thread{
			ID: s.newNodeID("PRRT"), Path: c.Path, Side: side,
			Line: c.Line, OriginalLine: c.Line, StartLine: c.StartLine,
			Comments: []*Comment{s.newComment(login, c.Body, in.CommitID)},
		})
	}
	pr.Threads = append(pr.Threads, threads...)
	pr.Reviews++
	return http.StatusOK, map[string]any{"id": s.nextID, "state": "COMMENTED"}
}

// diffAt parses the PR's diff at commit, which must be a head the PR has had.
func (pr *PR) diffAt(commit string) (*github.Diff, int, any) {
	text, ok := pr.Diffs[commit]
	if !ok {
		return nil, http.StatusUnprocessableEntity, msg("commit_id is not part of the pull request")
	}
	d, err := github.ParseDiff(text)
	if err != nil {
		return nil, http.StatusInternalServerError, msg("fake: bad diff: " + err.Error())
	}
	return d, 0, nil
}

func (s *Server) postFileComment(r *http.Request, login string) (int, any) {
	pr, ok := s.pr(r)
	if !ok {
		return http.StatusNotFound, msg("Not Found")
	}
	var in struct {
		CommitID    string `json:"commit_id"`
		Path        string `json:"path"`
		Body        string `json:"body"`
		SubjectType string `json:"subject_type"`
	}
	if !decode(r, &in) {
		return http.StatusBadRequest, msg("Problems parsing JSON")
	}
	if in.SubjectType != "file" {
		return http.StatusUnprocessableEntity, msg("fake only supports subject_type file here")
	}
	diff, status, body := pr.diffAt(in.CommitID)
	if status != 0 {
		return status, body
	}
	if diff.File(in.Path) == nil {
		return http.StatusUnprocessableEntity, msg("path is not part of the diff")
	}
	c := s.newComment(login, in.Body, in.CommitID)
	pr.Threads = append(pr.Threads, &Thread{
		ID: s.newNodeID("PRRT"), Path: in.Path, FileLevel: true, Comments: []*Comment{c},
	})
	return http.StatusCreated, restComment(c)
}

func restComment(c *Comment) map[string]any {
	return map[string]any{
		"id": c.DatabaseID, "node_id": c.NodeID, "body": c.Body,
		"user":       map[string]string{"login": c.Author},
		"created_at": c.CreatedAt.Format(time.RFC3339),
		"html_url":   fmt.Sprintf("https://github.example/comment/%d", c.DatabaseID),
	}
}

// findComment returns the thread holding the review comment with database ID
// id, and the comment's index in it.
func (pr *PR) findComment(id int64) (*Thread, int) {
	for _, t := range pr.Threads {
		for i, c := range t.Comments {
			if c.DatabaseID == id {
				return t, i
			}
		}
	}
	return nil, -1
}

func (s *Server) postReply(r *http.Request, login string) (int, any) {
	pr, ok := s.pr(r)
	if !ok {
		return http.StatusNotFound, msg("Not Found")
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return http.StatusNotFound, msg("Not Found")
	}
	t, i := pr.findComment(id)
	if t == nil {
		return http.StatusNotFound, msg("Not Found")
	}
	if i != 0 {
		return http.StatusUnprocessableEntity, msg("Replies to replies are not supported")
	}
	var in struct {
		Body string `json:"body"`
	}
	if !decode(r, &in) {
		return http.StatusBadRequest, msg("Problems parsing JSON")
	}
	c := s.newComment(login, in.Body, pr.HeadSHA)
	t.Comments = append(t.Comments, c)
	return http.StatusCreated, restComment(c)
}

func (s *Server) deleteReviewComment(r *http.Request, login string) (int, any) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return http.StatusNotFound, msg("Not Found")
	}
	for _, pr := range s.prs {
		t, i := pr.findComment(id)
		if t == nil {
			continue
		}
		if t.Comments[i].Author != login {
			return http.StatusForbidden, msg("Must have admin rights to Repository.")
		}
		t.Comments = append(t.Comments[:i], t.Comments[i+1:]...)
		if len(t.Comments) == 0 {
			kept := pr.Threads[:0]
			for _, other := range pr.Threads {
				if other != t {
					kept = append(kept, other)
				}
			}
			pr.Threads = kept
		}
		return http.StatusNoContent, rawBody("")
	}
	return http.StatusNotFound, msg("Not Found")
}

func (s *Server) listIssueComments(r *http.Request, _ string) (int, any) {
	pr, ok := s.pr(r)
	if !ok {
		return http.StatusNotFound, msg("Not Found")
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = max(page, 1)
	size := s.IssuePageSize
	lo := min((page-1)*size, len(pr.IssueComments))
	hi := min(lo+size, len(pr.IssueComments))
	out := make([]map[string]any, 0, hi-lo)
	for _, c := range pr.IssueComments[lo:hi] {
		out = append(out, restComment(c))
	}
	return http.StatusOK, paged{items: out, next: hi < len(pr.IssueComments), page: page, size: size, url: r.URL.Path}
}

// paged is a REST list page; wrap adds its Link header.
type paged struct {
	items any
	url   string
	page  int
	size  int
	next  bool
}

// MarshalJSON writes the page's items.
func (p paged) MarshalJSON() ([]byte, error) { return json.Marshal(p.items) }

func (s *Server) postIssueComment(r *http.Request, login string) (int, any) {
	pr, ok := s.pr(r)
	if !ok {
		return http.StatusNotFound, msg("Not Found")
	}
	var in struct {
		Body string `json:"body"`
	}
	if !decode(r, &in) {
		return http.StatusBadRequest, msg("Problems parsing JSON")
	}
	c := s.newComment(login, in.Body, "")
	c.NodeID = fmt.Sprintf("IC_%d", c.DatabaseID)
	pr.IssueComments = append(pr.IssueComments, c)
	return http.StatusCreated, restComment(c)
}

func (s *Server) patchIssueComment(r *http.Request, login string) (int, any) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return http.StatusNotFound, msg("Not Found")
	}
	var in struct {
		Body string `json:"body"`
	}
	if !decode(r, &in) {
		return http.StatusBadRequest, msg("Problems parsing JSON")
	}
	for _, pr := range s.prs {
		for _, c := range pr.IssueComments {
			if c.DatabaseID != id {
				continue
			}
			if c.Author != login {
				return http.StatusForbidden, msg("Resource not accessible by integration")
			}
			c.Body = in.Body
			return http.StatusOK, restComment(c)
		}
	}
	return http.StatusNotFound, msg("Not Found")
}
