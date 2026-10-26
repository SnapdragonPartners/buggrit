package store

import (
	"context"
	"fmt"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/bucket"
	bucketpg "github.com/SnapdragonPartners/maestro-llms/bucket/postgres"
	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"

	"github.com/SnapdragonPartners/buggrit/internal/config"
	"github.com/SnapdragonPartners/buggrit/internal/llm"
)

// slowModel counts the calls in flight across every client that uses it.
type slowModel struct {
	now, peak atomic.Int32
}

func (*slowModel) Model() llms.ModelRef {
	return llms.ModelRef{Provider: "openai-compatible", Name: "m"}
}

//nolint:gocritic // hugeParam: the signature is fixed by llms.ChatClient
func (m *slowModel) Complete(context.Context, llms.ChatRequest) (llms.ChatResponse, error) {
	n := m.now.Add(1)
	defer m.now.Add(-1)
	for {
		p := m.peak.Load()
		if n <= p || m.peak.CompareAndSwap(p, n) {
			break
		}
	}
	time.Sleep(20 * time.Millisecond)
	return testllm.TextResponse("ok"), nil
}

// Two repositories' runs, each with its own store and a run limit of 4,
// share a bucket of 2 (design §9): there are never more than 2 calls in
// flight. They connect as the runtime role, which can set the limits.
func TestSharedBucketAcrossRuns(t *testing.T) {
	dsn := newDatabase(t)
	migrateAll(t, dsn)
	role := fmt.Sprintf("buggrit_runtime_%d", time.Now().UnixNano())
	exec(t, dsn,
		"CREATE ROLE "+role+" LOGIN",
		"GRANT USAGE ON SCHEMA buggrit, bucket TO "+role,
		"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA buggrit, bucket TO "+role)
	t.Cleanup(func() { exec(t, dsn, "DROP OWNED BY "+role, "DROP ROLE "+role) })
	u, _ := url.Parse(dsn)
	u.User = url.User(role)

	cfg, err := config.Parse([]byte(`{"version": 1, "limits": {"max_concurrency": 4}, "models": {"default": {"provider": "openai-compatible", "base_url": "https://llm.example", "model": "m"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	model := &slowModel{}
	var runs []llms.ChatClient
	for range 2 {
		s, err := Open(ctx, u.String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		b, err := s.Buckets(ctx, bucketpg.Config{})
		if err != nil {
			t.Fatal(err)
		}
		if err = b.Ensure(ctx, "luna", bucket.Limits{MaxConcurrency: 2}, true); err != nil {
			t.Fatalf("Ensure as the runtime role: %v", err)
		}
		c, err := llm.New(cfg,
			llm.WithShared(&llm.Shared{Limiter: b, Stages: map[config.Stage]llm.SharedStage{config.StageFind: {Bucket: "luna", MaxWait: time.Minute}}}),
			llm.WithBase(func(*config.Model, string) (llms.ChatClient, error) { return model, nil }))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if p := c.Shared.Problems(); len(p) != 0 {
				t.Errorf("problems = %q", p)
			}
		})
		runs = append(runs, c.Find)
	}

	var wg sync.WaitGroup
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := runs[i%2].Complete(ctx, llms.ChatRequest{Messages: []llms.Message{llms.UserText("hi")}}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if p := model.peak.Load(); p != 2 {
		t.Errorf("peak in flight = %d, want 2", p)
	}
}
