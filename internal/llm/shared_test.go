package llm

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/bucket"
	"github.com/SnapdragonPartners/maestro-llms/bucket/memory"
	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/ratelimit"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"

	"github.com/SnapdragonPartners/buggrit/internal/config"
)

const sharedCfg = `{"version": 1, "limits": {"max_concurrency": 4}, "models": {"default": {"provider": "openai-compatible", "base_url": "https://llm.example", "model": "m"}}}`

// inFlight is a model that counts the calls running at once.
type inFlight struct {
	release chan struct{}
	now     atomic.Int32
	peak    atomic.Int32
}

func (f *inFlight) Model() llms.ModelRef {
	return llms.ModelRef{Provider: "openai-compatible", Name: "m"}
}

//nolint:gocritic // hugeParam: the signature is fixed by llms.ChatClient
func (f *inFlight) Complete(ctx context.Context, _ llms.ChatRequest) (llms.ChatResponse, error) {
	n := f.now.Add(1)
	defer f.now.Add(-1)
	for {
		p := f.peak.Load()
		if n <= p || f.peak.CompareAndSwap(p, n) {
			break
		}
	}
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			return llms.ChatResponse{}, ctx.Err()
		}
	}
	resp := testllm.TextResponse("ok")
	// Usage, so the middleware reconciles the lease's tokens.
	resp.Usage = llms.Usage{InputTokens: 1, OutputTokens: 1}
	return resp, nil
}

