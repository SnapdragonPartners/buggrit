package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/bucket"
	bucketpg "github.com/SnapdragonPartners/maestro-llms/bucket/postgres"
	"github.com/SnapdragonPartners/maestro-llms/llms"

	"github.com/SnapdragonPartners/buggrit/internal/config"
	"github.com/SnapdragonPartners/buggrit/internal/llm"
)

// fakeBuckets records what was ensured, and fails every lease with
// acquireErr.
type fakeBuckets struct {
	acquireErr error
	ensureErr  error
	ensured    []string
}

func (f *fakeBuckets) Acquire(context.Context, string, int) (bucket.Lease, error) {
	return nil, f.acquireErr
}

func (f *fakeBuckets) Ensure(_ context.Context, name string, l bucket.Limits, overwrite bool) error {
	f.ensured = append(f.ensured, fmt.Sprintf("%s %d/%d %v", name, l.MaxConcurrency, l.TokensPerMinute, overwrite))
	return f.ensureErr
}

func TestParseRateLimits(t *testing.T) {
	got, err := parseRateLimits(`{"luna": {"base_url": "https://gw.example/v1", "api_key_env": "K", "max_concurrency": 2, "max_wait": "3m"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if l := got["luna"]; l.MaxConcurrency != 2 || l.MaxWait.Std() != 3*time.Minute || l.APIKeyEnv != "K" {
		t.Errorf("got %+v", got)
	}
	for raw, want := range map[string]string{
		`not json`: "invalid character",
		`{"luna": {"base_url": "u", "max_concurency": 2}}`:                           "unknown field",
		`{"luna": {"max_concurrency": 2}}`:                                           "base_url is required",
		`{"luna": {"base_url": "u"}}`:                                                "set max_concurrency, tokens_per_minute or both",
		`{"luna": {"base_url": "u", "max_concurrency": -1, "tokens_per_minute": 5}}`: "can't be negative",
		`{"": {"base_url": "u", "max_concurrency": 1}}`:                              "empty name",
		`{} {}`: "more than one JSON value",
	} {
		if _, err := parseRateLimits(raw); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("parseRateLimits(%s) = %v, want %q", raw, err, want)
		}
	}
}

func limitsCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(`{"version": 1, "limits": {"pass_deadline": "40m"}, "models": {
	  "default": {"provider": "openai-compatible", "base_url": "https://gw.example/v1/", "model": "m", "api_key_env": "K", "request_timeout": "20m"},
	  "verify": {"base_url": "https://other.example"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestSharedLimitsMatchesStages(t *testing.T) {
	cfg := limitsCfg(t)
	b := &fakeBuckets{}
	var opened bucketpg.Config
	open := func(c bucketpg.Config) (sharedBuckets, error) { opened = c; return b, nil }
	var log strings.Builder
	logf := func(f string, a ...any) { fmt.Fprintf(&log, f, a...) }
	raw := `{"luna": {"base_url": "https://gw.example/v1", "api_key_env": "K", "max_concurrency": 2},
	  "unused": {"base_url": "https://nowhere.example", "max_concurrency": 1}}`
	s, err := sharedLimits(context.Background(), cfg, raw, open, logf)
	if err != nil {
		t.Fatal(err)
	}
	want := map[config.Stage]llm.SharedStage{
		config.StageFind:   {Bucket: "luna", MaxWait: 10 * time.Minute},
		config.StageDedupe: {Bucket: "luna", MaxWait: 10 * time.Minute},
	}
	if fmt.Sprint(s.Stages) != fmt.Sprint(want) {
		t.Errorf("stages = %v, want %v", s.Stages, want)
	}
	// Each bucket in use is set to the variable's limits, overwriting the
	// database's.
	if fmt.Sprint(b.ensured) != "[luna 2/0 true]" {
		t.Errorf("ensured = %v", b.ensured)
	}
	if opened.MaxLeaseDuration != 21*time.Minute {
		t.Errorf("MaxLeaseDuration = %s", opened.MaxLeaseDuration)
	}
	if !strings.Contains(log.String(), "verify's endpoint isn't in BUGGRIT_RATE_LIMITS") {
		t.Errorf("log = %q", log.String())
	}
}

func TestSharedLimitsNone(t *testing.T) {
	cfg := limitsCfg(t)
	open := func(bucketpg.Config) (sharedBuckets, error) {
		t.Fatal("opened the buckets")
		return nil, errors.New("unreachable")
	}
	logf := func(string, ...any) {}
	for _, raw := range []string{"", `{"x": {"base_url": "https://nowhere.example", "max_concurrency": 1}}`} {
		if s, err := sharedLimits(context.Background(), cfg, raw, open, logf); s != nil || err != nil {
			t.Errorf("%q: %v, %v", raw, s, err)
		}
	}
}

func TestSharedLimitsErrors(t *testing.T) {
	cfg := limitsCfg(t)
	logf := func(string, ...any) {}
	good := `{"luna": {"base_url": "https://gw.example/v1", "api_key_env": "K", "max_concurrency": 2}}`
	for name, tc := range map[string]struct {
		raw  string
		open func(bucketpg.Config) (sharedBuckets, error)
		want string
	}{
		"ambiguous": {
			raw:  `{"a": {"base_url": "https://gw.example/v1", "api_key_env": "K", "max_concurrency": 2}, "b": {"base_url": "https://gw.example/v1", "api_key_env": "K", "max_concurrency": 1}}`,
			want: "buckets a, b all match the find stage's endpoint",
		},
		"open": {
			raw:  good,
			open: func(bucketpg.Config) (sharedBuckets, error) { return nil, errors.New("schema behind") },
			want: "the shared rate limit: schema behind",
		},
		"ensure": {
			raw: good,
			open: func(bucketpg.Config) (sharedBuckets, error) {
				return &fakeBuckets{ensureErr: errors.New("permission denied")}, nil
			},
			want: `set the shared rate limit "luna": permission denied`,
		},
	} {
		open := tc.open
		if open == nil {
			open = func(bucketpg.Config) (sharedBuckets, error) { return &fakeBuckets{}, nil }
		}
		if _, err := sharedLimits(context.Background(), cfg, tc.raw, open, logf); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}

// A bad variable is a database problem: the run goes on with its own
// limit, and the warning says why.
func TestDatabaseWarningForRateLimits(t *testing.T) {
	e := newDBEnv(t)
	db := e.open(t, reviewArgs{databaseURL: "postgres://runner@127.0.0.1/buggrit", rateLimits: "not json"})
	if db == nil {
		t.Fatal("the database wasn't kept")
	}
	if w := e.warnings(); len(w) != 1 || !strings.Contains(w[0], "BUGGRIT_RATE_LIMITS: invalid character") {
		t.Errorf("warnings = %q", w)
	}
}

// A whole run whose bucket fails mid-run: the review is unchanged, and the
// warning and the job summary say what happened.
func TestReviewSharedLimitFails(t *testing.T) {
	e := setupReview(t, submit(finding))
	e.env["BUGGRIT_DATABASE_URL"] = "postgres://runner@127.0.0.1/buggrit"
	e.env["BUGGRIT_RATE_LIMITS"] = `{"luna": {"base_url": "https://llm.example/v1", "max_concurrency": 2}}`
	fs := &fakeStore{buckets: &fakeBuckets{acquireErr: errors.New("connection reset")}}
	e.deps.openStore = func(context.Context, string) (database, error) { return fs, nil }
	e.deps.clients = func(cfg *config.Config, _ modelObserver, shared *llm.Shared) (*llm.Clients, error) {
		if shared == nil {
			t.Fatal("no shared limit")
		}
		return llm.New(cfg, llm.WithShared(shared), llm.WithGetenv(func(string) string { return "key" }),
			llm.WithBase(func(*config.Model, string) (llms.ChatClient, error) { return e.model, nil }))
	}
	code, out, errOut := e.run(t)
	if code != 0 || !strings.Contains(out, "1 new finding") {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if fmt.Sprint(fs.buckets.ensured) != "[luna 2/0 true]" || !fs.closed {
		t.Errorf("ensured %v, closed %v", fs.buckets.ensured, fs.closed)
	}
	summary, err := os.ReadFile(e.summary)
	if err != nil || !strings.Contains(string(summary), "### Shared rate limit") || !strings.Contains(string(summary), `"luna" failed`) {
		t.Errorf("job summary = %q, %v", summary, err)
	}
	var warned bool
	for _, c := range e.fake.PR(7).IssueComments {
		warned = warned || strings.Contains(c.Body, "connection reset")
	}
	if !warned {
		t.Errorf("no warning on the PR")
	}
}

// Without a database the variable has no effect, and the log says so; it
// isn't a database problem, since no database is configured.
func TestRateLimitsWithoutDatabase(t *testing.T) {
	e := newDBEnv(t)
	if db := e.open(t, reviewArgs{rateLimits: `{"luna": {"base_url": "u", "max_concurrency": 1}}`}); db != nil {
		t.Fatal("opened a database")
	}
	if !strings.Contains(e.stderr.String(), "BUGGRIT_RATE_LIMITS is set, but there's no database") || len(e.warnings()) != 0 {
		t.Errorf("stderr %q, warnings %q", e.stderr.String(), e.warnings())
	}
}

// A run that stopped, perhaps because the head moved, doesn't publish the
// shared limit's problems: a newer run may own the PR (design §2).
func TestReviewSharedProblemsAfterStop(t *testing.T) {
	e := setupReview(t, submit(finding))
	e.env["BUGGRIT_DATABASE_URL"] = "postgres://runner@127.0.0.1/buggrit"
	e.env["BUGGRIT_RATE_LIMITS"] = `{"luna": {"base_url": "https://llm.example/v1", "max_concurrency": 2}}`
	fs := &fakeStore{buckets: &fakeBuckets{acquireErr: errors.New("connection reset")}}
	e.deps.openStore = func(context.Context, string) (database, error) { return fs, nil }
	e.deps.clients = func(cfg *config.Config, _ modelObserver, shared *llm.Shared) (*llm.Clients, error) {
		// The head moves while the model runs, so publishing stops.
		moving := &headMover{ChatClient: e.model, move: func() { e.fake.Push(7, "2222222222222222222222222222222222222222", diff) }}
		return llm.New(cfg, llm.WithShared(shared),
			llm.WithBase(func(*config.Model, string) (llms.ChatClient, error) { return moving, nil }))
	}
	code, out, errOut := e.run(t)
	if code != 0 || !strings.Contains(out, "nothing to do") {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if !strings.Contains(errOut, "connection reset") {
		t.Errorf("stderr = %q", errOut)
	}
	for _, c := range e.fake.PR(7).IssueComments {
		if strings.Contains(c.Body, "connection reset") {
			t.Errorf("published after stopping: %q", c.Body)
		}
	}
}

// headMover pushes to the PR when the model is first called.
type headMover struct {
	llms.ChatClient
	move func()
	once sync.Once
}

//nolint:gocritic // hugeParam: the signature is fixed by llms.ChatClient
func (h *headMover) Complete(ctx context.Context, req llms.ChatRequest) (llms.ChatResponse, error) {
	h.once.Do(h.move)
	return h.ChatClient.Complete(ctx, req) //nolint:wrapcheck // a test decorator
}
