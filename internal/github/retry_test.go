package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// retryServer fails the first fails requests with status and header, then
// succeeds, counting attempts.
func retryServer(t *testing.T, fails int, status int, header map[string]string) (*Client, *atomic.Int32, *[]time.Duration) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if int(n.Add(1)) <= fails {
			for k, v := range header {
				w.Header().Set(k, v)
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"message":"try later"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id": 1}`))
	}))
	t.Cleanup(srv.Close)
	c := NewClient("t", WithBaseURL(srv.URL))
	var waits []time.Duration
	c.sleep = func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}
	return c, &n, &waits
}

func TestRetries(t *testing.T) {
	tests := []struct {
		header   map[string]string
		name     string
		method   string
		status   int
		fails    int
		attempts int32
		ok       bool
	}{
		{name: "GET 502 retried", method: http.MethodGet, status: 502, fails: 2, attempts: 3, ok: true},
		{name: "GET 502 gives up", method: http.MethodGet, status: 502, fails: 5, attempts: 3},
		{name: "POST 502 not retried", method: http.MethodPost, status: 502, fails: 1, attempts: 1},
		{name: "POST 429 retried", method: http.MethodPost, status: 429, fails: 1, attempts: 2, ok: true,
			header: map[string]string{"Retry-After": "1"}},
		{name: "POST secondary rate limit retried", method: http.MethodPost, status: 403, fails: 1, attempts: 2, ok: true,
			header: map[string]string{"Retry-After": "3"}},
		{name: "403 without rate limit not retried", method: http.MethodGet, status: 403, fails: 1, attempts: 1},
		{name: "long Retry-After not waited", method: http.MethodGet, status: 429, fails: 1, attempts: 1,
			header: map[string]string{"Retry-After": "3600"}},
		{name: "422 not retried", method: http.MethodGet, status: 422, fails: 1, attempts: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, n, _ := retryServer(t, tt.fails, tt.status, tt.header)
			var out struct{ ID int }
			_, _, err := c.do(context.Background(), &request{method: tt.method, url: c.restURL + "/x", out: &out})
			if (err == nil) != tt.ok {
				t.Errorf("err = %v, want ok=%v", err, tt.ok)
			}
			if got := n.Load(); got != tt.attempts {
				t.Errorf("attempts = %d, want %d", got, tt.attempts)
			}
		})
	}
}

// truncatingServer sends a 200 with a body shorter than its Content-Length
// for the first fails requests, so the client's body read fails.
func truncatingServer(t *testing.T, fails int) (*Client, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if int(n.Add(1)) <= fails {
			w.Header().Set("Content-Length", "100")
			_, _ = w.Write([]byte(`{"id":`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	t.Cleanup(srv.Close)
	c := NewClient("t", WithBaseURL(srv.URL))
	c.sleep = func(context.Context, time.Duration) error { return nil }
	return c, &n
}

func TestTruncatedBodyRetriedForReadsOnly(t *testing.T) {
	c, n := truncatingServer(t, 1)
	if _, _, err := c.do(context.Background(), &request{method: http.MethodGet, url: c.restURL + "/x"}); err != nil {
		t.Errorf("GET: %v", err)
	}
	if n.Load() != 2 {
		t.Errorf("GET attempts = %d, want 2", n.Load())
	}

	c, n = truncatingServer(t, 1)
	if err := c.graphQL(context.Background(), "Q", "query Q { x }", nil, nil); err != nil {
		t.Errorf("query: %v", err)
	}
	if n.Load() != 2 {
		t.Errorf("query attempts = %d, want 2", n.Load())
	}

	c, n = truncatingServer(t, 1)
	if err := c.graphQL(context.Background(), "M", "mutation M { x }", nil, nil); err == nil {
		t.Error("mutation succeeded through a truncated body")
	}
	if n.Load() != 1 {
		t.Errorf("mutation attempts = %d, want 1", n.Load())
	}
}

// A rate-limited response whose body is cut off still follows the rate-limit
// rule: Retry-After is honored, and mutations are retried too.
func TestTruncatedRateLimitHonorsHeaders(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			var n atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if n.Add(1) == 1 {
					w.Header().Set("Retry-After", "30")
					w.Header().Set("Content-Length", "100")
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = w.Write([]byte(`{"mess`))
					return
				}
				_, _ = w.Write([]byte(`{}`))
			}))
			t.Cleanup(srv.Close)
			c := NewClient("t", WithBaseURL(srv.URL))
			var waits []time.Duration
			c.sleep = func(_ context.Context, d time.Duration) error {
				waits = append(waits, d)
				return nil
			}
			if _, _, err := c.do(context.Background(), &request{method: method, url: srv.URL}); err != nil {
				t.Fatal(err)
			}
			if n.Load() != 2 || len(waits) != 1 || waits[0] != 30*time.Second {
				t.Errorf("attempts = %d, waits = %v; want 2 attempts after one 30s wait", n.Load(), waits)
			}
		})
	}
}