func sharedClients(t *testing.T, cfg *config.Config, shared *Shared, model llms.ChatClient) *Clients {
	t.Helper()
	c, err := New(cfg, WithShared(shared), WithBase(func(*config.Model, string) (llms.ChatClient, error) { return model, nil }))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func call(c llms.ChatClient) error {
	_, err := c.Complete(context.Background(), llms.ChatRequest{Messages: []llms.Message{llms.UserText("hi")}})
	return err
}

func memBucket(t *testing.T, limits bucket.Limits) *memory.Limiter {
	t.Helper()
	l, err := memory.New(memory.Config{Limits: map[string]bucket.Limits{"luna": limits}, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// The shared limit is smaller than the run's own, so it's what bounds the
// stages, together.
func TestSharedBoundsEveryStage(t *testing.T) {
	cfg := parse(t, sharedCfg)
	b := memBucket(t, bucket.Limits{MaxConcurrency: 1})
	model := &inFlight{release: make(chan struct{})}
	c := sharedClients(t, cfg, &Shared{Limiter: b, Stages: map[config.Stage]SharedStage{
		config.StageFind: {Bucket: "luna"}, config.StageVerify: {Bucket: "luna"},
	}}, model)

	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := call(c.For([]config.Stage{config.StageFind, config.StageVerify}[i%2])); err != nil {
				t.Error(err)
			}
		}()
	}
	for range 4 {
		model.release <- struct{}{}
	}
	wg.Wait()
	if p := model.peak.Load(); p != 1 {
		t.Errorf("peak in flight = %d, want 1", p)
	}
	if _, leases := c.Shared.Waited(); leases != 4 {
		t.Errorf("leases = %d, want 4", leases)
	}
	if p := c.Shared.Problems(); len(p) != 0 {
		t.Errorf("problems = %q", p)
	}
}

// A stage without a bucket has only the run's own limit.
func TestSharedOnlyListedStages(t *testing.T) {
	cfg := parse(t, sharedCfg)
	failing := &fakeBucket{err: errors.New("unused")}
	c := sharedClients(t, cfg, &Shared{Limiter: failing, Stages: map[config.Stage]SharedStage{config.StageFind: {Bucket: "luna"}}}, &inFlight{})
	if err := call(c.Verify); err != nil {
		t.Fatal(err)
	}
	if failing.calls.Load() != 0 {
		t.Errorf("verify asked the bucket")
	}
	if c := sharedClients(t, cfg, nil, &inFlight{}); c.Shared != nil {
		t.Error("Shared state without a shared limit")
	}
}

// fakeBucket fails with err when it's set, and otherwise grants lease. A
// test changes err between calls, which run on its own goroutine.
type fakeBucket struct {
	err   error
	lease *fakeLease
	calls atomic.Int32
	// entered and proceed, when set, hold Acquire until the test lets it
	// go, after saying it has started.
	entered, proceed chan struct{}
	// untilDone makes Acquire wait for its context to end, then fail with
	// err.
	untilDone bool
}

func (f *fakeBucket) Acquire(ctx context.Context, _ string, _ int) (bucket.Lease, error) {
	f.calls.Add(1)
	if f.entered != nil {
		f.entered <- struct{}{}
		<-f.proceed
	}
	if f.untilDone {
		<-ctx.Done()
	}
	if f.err != nil {
		return nil, f.err
	}
	if f.lease == nil {
		return &fakeLease{}, nil
	}
	return f.lease, nil
}

type fakeLease struct{ adjustErr, releaseErr error }

func (l *fakeLease) Adjust(context.Context, int) error { return l.adjustErr }
func (l *fakeLease) Release(context.Context) error     { return l.releaseErr }
func (*fakeLease) Lost() <-chan struct{}               { return nil }

// fakeClock is the shared state's clock, moved by hand.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// breakerClients is sharedClients over b for the find stage, on a fake
// clock.
func breakerClients(t *testing.T, b bucket.Limiter, maxWait time.Duration) (*Clients, *fakeClock) {
	t.Helper()
	c := sharedClients(t, parse(t, sharedCfg), &Shared{Limiter: b, Stages: map[config.Stage]SharedStage{config.StageFind: {Bucket: "luna", MaxWait: maxWait}}}, &inFlight{})
	clk := &fakeClock{t: time.Date(2026, 11, 23, 21, 0, 0, 0, time.UTC)}
	c.Shared.now = clk.now
	return c, clk
}

// calls makes n find calls, each of which must succeed.
func calls(t *testing.T, c *Clients, n int) {
	t.Helper()
	for range n {
		if err := call(c.Find); err != nil {
			t.Fatal(err)
		}
	}
}

// asked checks how many times the bucket has been asked so far.
func asked(t *testing.T, b *fakeBucket, want int32) {
	t.Helper()
	if n := b.calls.Load(); n != want {
		t.Errorf("bucket asked %d times, want %d", n, want)
	}
}

// oneProblem checks there's one problem, containing every part.
func oneProblem(t *testing.T, c *Clients, parts ...string) {
	t.Helper()
	p := c.Shared.Problems()
	if len(p) != 1 {
		t.Fatalf("problems = %q, want one", p)
	}
	for _, part := range parts {
		if !strings.Contains(p[0], part) {
			t.Errorf("problem %q doesn't contain %q", p[0], part)
		}
	}
}

// One backend failure is a blip: that call goes ahead on the run's own
// limit, and the next call asks the bucket again.
func TestSharedBlip(t *testing.T) {
	b := &fakeBucket{err: errors.New("set lock_timeout: timeout: context deadline exceeded")}
	c, _ := breakerClients(t, b, 0)
	calls(t, c, 1)
	b.err = nil
	calls(t, c, 2)
	asked(t, b, 3)
	if _, leases := c.Shared.Waited(); leases != 2 {
		t.Errorf("%d leases, want 2", leases)
	}
	oneProblem(t, c, "failed 1 time", `"luna": ratelimit: bucket: set lock_timeout`, "each call it failed went ahead on the run's own limit")
}

// Failures that aren't in a row don't open the breaker.
func TestSharedFailuresNotInARow(t *testing.T) {
	b := &fakeBucket{}
	c, _ := breakerClients(t, b, 0)
	for range breakerThreshold {
		b.err = errors.New("connection refused")
		calls(t, c, breakerThreshold-1)
		b.err = nil
		calls(t, c, 1)
	}
	calls(t, c, 1)
	asked(t, b, 3*breakerThreshold+1)
	oneProblem(t, c, "failed 6 times", "each call it failed")
}

// breakerThreshold failures in a row open the breaker: calls skip the
// bucket until the cooldown is over. Then one trial call asks it, and when
// that works the run uses the bucket again.
func TestSharedBreakerOpensAndCloses(t *testing.T) {
	b := &fakeBucket{err: errors.New("connection refused")}
	c, clk := breakerClients(t, b, 0)
	calls(t, c, breakerThreshold+2)
	asked(t, b, breakerThreshold)

	clk.advance(breakerCooldown - time.Second)
	calls(t, c, 1)
	asked(t, b, breakerThreshold)

	clk.advance(time.Second)
	b.err = nil
	calls(t, c, 3)
	asked(t, b, breakerThreshold+3)
	oneProblem(t, c, "failed 3 times", "switched this run to its own limit 1 time, for 1m0s in all", "using the shared limit again by the end")

	// Closing resets it: it takes a full streak to open again, and the
	// cooldown starts over.
	b.err = errors.New("connection refused")
	calls(t, c, breakerThreshold+1)
	asked(t, b, 2*breakerThreshold+3)
	clk.advance(breakerCooldown)
	calls(t, c, 1)
	asked(t, b, 2*breakerThreshold+4)
}

// A failed trial reopens the breaker for twice as long, up to
// breakerMaxCooldown.
func TestSharedBreakerBacksOff(t *testing.T) {
	b := &fakeBucket{err: errors.New("connection refused")}
	c, clk := breakerClients(t, b, 0)
	calls(t, c, breakerThreshold)
	n := int32(breakerThreshold)
	clk.advance(breakerCooldown)
	open := breakerCooldown
	for _, minutes := range []time.Duration{2, 4, 8, 8} {
		calls(t, c, 2) // the trial fails, and the second call skips
		n++
		asked(t, b, n)
		cooldown := minutes * time.Minute
		clk.advance(cooldown - time.Second)
		calls(t, c, 1)
		asked(t, b, n)
		clk.advance(time.Second)
		open += cooldown
	}
	oneProblem(t, c, "failed 7 times", "1 time, for "+open.String(), "still on its own limit at the end")
}

// While the trial call waits for its lease, other calls skip the bucket
// rather than piling onto a backend that may still be down.
func TestSharedOneTrialAtATime(t *testing.T) {
	b := &fakeBucket{err: errors.New("connection refused")}
	c, clk := breakerClients(t, b, 0)
	calls(t, c, breakerThreshold)
	clk.advance(breakerCooldown)

	b.err = nil
	b.entered, b.proceed = make(chan struct{}), make(chan struct{})
	done := make(chan error)
	go func() { done <- call(c.Find) }()
	<-b.entered
	// A call that wrongly asked the bucket now would fail the count below
	// instead of blocking the test.
	b.entered = nil
	calls(t, c, 2)
	asked(t, b, breakerThreshold+1)
	close(b.proceed)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	calls(t, c, 1)
	asked(t, b, breakerThreshold+2)
}

// A trial call that ends for its own reasons doesn't decide anything: the
// next call is the trial.
func TestSharedCanceledTrial(t *testing.T) {
	b := &fakeBucket{err: errors.New("connection refused")}
	c, clk := breakerClients(t, b, 0)
	calls(t, c, breakerThreshold)
	clk.advance(breakerCooldown)

	b.untilDone = true
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := c.Find.Complete(ctx, llms.ChatRequest{Messages: []llms.Message{llms.UserText("hi")}}); err == nil {
		t.Fatal("canceled trial succeeded")
	}
	b.untilDone, b.err = false, nil
	calls(t, c, 2)
	asked(t, b, breakerThreshold+3)
	oneProblem(t, c, "failed 3 times", "using the shared limit again by the end")
}

// A bucket that can never admit the run's calls is given up for the rest
// of the run; the breaker doesn't keep trying it.
func TestSharedPermanentRefusal(t *testing.T) {
	b := &fakeBucket{err: &bucket.LimitError{Key: "luna", Reason: "tokens exceed capacity", Permanent: true}}
	c, clk := breakerClients(t, b, 0)
	calls(t, c, 2)
	clk.advance(time.Hour)
	calls(t, c, 1)
	asked(t, b, 1)
	oneProblem(t, c, `"luna" can never admit`, "tokens exceed capacity")
}

// A refusal on the breaker's trial call gives the bucket up like any other:
// no later trial asks it, and the warning says why. Each kind of refusal:
// a permanent one, and max_wait running out.
func TestSharedRefusalOnTrial(t *testing.T) {
	for name, tc := range map[string]struct {
		err       error
		untilDone bool
		want      string
	}{
		"permanent": {err: &bucket.LimitError{Key: "luna", Reason: "tokens exceed capacity", Permanent: true}, want: `"luna" can never admit`},
		"max_wait":  {err: &bucket.LimitError{Key: "luna", Reason: "full"}, untilDone: true, want: "no lease within max_wait (10ms)"},
	} {
		t.Run(name, func(t *testing.T) {
			b := &fakeBucket{err: errors.New("connection refused")}
			c, clk := breakerClients(t, b, 10*time.Millisecond)
			calls(t, c, breakerThreshold)
			clk.advance(breakerCooldown)
			b.err, b.untilDone = tc.err, tc.untilDone
			calls(t, c, 1)
			asked(t, b, breakerThreshold+1)

			clk.advance(time.Hour)
			calls(t, c, 3)
			asked(t, b, breakerThreshold+1)
			p := c.Shared.Problems()
			if len(p) != 2 || !strings.Contains(p[0], "it was on its own limit at the end") || !strings.Contains(p[1], tc.want) {
				t.Errorf("problems = %q", p)
			}
		})
	}
}

// A bucket given up on after the breaker closed: the breaker's line
// doesn't claim the run was back on the shared limit.
func TestSharedGiveUpAfterBreakerClosed(t *testing.T) {
	b := &fakeBucket{err: errors.New("connection refused")}
	c, clk := breakerClients(t, b, 0)
	calls(t, c, breakerThreshold)
	clk.advance(breakerCooldown)
	b.err = nil
	calls(t, c, 1)
	b.err = &bucket.LimitError{Key: "luna", Reason: "tokens exceed capacity", Permanent: true}
	calls(t, c, 2)
	asked(t, b, breakerThreshold+2)
	p := c.Shared.Problems()
	if len(p) != 2 || !strings.Contains(p[0], "for 1m0s in all, and it was on its own limit at the end") || !strings.Contains(p[1], "can never admit") {
		t.Errorf("problems = %q", p)
	}
}

// An error that isn't a refusal is the backend's even when it arrives as
// max_wait ends (maestro-llms reports a commit whose outcome is unknown
// that way): it counts toward the breaker instead of giving the bucket up.
func TestSharedBackendErrorAtMaxWait(t *testing.T) {
	b := &fakeBucket{err: errors.New("bucket: commit: conn closed"), untilDone: true}
	c, _ := breakerClients(t, b, 10*time.Millisecond)
	calls(t, c, 1)
	b.untilDone, b.err = false, nil
	calls(t, c, 1)
	asked(t, b, 2)
	oneProblem(t, c, "failed 1 time", "commit: conn closed")
}

// A lease that doesn't come within max_wait does the same, so a run never
// hangs on the bucket.
func TestSharedMaxWaitFallsBack(t *testing.T) {
	cfg := parse(t, sharedCfg)
	b := memBucket(t, bucket.Limits{MaxConcurrency: 1})
	held, err := b.Acquire(context.Background(), "luna", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Release(context.Background()) }()
	c := sharedClients(t, cfg, &Shared{Limiter: b, Stages: map[config.Stage]SharedStage{config.StageFind: {Bucket: "luna", MaxWait: 20 * time.Millisecond}}}, &inFlight{})
	if err := call(c.Find); err != nil {
		t.Fatal(err)
	}
	p := c.Shared.Problems()
	if len(p) != 1 || !strings.Contains(p[0], "no lease within max_wait (20ms)") {
		t.Errorf("problems = %q", p)
	}
	if waited, leases := c.Shared.Waited(); waited < 20*time.Millisecond || leases != 0 {
		t.Errorf("waited %s for %d leases", waited, leases)
	}
}

// A call whose own context ends while it waits fails as it would without
// the bucket, gives back its slot of the run's limit, and isn't a problem
// with the bucket.
func TestSharedCanceledWait(t *testing.T) {
	cfg := parse(t, sharedCfg)
	b := memBucket(t, bucket.Limits{MaxConcurrency: 1})
	held, _ := b.Acquire(context.Background(), "luna", 0)
	defer func() { _ = held.Release(context.Background()) }()
	c := sharedClients(t, cfg, &Shared{Limiter: b, Stages: map[config.Stage]SharedStage{config.StageFind: {Bucket: "luna"}}}, &inFlight{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.Find.Complete(ctx, llms.ChatRequest{Messages: []llms.Message{llms.UserText("hi")}}); err == nil {
		t.Fatal("call succeeded without a lease")
	}
	if p := c.Shared.Problems(); len(p) != 0 {
		t.Errorf("problems = %q", p)
	}
	// All four of the run's slots are free again.
	for range 4 {
		r, err := c.Limiter.Reserve(context.Background(), reservation())
		if err != nil {
			t.Fatalf("run slot leaked: %v", err)
		}
		defer func() { _ = r.Release(context.Background()) }()
	}
}

// A lease granted before the breaker opened, failing on release while it's
// open, is counted and reported but changes nothing: the breaker stays open
// for its first cooldown, and it's still the first time it opened.
func TestSharedReleaseFailsWhileOpen(t *testing.T) {
	ctx := context.Background()
	clk := &fakeClock{t: time.Date(2026, 11, 23, 21, 0, 0, 0, time.UTC)}
	state := &SharedState{now: clk.now}
	local, err := ratelimit.NewInMemoryLimiter(ratelimit.Config{MaxConcurrency: 8})
	if err != nil {
		t.Fatal(err)
	}
	b := &fakeBucket{lease: &fakeLease{releaseErr: errors.New("release: timeout")}}
	l := newSharedLimiter(local, b, SharedStage{Bucket: "luna"}, state)
	reserve := func() ratelimit.Reservation {
		t.Helper()
		r, err := l.Reserve(ctx, reservation())
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	held := reserve()

	b.err = errors.New("connection refused")
	for range breakerThreshold {
		_ = reserve().Release(ctx)
	}
	asked(t, b, breakerThreshold+1)

	clk.advance(breakerCooldown / 2)
	if err := held.Release(ctx); err == nil || !strings.Contains(err.Error(), "release: timeout") {
		t.Fatalf("release = %v", err)
	}
	_ = reserve().Release(ctx)
	asked(t, b, breakerThreshold+1)

	clk.advance(breakerCooldown / 2)
	_ = reserve().Release(ctx)
	asked(t, b, breakerThreshold+2)
	p := state.Problems()
	if len(p) != 1 || !strings.Contains(p[0], "failed 5 times") || !strings.Contains(p[0], "its own limit 1 time") {
		t.Errorf("problems = %q", p)
	}
}

// A lease lost while held is reported, and the run goes on using the
// bucket.
func TestSharedLostLease(t *testing.T) {
	cfg := parse(t, sharedCfg)
	b := &fakeBucket{lease: &fakeLease{releaseErr: bucket.ErrLeaseLost}}
	c := sharedClients(t, cfg, &Shared{Limiter: b, Stages: map[config.Stage]SharedStage{config.StageFind: {Bucket: "luna"}}}, &inFlight{})
	for range 2 {
		if err := call(c.Find); err != nil {
			t.Fatal(err)
		}
	}
	if n := b.calls.Load(); n != 2 {
		t.Errorf("bucket asked %d times, want 2", n)
	}
	p := c.Shared.Problems()
	if len(p) != 1 || !strings.Contains(p[0], "a lease expired while a call held it") {
		t.Errorf("problems = %q", p)
	}
}

func reservation() ratelimit.ReservationRequest {
	return ratelimit.ReservationRequest{EstimatedUnits: ratelimit.UsageUnits{Requests: 1}}
}

// A backend failure after the lease was granted, while reconciling tokens
// or releasing, is counted and reported, although the middleware discards
// the error. A call whose commit and release both fail counts once. The
// next granted lease ends the streak, so these alone never open the
// breaker.
func TestSharedLeaseBackendFailure(t *testing.T) {
	for name, tc := range map[string]struct {
		lease *fakeLease
		want  string
	}{
		"adjust":  {&fakeLease{adjustErr: errors.New("adjust: timeout")}, "adjust: timeout"},
		"release": {&fakeLease{releaseErr: errors.New("release: timeout")}, "release: timeout"},
		"both":    {&fakeLease{adjustErr: errors.New("adjust: timeout"), releaseErr: errors.New("release: timeout")}, "adjust: timeout"},
	} {
		t.Run(name, func(t *testing.T) {
			b := &fakeBucket{lease: tc.lease}
			c, _ := breakerClients(t, b, 0)
			calls(t, c, 1)
			oneProblem(t, c, "failed 1 time", tc.want, "each call it failed")
			// Every acquire succeeds and closes the streak, so the breaker
			// never opens: the run keeps using the bucket.
			calls(t, c, breakerThreshold)
			asked(t, b, breakerThreshold+1)
		})
	}
}
