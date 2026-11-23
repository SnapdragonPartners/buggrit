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
// bucket. A call the bucket can't serve goes ahead on the run's own limit.
// A backend failure counts toward a circuit breaker, which rests the
// bucket and tries it again later; a bucket that can't admit a call within
// MaxWait is given up for the rest of the run. SharedState.Problems says
// what happened.
func WithShared(s *Shared) Option { return func(o *options) { o.shared = s } }

// The circuit breaker over the shared limit's backend (design §9). One
// failure is a blip, covered by the run's own limit for that call.
// breakerThreshold failures in a row open the breaker: calls skip the
// bucket for a cooldown that starts at breakerCooldown and doubles, up to
// breakerMaxCooldown, each time the trial call after it fails.
const (
	breakerThreshold   = 3
	breakerCooldown    = time.Minute
	breakerMaxCooldown = 8 * time.Minute
)

// SharedState is what happened with the shared rate limit during a run,
// and the circuit breaker over its backend. It's shared by every stage,
// since every bucket lives in the one backend.
type SharedState struct {
	// The breaker is closed while openUntil is zero. probing is set while
	// one call tries the bucket after the cooldown; openedAt, openFor,
	// opened, failures and lastErr are what it did, for Problems.
	openUntil time.Time
	openedAt  time.Time
	now       func() time.Time
	lastErr   string
	problems  []string
	mu        sync.Mutex
	waited    time.Duration
	cooldown  time.Duration
	openFor   time.Duration
	leases    int
	streak    int
	failures  int
	opened    int
	probing   bool
	// done is set when a bucket can't admit a call (max_wait ran out, or
	// the call can never fit): the run uses only its own limit from then on.
	done bool
}

// Problems says why the shared limit wasn't used for all of the run: the
// backend's failures and what the breaker did about them, a bucket given
// up on, and any lease that was lost while held. It's empty when nothing
// went wrong.
func (s *SharedState) Problems() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	if b := s.breakerProblem(); b != "" {
		out = append(out, b)
	}
	return append(out, s.problems...)
}

// breakerProblem sums up the backend's failures in one line, however many
// there were.
func (s *SharedState) breakerProblem() string {
	if s.failures == 0 {
		return ""
	}
	failed := fmt.Sprintf("the shared rate limit failed %d %s (last: %s)", s.failures, plural(s.failures, "time", "times"), s.lastErr)
	if s.opened == 0 {
		return failed + "; each call it failed went ahead on the run's own limit"
	}
	off := s.openFor
	end := "it was using the shared limit again by the end"
	if !s.openUntil.IsZero() {
		off += s.now().Sub(s.openedAt)
		end = "it was still on its own limit at the end"
	}
	return fmt.Sprintf("%s; %d failures in a row switched this run to its own limit %d %s, for %s in all, and %s",
		failed, breakerThreshold, s.opened, plural(s.opened, "time", "times"), off.Round(time.Second), end)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// Waited is the total time spent waiting for leases, and how many were
// granted.
func (s *SharedState) Waited() (time.Duration, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.waited, s.leases
}

// admit says whether a call should ask the bucket, and whether it's the
// breaker's trial call. While the breaker is open, no call asks. Once the
// cooldown is over, the first call to come is the trial, and the others
// skip the bucket until it's decided.
func (s *SharedState) admit() (use, probe bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.done:
		return false, false
	case s.openUntil.IsZero():
		return true, false
	case s.probing || s.now().Before(s.openUntil):
		return false, false
	}
	s.probing = true
	return true, true
}

func (s *SharedState) record(wait time.Duration, granted bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.waited += wait
	if granted {
		s.leases++
	}
}

// succeeded is a lease granted: the backend works, so the breaker closes.
func (s *SharedState) succeeded(probe bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if probe {
		s.probing = false
	}
	s.streak = 0
	if !s.openUntil.IsZero() {
		s.openFor += s.now().Sub(s.openedAt)
		s.openUntil = time.Time{}
		s.cooldown = 0
	}
}

