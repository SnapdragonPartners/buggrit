package telemetry

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Run is what a run's row records when it starts.
type Run struct {
	StartedAt time.Time
	Meta      any
	ID        string
	Repo      string
	HeadSHA   string
	Label     string
	PR        int
	DryRun    bool
}

// End is what a run's row records when it ends. Complete is set when every
// emitted event was written.
type End struct {
	EndedAt  time.Time
	Outcome  any
	Emitted  int
	Written  int
	Complete bool
}

// Sink is where a Writer sends a run's events: the database, through
// internal/store. StartRun is idempotent, and WriteEvents skips events it
// already has, by run and Seq, so a retried write is harmless.
type Sink interface {
	StartRun(ctx context.Context, run *Run) error
	WriteEvents(ctx context.Context, runID string, events []*Event) error
	FinishRun(ctx context.Context, runID string, end *End) error
}

// Writer batches a run's events to a Sink in the background, so recording
// never waits on the database. There is no spool: an event that can't be
// written, because the buffer is full or the sink failed, is counted and
// dropped, and the run's row says it's incomplete (docs/design.md §6).
type Writer struct {
	sink Sink
	err  error // the first failure; the worker's until done closes
	ch   chan *Event
	done chan struct{}
	run  *Run
	opts WriterOptions
	mu   sync.RWMutex
	// emitted and dropped are guarded by mu; written is the worker's.
	emitted, dropped, written int
	closed                    bool
}

// WriterOptions tune a Writer; the zero value takes the defaults.
type WriterOptions struct {
	// Buffer is how many events wait for the worker before more are
	// dropped.
	Buffer int
	// MaxBatch and MaxBatchBytes bound one write.
	MaxBatch, MaxBatchBytes int
	// FlushEvery is how long an event waits for a full batch.
	FlushEvery time.Duration
	// OpTimeout bounds one sink call; a failed call is tried once more
	// after RetryAfter.
	OpTimeout, RetryAfter time.Duration
}

func (o *WriterOptions) defaults() {
	if o.Buffer <= 0 {
		o.Buffer = 10000
	}
	if o.MaxBatch <= 0 {
		o.MaxBatch = 500
	}
	if o.MaxBatchBytes <= 0 {
		o.MaxBatchBytes = 8 << 20
	}
	if o.FlushEvery <= 0 {
		o.FlushEvery = 2 * time.Second
	}
	if o.OpTimeout <= 0 {
		o.OpTimeout = 30 * time.Second
	}
	if o.RetryAfter <= 0 {
		o.RetryAfter = time.Second
	}
}

// NewWriter starts writing run to sink. The run's row is written first, so
// a run cut off before Close still shows, as incomplete.
func NewWriter(sink Sink, run *Run, opts WriterOptions) *Writer {
	opts.defaults()
	w := &Writer{sink: sink, run: run, opts: opts, ch: make(chan *Event, opts.Buffer), done: make(chan struct{})}
	go w.loop()
	return w
}

// Emit numbers e and queues it, without blocking. It's safe to call from
// any goroutine, also after Close, when the event is dropped.
func (w *Writer) Emit(e *Event) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.emitted++
	e.Seq = w.emitted
	if w.closed {
		w.dropped++
		return
	}
	select {
	case w.ch <- e:
	default:
		w.dropped++
	}
}

// Report is how a run's capture went.
type Report struct {
	// Err is the first failure, or nil when every event was written.
	Err              error
	Emitted, Written int
}

// Problem is the report as a database warning, or "" when every event was
// written.
func (r *Report) Problem() string {
	if r.Err == nil {
		return ""
	}
	return fmt.Sprintf("telemetry: wrote %d of %d events: %v", r.Written, r.Emitted, r.Err)
}

// Close writes what's queued and ends the run's row with out as its
// outcome. It waits until ctx is done at most; whatever isn't written by
// then is lost.
func (w *Writer) Close(ctx context.Context, outcome any) *Report {
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		close(w.ch)
	}
	w.mu.Unlock()
	select {
	case <-w.done:
	case <-ctx.Done():
		w.mu.RLock()
		defer w.mu.RUnlock()
		return &Report{Emitted: w.emitted, Err: fmt.Errorf("gave up waiting to write events: %w", ctx.Err())}
	}

	w.mu.RLock()
	emitted, dropped := w.emitted, w.dropped
	w.mu.RUnlock()
	err := w.err
	if dropped > 0 && err == nil {
		err = fmt.Errorf("dropped %d events: the buffer of %d was full", dropped, w.opts.Buffer)
	}
	end := &End{
		EndedAt: time.Now(), Outcome: outcome, Emitted: emitted, Written: w.written,
		Complete: err == nil && w.written == emitted,
	}
	if ferr := w.call(ctx, func(ctx context.Context) error { return w.sink.FinishRun(ctx, w.run.ID, end) }); ferr != nil {
		err = errors.Join(err, fmt.Errorf("finish the run: %w", ferr))
	}
	return &Report{Emitted: emitted, Written: w.written, Err: err}
}

// loop is the worker: it starts the run, then writes batches until the
// channel closes.
func (w *Writer) loop() {
	defer close(w.done)
	ctx := context.Background()
	started := w.start(ctx)
	tick := time.NewTicker(w.opts.FlushEvery)
	defer tick.Stop()
	var batch []*Event
	size := 0
	flush := func() {
		if len(batch) == 0 {
			return
		}
		// Without the run's row, events can't be written: the batch is
		// lost, and start kept why.
		if !started {
			started = w.start(ctx)
		}
		if started && w.call(ctx, func(ctx context.Context) error { return w.sink.WriteEvents(ctx, w.run.ID, batch) }) == nil {
			w.written += len(batch)
		}
		batch, size = nil, 0
	}
	for {
		select {
		case e, ok := <-w.ch:
			if !ok {
				flush()
				if !started {
					w.start(ctx)
				}
				return
			}
			batch = append(batch, e)
			size += len(e.Payload)
			if len(batch) >= w.opts.MaxBatch || size >= w.opts.MaxBatchBytes {
				flush()
			}
		case <-tick.C:
			flush()
		}
	}
}

func (w *Writer) start(ctx context.Context) bool {
	return w.call(ctx, func(ctx context.Context) error { return w.sink.StartRun(ctx, w.run) }) == nil
}

// call runs one sink operation, trying it once more if it fails, and keeps
// the first failure.
func (w *Writer) call(ctx context.Context, op func(context.Context) error) error {
	var err error
	for attempt := range 2 {
		if attempt > 0 {
			select {
			case <-time.After(w.opts.RetryAfter):
			case <-ctx.Done():
				return fmt.Errorf("%w (then %w)", err, ctx.Err())
			}
		}
		octx, cancel := context.WithTimeout(ctx, w.opts.OpTimeout)
		err = op(octx)
		cancel()
		if err == nil {
			return nil
		}
	}
	w.fail(err)
	return err
}

// fail keeps the first failure. Only the worker calls it before done, and
// Close calls it after.
func (w *Writer) fail(err error) {
	if w.err == nil {
		w.err = err
	}
}
