package telemetry

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// memSink keeps what it's sent. failWrites fails that many writes first;
// block, if set, holds every write until it's closed.
type memSink struct {
	startErr   error
	block      chan struct{}
	run        *Run
	end        *End
	events     map[int]*Event
	writes     int
	failWrites int
	mu         sync.Mutex
}

func newMemSink() *memSink { return &memSink{events: map[int]*Event{}} }

func (m *memSink) StartRun(_ context.Context, run *Run) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.startErr != nil {
		return m.startErr
	}
	m.run = run
	return nil
}

func (m *memSink) WriteEvents(ctx context.Context, _ string, events []*Event) error {
	if m.block != nil {
		select {
		case <-m.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writes++
	if m.failWrites > 0 {
		m.failWrites--
		// A failed write may still have landed: the retry must not
		// duplicate it.
		for _, e := range events {
			m.events[e.Seq] = e
		}
		return errors.New("connection reset")
	}
	for _, e := range events {
		if _, dup := m.events[e.Seq]; dup {
			continue
		}
		m.events[e.Seq] = e
	}
	return nil
}

func (m *memSink) FinishRun(_ context.Context, _ string, end *End) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.end = end
	return nil
}

var fast = WriterOptions{FlushEvery: time.Millisecond, RetryAfter: time.Millisecond, OpTimeout: time.Second}

func emitN(w *Writer, n int) {
	for range n {
		w.Emit(&Event{Kind: KindTool, Payload: []byte(`{}`)})
	}
}

func TestWriterWritesEveryEventInOrder(t *testing.T) {
	sink := newMemSink()
	opts := fast
	opts.MaxBatch = 7
	w := NewWriter(sink, &Run{ID: "r"}, opts)
	emitN(w, 100)
	rep := w.Close(context.Background(), map[string]int{"exit_code": 0})
	if rep.Err != nil || rep.Emitted != 100 || rep.Written != 100 || rep.Problem() != "" {
		t.Fatalf("report = %+v", rep)
	}
	for seq := 1; seq <= 100; seq++ {
		if sink.events[seq] == nil {
			t.Fatalf("event %d missing", seq)
		}
	}
	if sink.run == nil || sink.end == nil || !sink.end.Complete || sink.end.Emitted != 100 || sink.end.Written != 100 {
		t.Errorf("run %+v, end %+v", sink.run, sink.end)
	}
}

// A write that fails once is retried, and the sink skips what the failed
// attempt already wrote.
func TestWriterRetriesAFailedWrite(t *testing.T) {
	sink := newMemSink()
	sink.failWrites = 1
	w := NewWriter(sink, &Run{ID: "r"}, fast)
	emitN(w, 3)
	rep := w.Close(context.Background(), nil)
	if rep.Err != nil || rep.Written != 3 || len(sink.events) != 3 || !sink.end.Complete {
		t.Fatalf("report = %+v, events %d, end %+v", rep, len(sink.events), sink.end)
	}
}

// Events that can't be written are counted and dropped, and the run says
// it's incomplete. There's no spool.
func TestWriterDropsWhatItCantWrite(t *testing.T) {
	sink := newMemSink()
	sink.failWrites = 1 << 30
	w := NewWriter(sink, &Run{ID: "r"}, fast)
	emitN(w, 5)
	rep := w.Close(context.Background(), nil)
	if rep.Err == nil || rep.Written != 0 || rep.Emitted != 5 || sink.end.Complete {
		t.Fatalf("report = %+v, end %+v", rep, sink.end)
	}
	if p := rep.Problem(); !strings.Contains(p, "wrote 0 of 5 events") || !strings.Contains(p, "connection reset") {
		t.Errorf("problem = %q", p)
	}
}

func TestWriterWithoutTheRunRow(t *testing.T) {
	sink := newMemSink()
	sink.startErr = errors.New("permission denied for table run")
	w := NewWriter(sink, &Run{ID: "r"}, fast)
	emitN(w, 2)
	rep := w.Close(context.Background(), nil)
	if rep.Written != 0 || !strings.Contains(rep.Problem(), "permission denied") {
		t.Fatalf("report = %+v", rep)
	}
}

// Emit never blocks: with the buffer full and the sink stuck, events are
// dropped, and so are events after Close.
func TestWriterNeverBlocks(t *testing.T) {
	sink := newMemSink()
	sink.block = make(chan struct{})
	opts := fast
	opts.Buffer, opts.MaxBatch = 2, 1
	w := NewWriter(sink, &Run{ID: "r"}, opts)
	done := make(chan struct{})
	go func() {
		emitN(w, 50)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Emit blocked")
	}
	close(sink.block)
	rep := w.Close(context.Background(), nil)
	w.Emit(&Event{Kind: KindTool})
	if rep.Err == nil || rep.Written >= 50 || !strings.Contains(rep.Problem(), "buffer") || sink.end.Complete {
		t.Fatalf("report = %+v (%s)", rep, rep.Problem())
	}
}

// Close gives up when its context ends, and says so, and stops the worker:
// nothing queued is written after it returns, even once the sink would
// accept it.
func TestWriterCloseTimesOut(t *testing.T) {
	sink := newMemSink()
	sink.block = make(chan struct{})
	opts := fast
	opts.MaxBatch = 1
	w := NewWriter(sink, &Run{ID: "r"}, opts)
	emitN(w, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	rep := w.Close(ctx, nil)
	if !errors.Is(rep.Err, context.DeadlineExceeded) || rep.Emitted != 3 || rep.Written != 0 {
		t.Fatalf("report = %+v", rep)
	}
	close(sink.block)
	time.Sleep(50 * time.Millisecond)
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.events) != 0 || sink.end != nil {
		t.Errorf("after Close gave up: %d events written, end %+v", len(sink.events), sink.end)
	}
}