func TestDecodeFailureNotRetried(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		_, _ = w.Write([]byte(`not json`))
	}))
	t.Cleanup(srv.Close)
	c := NewClient("t", WithBaseURL(srv.URL))
	var out struct{}
	if _, _, err := c.do(context.Background(), &request{method: http.MethodGet, url: srv.URL, out: &out}); err == nil {
		t.Error("decoded invalid JSON")
	}
	if n.Load() != 1 {
		t.Errorf("attempts = %d, want 1", n.Load())
	}
}

func TestRetryHonorsRetryAfter(t *testing.T) {
	c, _, waits := retryServer(t, 1, 429, map[string]string{"Retry-After": "7"})
	if _, _, err := c.do(context.Background(), &request{method: http.MethodPost, url: c.restURL + "/x"}); err != nil {
		t.Fatal(err)
	}
	if len(*waits) != 1 || (*waits)[0] != 7*time.Second {
		t.Errorf("waits = %v, want [7s]", *waits)
	}
}

func TestGraphQLQueryRetriedMutationNot(t *testing.T) {
	c, n, _ := retryServer(t, 1, 502, nil)
	if err := c.graphQL(context.Background(), "Q", "query Q { x }", nil, nil); err != nil {
		t.Fatalf("query: %v", err)
	}
	if n.Load() != 2 {
		t.Errorf("query attempts = %d, want 2", n.Load())
	}
	c, n, _ = retryServer(t, 1, 502, nil)
	if err := c.graphQL(context.Background(), "M", "mutation M { x }", nil, nil); err == nil {
		t.Fatal("mutation succeeded through a 502")
	}
	if n.Load() != 1 {
		t.Errorf("mutation attempts = %d, want 1", n.Load())
	}
}

func TestGraphQLErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":null,"errors":[{"type":"FORBIDDEN","message":"Resource not accessible by integration"}]}`))
	}))
	t.Cleanup(srv.Close)
	c := NewClient("t", WithBaseURL(srv.URL))
	err := c.ResolveThread(context.Background(), "PRRT_1")
	if !IsForbidden(err) {
		t.Errorf("error = %v, want forbidden", err)
	}
}

func TestBaseURLs(t *testing.T) {
	c := NewClient("t")
	if c.restURL != "https://api.github.com" || c.gqlURL != "https://api.github.com/graphql" {
		t.Errorf("default = %s, %s", c.restURL, c.gqlURL)
	}
	c = NewClient("t", WithBaseURL("https://ghe.example/api/v3/"))
	if c.restURL != "https://ghe.example/api/v3" || c.gqlURL != "https://ghe.example/api/graphql" {
		t.Errorf("GHE = %s, %s", c.restURL, c.gqlURL)
	}
	if got := c.rest("repos", "o", "a b"); got != "https://ghe.example/api/v3/repos/o/a%20b" {
		t.Errorf("rest = %s", got)
	}
}

func TestNextLink(t *testing.T) {
	h := `<https://api.github.com/x?page=2>; rel="next", <https://api.github.com/x?page=5>; rel="last"`
	if got := nextLink(h); got != "https://api.github.com/x?page=2" {
		t.Errorf("next = %q", got)
	}
	if got := nextLink(`<https://api.github.com/x?page=1>; rel="prev"`); got != "" {
		t.Errorf("next = %q, want empty", got)
	}
}

func TestParseRepo(t *testing.T) {
	if r, err := ParseRepo("acme/widgets"); err != nil || r.String() != "acme/widgets" {
		t.Errorf("got %v, %v", r, err)
	}
	for _, bad := range []string{"", "acme", "/w", "a/", "a/b/c"} {
		if _, err := ParseRepo(bad); err == nil {
			t.Errorf("ParseRepo(%q) succeeded", bad)
		}
	}
}