// abandoned is a trial call that ended without saying whether the backend
// works: the next call is the trial instead.
func (s *SharedState) abandoned(probe bool) {
	if !probe {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.probing = false
}

// failed is a backend failure, while acquiring or after. A failed trial
// reopens the breaker for twice as long; a closed breaker opens after
// breakerThreshold failures in a row. A failure while the breaker is
// already open (a lease granted before it opened, failing on release) only
// counts.
func (s *SharedState) failed(probe bool, bucketName string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures++
	s.lastErr = fmt.Sprintf("%q: %v", bucketName, err)
	now := s.now()
	open := !s.openUntil.IsZero()
	if probe {
		s.probing = false
	}
	switch {
	case probe && open:
		s.cooldown = min(2*s.cooldown, breakerMaxCooldown)
		s.openUntil = now.Add(s.cooldown)
	case open:
	default:
		// A trial whose breaker another call's lease closed while it was
		// out lands here too, and counts like any other failure.
		s.streak++
		if s.streak >= breakerThreshold {
			s.streak = 0
			s.opened++
			s.openedAt = now
			s.cooldown = breakerCooldown
			s.openUntil = now.Add(s.cooldown)
		}
	}
}

// giveUp switches the run to its own limit for good, because a bucket
// can't admit its calls. Only the first reason is kept: once the run has
// stopped using the bucket, later calls don't ask it.
func (s *SharedState) giveUp(problem string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.done {
		s.done = true
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
	use, probe := l.state.admit()
	if !use {
		return own, nil
	}
	// The wait starts before its timeout's timer does, so a wait cut off by
	// max_wait is never recorded as shorter than it.
	start := l.state.now()
	wctx, cancel := ctx, context.CancelFunc(func() {})
	if l.maxWait > 0 {
		wctx, cancel = context.WithTimeout(ctx, l.maxWait)
	}
	lease, err := l.shared.Reserve(wctx, req)
	waited := l.state.now().Sub(start)
	timedOut := wctx.Err() != nil
	cancel()
	l.state.record(waited, err == nil)
	if err == nil {
		l.state.succeeded(probe)
		return &sharedReservation{own: own, lease: lease, limiter: l}, nil
	}
	if ctx.Err() != nil {
		// The call itself is over; that's not the bucket's fault.
		l.state.abandoned(probe)
		_ = own.Release(context.WithoutCancel(ctx))
		return nil, err //nolint:wrapcheck // keep *llms.LimitError for the retry and circuit middleware
	}
	// A refusal is the bucket working: it's full until max_wait, or the
	// call can never fit. Any other error is the backend's, even one that
	// arrives as the wait ends; maestro-llms reports a commit whose outcome
	// is unknown that way, not as a refusal.
	var refused *llms.LimitError
	switch {
	case errors.As(err, &refused) && timedOut:
		l.state.abandoned(probe)
		l.state.giveUp(fmt.Sprintf("the shared rate limit %q: no lease within max_wait (%s), so this run used only its own limit from then on", l.bucket, l.maxWait))
	case errors.As(err, &refused) && refused.Permanent:
		l.state.abandoned(probe)
		l.state.giveUp(fmt.Sprintf("the shared rate limit %q can never admit this run's calls, so this run used only its own limit from then on: %v", l.bucket, err))
	default:
		l.state.failed(probe, l.bucket, err)
	}
	return own, nil
}

type sharedReservation struct {
	own, lease ratelimit.Reservation
	limiter    *sharedLimiter
	// failed is set once this lease's backend has failed, so a call whose
	// commit and release both fail counts once toward the breaker.
	failed bool
}

// Commit implements ratelimit.Reservation. The middleware discards its
// error, so a backend failure here counts toward the breaker, as one while
// acquiring does.
//
//nolint:gocritic // hugeParam: the signature is fixed by ratelimit.Reservation
func (r *sharedReservation) Commit(ctx context.Context, usage llms.Usage) error {
	err := r.lease.Commit(ctx, usage)
	if err != nil {
		r.fail(err)
	}
	return errors.Join(err, r.own.Commit(ctx, usage)) //nolint:wrapcheck // each limiter's error, as is
}

// Release implements ratelimit.Reservation. A lease that was lost while
// held (its renewal failed, so another holder may have had the slot) is
// reported, and the run goes on using the bucket. Any other failure is the
// backend's, and counts toward the breaker; the middleware discards the
// error, so this is the only place it's seen.
func (r *sharedReservation) Release(ctx context.Context) error {
	err := r.lease.Release(ctx)
	switch {
	case errors.Is(err, bucket.ErrLeaseLost):
		r.limiter.state.lost(fmt.Sprintf("the shared rate limit %q: a lease expired while a call held it, so the limit may have been exceeded", r.limiter.bucket))
	case err != nil:
		r.fail(err)
	}
	return errors.Join(err, r.own.Release(ctx)) //nolint:wrapcheck // each limiter's error, as is; ErrLeaseLost stays matchable
}

func (r *sharedReservation) fail(err error) {
	if r.failed {
		return
	}
	r.failed = true
	r.limiter.state.failed(false, r.limiter.bucket, err)
}
