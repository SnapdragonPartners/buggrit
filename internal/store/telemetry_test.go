package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/middleware"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"

	"github.com/SnapdragonPartners/buggrit/internal/telemetry"
)

// docQueries returns docs/telemetry.md's SQL blocks, with psql's :'run',
// :'session' and :'call' variables replaced by repl.
func docQueries(t *testing.T, repl *strings.Replacer) []string {
	t.Helper()
	data, err := os.ReadFile("../../docs/telemetry.md")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, block := range strings.Split(string(data), "```sql\n")[1:] {
		q, _, _ := strings.Cut(block, "```")
		out = append(out, repl.Replace(q))
	}
	if len(out) < 2 {
		t.Fatalf("found %d SQL blocks in docs/telemetry.md", len(out))
	}
	return out
}

// A run's telemetry round trips through Postgres: the run row, its events
// in order, a retried batch without duplicates, a NUL in a tool result,
// and every request rebuilt by the documented query.
func TestTelemetryRoundTrip(t *testing.T) {
	dsn := newDatabase(t)
	migrateAll(t, dsn)
	ctx := context.Background()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	sink := &retryingSink{Sink: st.Telemetry(), failAfterFirst: true}
	run := &telemetry.Run{
		ID: "6f1c2a4e-8b3d-4c5e-9f60-718293a4b5c6", StartedAt: time.Now(), Repo: "acme/widgets", PR: 7,
		HeadSHA: "abc", Label: "packing-a", Meta: map[string]any{
			"buggrit": "dev",
			"models":  map[string]any{"find": map[string]string{"model": "glm-5.3", "reasoning_effort": "medium"}},
			"mph": map[string]any{
				"commit": "0123456789abcdef0123456789abcdef01234567", "modified": false,
				"stages": map[string]any{"find": map[string]string{"prompt_hash": "sha256:aa", "tools_hash": "sha256:bb"}},
			},
		},
	}
	w := telemetry.NewWriter(sink, run, telemetry.WriterOptions{FlushEvery: time.Millisecond, RetryAfter: time.Millisecond, MaxBatch: 3})
	rec := telemetry.NewRecorder(w.Emit)
	rec.Emit(telemetry.KindPass, map[string]string{"mode": "full"})
	fake := &testllm.FakeChatClient{Func: func(context.Context, llms.ChatRequest) (llms.ChatResponse, error) {
		resp := testllm.TextResponse("ok")
		resp.Usage = llms.Usage{InputTokens: 100, OutputTokens: 10}
		return resp, nil
	}}
	s := rec.Session("find")
	s.Emit(telemetry.KindOpening, map[string]any{"diff": []map[string]any{{"path": "a.go", "bytes": 10}}})
	// Through maestro-llms' middleware, so each call's attempt is recorded.
	client := s.Chat(middleware.RecommendedChat(fake, middleware.RecommendedConfig{Observer: attempts{rec}}))
	var msgs []llms.Message
	for i := range 4 {
		msgs = append(msgs, llms.UserText(strings.Repeat("x", i)+"\x00"))
		if i == 3 {
			msgs = append(msgs[:2], llms.UserText("nudge")) // cut back, then append
		}
		if _, cerr := client.Complete(ctx, llms.ChatRequest{Messages: msgs}); cerr != nil {
			t.Fatal(cerr)
		}
		// The same get_diff after each of the first two calls: shown in the
		// opening, then repeated too.
		if i < 2 {
			s.Tool(toolloop.ToolCallEvent{Call: llms.ToolCall{ID: "t", Name: "get_diff", Parameters: json.RawMessage(`{"path": "a.go"}`)}})
		}
	}
	// A clean pass, and a verifier whose opening showed no diff: the
	// documented queries take empty lists.
	rec.Session("verify").Emit(telemetry.KindOpening, map[string]any{"diff": []any{}})
	rec.Emit(telemetry.KindResult, map[string]any{"elapsed_ms": 1200, "done": true, "kept": []any{}})
	rep := w.Close(ctx, map[string]any{"exit_code": 0, "status": "complete"})
	if rep.Err != nil || rep.Written != rep.Emitted {
		t.Fatalf("report = %+v", rep)
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var (
		emitted, written int
		complete, dryRun bool
		label            string
		events           int
	)
	if err := db.QueryRow(`SELECT events_emitted, events_written, complete, dry_run, label,
	    (SELECT count(*) FROM buggrit.event WHERE run_id = run.id) FROM buggrit.run WHERE id = $1`, run.ID).
		Scan(&emitted, &written, &complete, &dryRun, &label, &events); err != nil {
		t.Fatal(err)
	}
	if !complete || emitted != rep.Emitted || written != emitted || events != emitted || dryRun || label != "packing-a" {
		t.Errorf("run row: emitted %d written %d complete %v dry %v label %q, %d event rows", emitted, written, complete, dryRun, label, events)
	}
	var nulls int
	if err := db.QueryRow(`SELECT count(*) FROM buggrit.event WHERE run_id = $1 AND session IS NULL AND call IS NULL`, run.ID).Scan(&nulls); err != nil || nulls != 2 {
		t.Errorf("events outside a session: %d, %v", nulls, err)
	}

	// The documented rebuild query, the first block, rebuilds every request.
	rebuildSQL := docQueries(t, strings.NewReplacer(":'run'", "$1", ":'session'", "$2", ":'call'", "$3"))[0]
	for n, req := range fake.Calls() {
		rows, err := db.Query(rebuildSQL, run.ID, "find", n+1)
		if err != nil {
			t.Fatal(err)
		}
		var got []any
		for rows.Next() {
			var pos int
			var raw []byte
			if err := rows.Scan(&pos, &raw); err != nil {
				t.Fatal(err)
			}
			if pos != len(got) {
				t.Fatalf("call %d: position %d after %d messages", n+1, pos, len(got))
			}
			var m any
			_ = json.Unmarshal(raw, &m)
			got = append(got, m)
		}
		_ = rows.Close()
		var want []any
		for _, m := range req.Messages {
			data, _ := json.Marshal(m)
			var v any
			_ = json.Unmarshal([]byte(strings.ReplaceAll(string(data), `\u0000`, `�`)), &v)
			want = append(want, v)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("call %d rebuilt as %v, want %v", n+1, got, want)
		}
	}
	checkDocQueries(t, db, run.ID)
}

// Every documented query runs against the schema, and each of its columns
// has a value in some row: a JSON key the payloads don't have would be
// null throughout.
func checkDocQueries(t *testing.T, db *sql.DB, runID string) {
	t.Helper()
	for i, q := range docQueries(t, strings.NewReplacer(":'run'", "'"+runID+"'", ":'session'", "'find'", ":'call'", "'2'")) {
		rows, err := db.Query(q)
		if err != nil {
			t.Errorf("docs/telemetry.md query %d: %v\n%s", i+1, err, q)
			continue
		}
		cols, _ := rows.Columns()
		seen := make([]bool, len(cols))
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for j := range vals {
				ptrs[j] = &vals[j]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			for j, v := range vals {
				seen[j] = seen[j] || v != nil
			}
		}
		if err := rows.Err(); err != nil {
			t.Errorf("docs/telemetry.md query %d: %v", i+1, err)
		}
		_ = rows.Close()
		for j, ok := range seen {
			if !ok {
				t.Errorf("docs/telemetry.md query %d: column %s is null in every row", i+1, cols[j])
			}
		}
	}
}

type attempts struct{ rec *telemetry.Recorder }

func (a attempts) ObserveContext(ctx context.Context, e middleware.Event) { //nolint:gocritic // fixed by the interface
	a.rec.Attempt(ctx, e)
}
func (attempts) Observe(middleware.Event) {} //nolint:gocritic // fixed by the interface

// retryingSink fails the second batch after writing it, so the Writer's
// retry sends it again.
type retryingSink struct {
	telemetry.Sink
	batches        int
	mu             sync.Mutex
	failAfterFirst bool
}

func (r *retryingSink) WriteEvents(ctx context.Context, runID string, events []*telemetry.Event) error {
	if err := r.Sink.WriteEvents(ctx, runID, events); err != nil {
		return err //nolint:wrapcheck // the test's own sink
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.batches++
	if r.failAfterFirst && r.batches == 2 {
		return context.DeadlineExceeded
	}
	return nil
}
