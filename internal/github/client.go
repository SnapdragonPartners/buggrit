// Package github is buggrit's GitHub layer: a small REST and GraphQL client
// for the calls a review pass makes, unified-diff parsing, and the mapping
// from file lines to review-comment anchors (docs/design.md §2, §3, §6).
//
// It uses only the standard library. Tests run it against the fake server in
// githubtest.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultAPIURL is GitHub's public REST endpoint. GraphQL is at /graphql
// under the same root.
const DefaultAPIURL = "https://api.github.com"

// Repo names a repository.
type Repo struct {
	Owner string
	Name  string
}

// ParseRepo parses "owner/name".
func ParseRepo(s string) (Repo, error) {
	owner, name, ok := strings.Cut(s, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return Repo{}, fmt.Errorf("repository %q is not owner/name", s)
	}
	return Repo{Owner: owner, Name: name}, nil
}

func (r Repo) String() string { return r.Owner + "/" + r.Name }

// Client talks to the GitHub API with one token.
type Client struct {
	http    *http.Client
	sleep   func(context.Context, time.Duration) error
	token   string
	restURL string
	gqlURL  string
	retries int
}

// Option customizes NewClient.
type Option func(*Client)

// WithBaseURL points the client at another API root, such as GitHub
// Enterprise ("https://ghe.example/api/v3") or a test server. GraphQL is
// taken to be at "<root>/graphql", except for GHE's "/api/v3" root, whose
// GraphQL endpoint is "/api/graphql".
func WithBaseURL(root string) Option {
	return func(c *Client) {
		root = strings.TrimSuffix(root, "/")
		c.restURL = root
		if base, ok := strings.CutSuffix(root, "/api/v3"); ok {
			c.gqlURL = base + "/api/graphql"
		} else {
			c.gqlURL = root + "/graphql"
		}
	}
}

// WithHTTPClient replaces the HTTP client.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// WithRetries sets how many attempts a retryable request gets (default 3).
func WithRetries(attempts int) Option {
	return func(c *Client) { c.retries = max(attempts, 1) }
}

// NewClient returns a client authenticating with token.
func NewClient(token string, opts ...Option) *Client {
	c := &Client{
		http:    &http.Client{Timeout: 60 * time.Second},
		sleep:   sleepCtx,
		token:   token,
		retries: 3,
	}
	WithBaseURL(DefaultAPIURL)(c)
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// APIError is a non-2xx REST response or a GraphQL error.
type APIError struct {
	Message string
	// Type is the GraphQL error type (for example FORBIDDEN or NOT_FOUND);
	// empty for REST errors.
	Type   string
	Status int
}

func (e *APIError) Error() string {
	switch {
	case e.Type != "":
		return fmt.Sprintf("github: %s: %s", e.Type, e.Message)
	case e.Status != 0:
		return fmt.Sprintf("github: HTTP %d: %s", e.Status, e.Message)
	}
	return "github: " + e.Message
}

// IsForbidden reports whether err is GitHub refusing the token, as it does for
// resolveReviewThread with some integration tokens ("Resource not accessible
// by integration", design §3). Callers fall back rather than fail the pass.
func IsForbidden(err error) bool {
	var e *APIError
	if !errors.As(err, &e) {
		return false
	}
	return e.Status == http.StatusForbidden || e.Type == "FORBIDDEN" ||
		strings.Contains(e.Message, "Resource not accessible by integration")
}

// IsNotFound reports whether err is a 404 or a GraphQL NOT_FOUND.
func IsNotFound(err error) bool {
	var e *APIError
	return errors.As(err, &e) && (e.Status == http.StatusNotFound || e.Type == "NOT_FOUND")
}

// errBodyRead marks a response whose body failed to arrive in full.
var errBodyRead = errors.New("read body")

// request is one API call.
type request struct {
	body   any
	out    any
	header http.Header
	method string
	url    string
	accept string
	// read marks a POST that only reads (a GraphQL query), so it is retried
	// like a GET.
	read bool
}

// do sends a request, retrying rate limits and, for reads, server errors. It
// decodes a JSON response into r.out, or returns the raw body when r.out is
// nil.
func (c *Client) do(ctx context.Context, r *request) (*http.Response, []byte, error) {
	var payload []byte
	if r.body != nil {
		var err error
		if payload, err = json.Marshal(r.body); err != nil {
			return nil, nil, fmt.Errorf("encode %s %s: %w", r.method, r.url, err)
		}
	}
	idempotent := r.method == http.MethodGet || r.read
	for attempt := 1; ; attempt++ {
		resp, body, err := c.once(ctx, r, payload)
		if err == nil {
			return resp, body, nil
		}
		wait, retry := retryAfter(resp, err, idempotent)
		if !retry || attempt >= c.retries {
			return resp, body, err
		}
		if serr := c.sleep(ctx, wait); serr != nil {
			return resp, body, err
		}
	}
}

func (c *Client) once(ctx context.Context, r *request, payload []byte) (*http.Response, []byte, error) {
	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, r.method, r.url, rd)
	if err != nil {
		return nil, nil, fmt.Errorf("build %s %s: %w", r.method, r.url, err)
	}
	for k, v := range r.header {
		req.Header[k] = v
	}
	accept := r.accept
	if accept == "" {
		accept = "application/vnd.github+json"
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "buggrit")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req) //nolint:gosec // the URL is built from the configured API root
	if err != nil {
		return nil, nil, fmt.Errorf("%s %s: %w", r.method, r.url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return resp, nil, fmt.Errorf("%s %s: %w: %w", r.method, r.url, errBodyRead, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp, body, &APIError{Status: resp.StatusCode, Message: errorMessage(body)}
	}
	if r.out != nil && len(body) > 0 {
		if err := json.Unmarshal(body, r.out); err != nil {
			return resp, body, fmt.Errorf("%s %s: decode response: %w", r.method, r.url, err)
		}
	}
	return resp, body, nil
}

// retryAfter decides whether a failed attempt is worth retrying, and after how
// long. Rate limits are retried for every method, since GitHub rejected the
// request without acting on it. Server errors and transport failures are
// retried only for reads: a mutation may have landed, and the markers make
// the next pass repair it instead (design §2).
func retryAfter(resp *http.Response, err error, idempotent bool) (time.Duration, bool) {
	const maxWait = 60 * time.Second
	// A body cut off mid-read is a transport failure even though a status
	// line arrived first.
	if resp == nil || errors.Is(err, errBodyRead) {
		return 2 * time.Second, idempotent && !errors.Is(err, context.Canceled) &&
			!errors.Is(err, context.DeadlineExceeded)
	}
	limited := resp.StatusCode == http.StatusTooManyRequests ||
		(resp.StatusCode == http.StatusForbidden &&
			(resp.Header.Get("Retry-After") != "" || resp.Header.Get("X-RateLimit-Remaining") == "0"))
	if !limited && !(idempotent && resp.StatusCode >= 500) {
		return 0, false
	}
	wait := 2 * time.Second
	if s, perr := strconv.Atoi(resp.Header.Get("Retry-After")); perr == nil && s >= 0 {
		wait = time.Duration(s) * time.Second
	} else if reset, perr := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); perr == nil &&
		resp.Header.Get("X-RateLimit-Remaining") == "0" {
		wait = time.Until(time.Unix(reset, 0))
	}
	if wait > maxWait {
		// Waiting longer than this eats the pass deadline; fail and let the
		// pass report itself incomplete.
		return 0, false
	}
	return max(wait, 0), true
}

