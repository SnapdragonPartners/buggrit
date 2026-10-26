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

type fakeBucket struct {
	err   error
	lease *fakeLease
	calls atomic.Int32
}

func (f *fakeBucket) Acquire(context.Context, string, int) (bucket.Lease, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	return f.lease, nil
}

type fakeLease struct{ adjustErr, releaseErr error }

func (l *fakeLease) Adjust(context.Context, int) error { return l.adjustErr }
func (l *fakeLease) Release(context.Context) error     { return l.releaseErr }
func (*fakeLease) Lost() <-chan struct{}               { return nil }

// A failing backend switches the run to its own limit, for good: the call
// goes ahead, and later calls don't ask the bucket.
func TestSharedBackendFailureFallsBack(t *testing.T) {
	cfg := parse(t, sharedCfg)
	b := &fakeBucket{err: errors.New("dial tcp: connection refused")}
	c := sharedClients(t, cfg, &Shared{Limiter: b, Stages: map[config.Stage]SharedStage{config.StageFind: {Bucket: "luna"}}}, &inFlight{})
	for range 3 {
		if err := call(c.Find); err != nil {
			t.Fatal(err)
		}
	}
	if n := b.calls.Load(); n != 1 {
		t.Errorf("bucket asked %d times, want 1", n)
	}
	p := c.Shared.Problems()
	if len(p) != 1 || !strings.Contains(p[0], `"luna" failed`) || !strings.Contains(p[0], "connection refused") {
		t.Errorf("problems = %q", p)
	}
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
// or releasing, is a failure like one while acquiring, although the
// middleware discards the error: the run switches to its own limit.
func TestSharedLeaseBackendFailure(t *testing.T) {
	cfg := parse(t, sharedCfg)
	for name, lease := range map[string]*fakeLease{
		"adjust":  {adjustErr: errors.New("adjust: timeout")},
		"release": {releaseErr: errors.New("release: timeout")},
	} {
		b := &fakeBucket{lease: lease}
		c := sharedClients(t, cfg, &Shared{Limiter: b, Stages: map[config.Stage]SharedStage{config.StageFind: {Bucket: "luna"}}}, &inFlight{})
		for range 2 {
			if err := call(c.Find); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
		if n := b.calls.Load(); n != 1 {
			t.Errorf("%s: bucket asked %d times, want 1", name, n)
		}
		if p := c.Shared.Problems(); len(p) != 1 || !strings.Contains(p[0], name+": timeout") {
			t.Errorf("%s: problems = %q", name, p)
		}
	}
}
