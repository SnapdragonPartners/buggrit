package githubtest

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type gqlRequest struct {
	Variables     map[string]any `json:"variables"`
	OperationName string         `json:"operationName"`
}

// peekOperation reads a GraphQL request's operation name and restores the
// body for the handler.
func peekOperation(r *http.Request) string {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return ""
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var req gqlRequest
	_ = json.Unmarshal(body, &req)
	return req.OperationName
}

func gqlError(typ, message string) (int, any) {
	return http.StatusOK, map[string]any{
		"data":   nil,
		"errors": []map[string]string{{"type": typ, "message": message}},
	}
}

func data(v any) (int, any) { return http.StatusOK, map[string]any{"data": v} }

func (s *Server) graphql(r *http.Request, login string) (int, any) {
	var req gqlRequest
	if !decode(r, &req) {
		return http.StatusBadRequest, msg("Problems parsing JSON")
	}
	vars := req.Variables
	switch req.OperationName {
	case "ReviewThreads":
		if vars["owner"] != s.owner || vars["name"] != s.repo {
			return data(map[string]any{"repository": nil})
		}
		number, _ := vars["number"].(float64)
		pr, ok := s.prs[int(number)]
		if !ok {
			return data(map[string]any{"repository": map[string]any{"pullRequest": nil}})
		}
		lo := cursorStart(vars["after"])
		hi := min(lo+s.ThreadsPageSize, len(pr.Threads))
		nodes := make([]map[string]any, 0, hi-lo)
		for _, t := range pr.Threads[lo:hi] {
			nodes = append(nodes, s.threadNode(t))
		}
		return data(map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
			"reviewThreads": map[string]any{"nodes": nodes, "pageInfo": pageInfo(hi, len(pr.Threads))},
		}}})
	case "ThreadComments":
		t := s.findThread(vars["id"])
		if t == nil {
			return data(map[string]any{"node": nil})
		}
		return data(map[string]any{"node": map[string]any{
			"comments": s.commentPage(t, cursorStart(vars["after"])),
		}})
	case "ThreadResolved":
		t := s.findThread(vars["id"])
		if t == nil {
			return data(map[string]any{"node": nil})
		}
		return data(map[string]any{"node": map[string]any{"isResolved": t.Resolved}})
	case "ResolveThread", "UnresolveThread":
		if s.ForbidResolve {
			return gqlError("FORBIDDEN", "Resource not accessible by integration")
		}
		t := s.findThread(vars["id"])
		if t == nil {
			return gqlError("NOT_FOUND", "Could not resolve to a node")
		}
		resolved := req.OperationName == "ResolveThread"
		if resolved != t.Resolved {
			t.Resolved, t.ResolvedBy = resolved, ""
			if resolved {
				t.ResolvedBy = login
			}
		}
		key := "resolveReviewThread"
		if !t.Resolved {
			key = "unresolveReviewThread"
		}
		return data(map[string]any{key: map[string]any{"thread": map[string]any{"id": t.ID, "isResolved": t.Resolved}}})
	}
	return gqlError("UNKNOWN_OPERATION", "fake does not support operation "+strconv.Quote(req.OperationName))
}

func (s *Server) findThread(id any) *Thread {
	for _, pr := range s.prs {
		for _, t := range pr.Threads {
			if t.ID == id {
				return t
			}
		}
	}
	return nil
}

// Cursors are the decimal index of the next item.
func cursorStart(after any) int {
	a, _ := after.(string)
	n, _ := strconv.Atoi(a)
	return max(n, 0)
}

func pageInfo(next, total int) map[string]any {
	return map[string]any{"hasNextPage": next < total, "endCursor": strconv.Itoa(next)}
}

func nullable(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

func (s *Server) threadNode(t *Thread) map[string]any {
	subject := "LINE"
	if t.FileLevel {
		subject = "FILE"
	}
	line := nullable(t.Line)
	if t.Outdated {
		line = nil
	}
	var side any = string(t.Side)
	if t.FileLevel {
		side = nil
	}
	// GitHub reports a bot resolver as a User whose login keeps "[bot]".
	var resolvedBy any
	if t.Resolved && t.ResolvedBy != "" {
		resolvedBy = map[string]any{"__typename": "User", "login": t.ResolvedBy}
	}
	return map[string]any{
		"id": t.ID, "isResolved": t.Resolved, "resolvedBy": resolvedBy, "isOutdated": t.Outdated, "path": t.Path,
		"line": line, "originalLine": nullable(t.OriginalLine), "startLine": nullable(t.StartLine),
		"diffSide": side, "subjectType": subject,
		"comments": s.commentPage(t, 0),
	}
}

func (s *Server) commentPage(t *Thread, lo int) map[string]any {
	lo = min(lo, len(t.Comments))
	hi := min(lo+s.CommentsPageSize, len(t.Comments))
	nodes := make([]map[string]any, 0, hi-lo)
	for _, c := range t.Comments[lo:hi] {
		nodes = append(nodes, gqlComment(c))
	}
	return map[string]any{"nodes": nodes, "pageInfo": pageInfo(hi, len(t.Comments))}
}

// gqlComment renders a comment the way GitHub's GraphQL API does, including
// a bot author's login without its "[bot]" suffix.
func gqlComment(c *Comment) map[string]any {
	author := map[string]any{"__typename": "User", "login": c.Author}
	if login, isBot := strings.CutSuffix(c.Author, "[bot]"); isBot {
		author = map[string]any{"__typename": "Bot", "login": login}
	}
	var commit any
	if c.CommitSHA != "" {
		commit = map[string]any{"oid": c.CommitSHA}
	}
	return map[string]any{
		"id": c.NodeID, "databaseId": c.DatabaseID, "body": c.Body, "diffHunk": c.DiffHunk,
		"url":       "https://github.example/comment/" + strconv.FormatInt(c.DatabaseID, 10),
		"createdAt": c.CreatedAt.Format(time.RFC3339),
		"author":    author, "commit": commit, "authorAssociation": c.Association,
	}
}