func errorMessage(body []byte) string {
	var e struct {
		Message string `json:"message"`
		Errors  []struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"errors"`
	}
	if json.Unmarshal(body, &e) != nil || e.Message == "" {
		s := strings.TrimSpace(string(body))
		if len(s) > 200 {
			s = s[:200] + "…"
		}
		return s
	}
	msg := e.Message
	for _, d := range e.Errors {
		if d.Message != "" {
			msg += "; " + d.Message
		} else if d.Code != "" {
			msg += "; " + d.Code
		}
	}
	return msg
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err() //nolint:wrapcheck // the caller reports its own error, not this one
	case <-t.C:
		return nil
	}
}

// rest builds a REST URL from path segments, escaping each one.
func (c *Client) rest(segments ...string) string {
	var b strings.Builder
	b.WriteString(c.restURL)
	for _, s := range segments {
		b.WriteByte('/')
		b.WriteString(url.PathEscape(s))
	}
	return b.String()
}

func (r Repo) segments(more ...string) []string {
	return append([]string{"repos", r.Owner, r.Name}, more...)
}

// graphQL runs one GraphQL operation and decodes its data into out.
func (c *Client) graphQL(ctx context.Context, op, query string, vars map[string]any, out any) error {
	var resp struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	_, _, err := c.do(ctx, &request{
		method: http.MethodPost,
		url:    c.gqlURL,
		body:   map[string]any{"query": query, "operationName": op, "variables": vars},
		out:    &resp,
		// Queries are reads and safe to retry; mutations are not.
		read: strings.HasPrefix(strings.TrimSpace(query), "query"),
	})
	if err != nil {
		return fmt.Errorf("graphql %s: %w", op, err)
	}
	if len(resp.Errors) > 0 {
		e := resp.Errors[0]
		return fmt.Errorf("graphql %s: %w", op, &APIError{Type: e.Type, Message: e.Message})
	}
	if out != nil {
		if err := json.Unmarshal(resp.Data, out); err != nil {
			return fmt.Errorf("graphql %s: decode data: %w", op, err)
		}
	}
	return nil
}
