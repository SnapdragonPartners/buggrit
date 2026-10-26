package llm

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/bucket"
	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/ratelimit"

	"github.com/SnapdragonPartners/buggrit/internal/config"
)

// Shared is a rate limit shared across runs and repositories (design §9),
// such as maestro-llms' bucket/postgres. Its buckets must already exist
// with their limits; New only acquires leases.
type Shared struct {
	// Limiter holds the buckets.
	Limiter bucket.Limiter
	// Stages maps each stage that shares a limit to its bucket. A stage
	// that isn't listed has only the run's own limit.
	Stages map[config.Stage]SharedStage
}

// SharedStage is one stage's shared bucket.
type SharedStage struct {
	// Bucket is the bucket's name.
	Bucket string
	// MaxWait bounds the wait for a lease; 0 waits as long as the call's
	// context allows.
	MaxWait time.Duration
}

// WithShared adds the shared rate limit to the stages s lists. Each call
// then holds a slot of the run's own limit and a lease on its stage's
// bucket. When the bucket can't be used, because its backend fails or a
// lease takes longer than MaxWait, the run goes on with only its own limit,
// and SharedState.Problems says why.
func WithShared(s *Shared) Option { return func(o *options) { o.shared = s } }

// SharedState is what happened with the shared rate limit during a run.
// It's shared by every stage.
type SharedState struct {
	now      func() time.Time
	problems []string
	waited   time.Duration
	leases   int
	mu       sync.Mutex
	failed   bool
}

// Problems says why the shared limit wasn't used for all of the run: the
// failure that switched the run to its own limit, and any lease that was
// lost while held. It's empty when nothing went wrong.
func (s *SharedState) Problems() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.problems...)
}

// Waited is the total time spent waiting for leases, and how many were
// granted.
func (s *SharedState) Waited() (time.Duration, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.waited, s.leases
}

func (s *SharedState) active() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.failed
}

func (s *SharedState) record(wait time.Duration, granted bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.waited += wait
	if granted {
		s.leases++
	}
}

// fail switches the run to its own limit. Only the first failure is kept:
// once the run has stopped using the bucket, later calls don't ask it.
func (s *SharedState) fail(problem string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.failed {
		s.failed = true
		s.problems = append(s.problems, problem)
	}
}

func (s *SharedState) lost(problem string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.problems {
		if p == problem {
			return
		}
	}
	s.problems = append(s.problems, problem)
}

// sharedLimiter takes a slot of the run's own limit, then a lease on the
// stage's bucket. Taking the run's slot first means a call never holds a
// shared slot, which other repositories are waiting for, while it waits
// for one of its own.
type sharedLimiter struct {
	local   ratelimit.Limiter
	shared  ratelimit.Limiter
	state   *SharedState
	bucket  string
	maxWait time.Duration
}

func newSharedLimiter(local ratelimit.Limiter, b bucket.Limiter, st SharedStage, state *SharedState) *sharedLimiter {
	name := st.Bucket
	return &sharedLimiter{
		local:   local,
		shared:  ratelimit.FromBucket(b, func(ratelimit.ReservationRequest) string { return name }),
		state:   state,
		bucket:  name,
		maxWait: st.MaxWait,
	}
}

// Reserve implements ratelimit.Limiter.
//
//nolint:gocritic // hugeParam: the signature is fixed by ratelimit.Limiter
func (l *sharedLimiter) Reserve(ctx context.Context, req ratelimit.ReservationRequest) (ratelimit.Reservation, error) {
	own, err := l.local.Reserve(ctx, req)
	if err != nil {
		return nil, err //nolint:wrapcheck // keep *llms.LimitError for the retry and circuit middleware
	}
	if !l.state.active() {
		return own, nil
	}
	wctx, cancel := ctx, context.CancelFunc(func() {})
	if l.maxWait > 0 {
		wctx, cancel = context.WithTimeout(ctx, l.maxWait)
	}
	start := l.state.now()
	lease, err := l.shared.Reserve(wctx, req)
	waited := l.state.now().Sub(start)
	timedOut := wctx.Err() != nil
	cancel()
	l.state.record(waited, err == nil)
	if err == nil {
		return &sharedReservation{own: own, lease: lease, limiter: l}, nil
	}
	if ctx.Err() != nil {
		// The call itself is over; that's not the bucket's fault.
		_ = own.Release(context.WithoutCancel(ctx))
		return nil, err //nolint:wrapcheck // keep *llms.LimitError for the retry and circuit middleware
	}
	if timedOut {
		l.state.fail(fmt.Sprintf("the shared rate limit %q: no lease within max_wait (%s), so this run used only its own limit from then on", l.bucket, l.maxWait))
	} else {
		l.failed(err)
	}
	return own, nil
}

type sharedReservation struct {
	own, lease ratelimit.Reservation
	limiter    *sharedLimiter
}

// Commit implements ratelimit.Reservation. The middleware discards its
// error, so a backend failure here switches the run to its own limit, as
// one while acquiring does.
//
//nolint:gocritic // hugeParam: the signature is fixed by ratelimit.Reservation
func (r *sharedReservation) Commit(ctx context.Context, usage llms.Usage) error {
	err := r.lease.Commit(ctx, usage)
	if err != nil {
		r.limiter.failed(err)
	}
	return errors.Join(err, r.own.Commit(ctx, usage)) //nolint:wrapcheck // each limiter's error, as is
}

// Release implements ratelimit.Reservation. A lease that was lost while
// held (its renewal failed, so another holder may have had the slot) is
// reported, and the run goes on using the bucket. Any other failure is the
// backend's, and switches the run to its own limit; the middleware
// discards the error, so this is the only place it's seen.
func (r *sharedReservation) Release(ctx context.Context) error {
	err := r.lease.Release(ctx)
	switch {
	case errors.Is(err, bucket.ErrLeaseLost):
		r.limiter.state.lost(fmt.Sprintf("the shared rate limit %q: a lease expired while a call held it, so the limit may have been exceeded", r.limiter.bucket))
	case err != nil:
		r.limiter.failed(err)
	}
	return errors.Join(err, r.own.Release(ctx)) //nolint:wrapcheck // each limiter's error, as is; ErrLeaseLost stays matchable
}

// failed switches the run to its own limit after a backend failure.
func (l *sharedLimiter) failed(err error) {
	l.state.fail(fmt.Sprintf("the shared rate limit %q failed, so this run used only its own limit from then on: %v", l.bucket, err))
}
