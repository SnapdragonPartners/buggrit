package llm

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"

	"github.com/SnapdragonPartners/buggrit/internal/config"
)

func parse(t *testing.T, doc string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return cfg
}

func env(m map[string]string) Option {
	return WithGetenv(func(k string) string { return m[k] })
}

func TestNewBuildsEachStageFromItsEffectiveModel(t *testing.T) {
	cfg := parse(t, `{"version": 1, "models": {
	  "default": {"provider": "openai-compatible", "base_url": "https://llm.example", "model": "qwen3-coder", "api_key_env": "K"},
	  "verify": {"model": "glm-4.6"}}}`)

	type call struct{ model, key string }
	var calls []call
	fake := func(m *config.Model, key string) (llms.ChatClient, error) {
		calls = append(calls, call{m.Model, key})
		return &testllm.FakeChatClient{ModelRef: llms.ModelRef{Provider: llms.ProviderID(m.Provider), Name: m.Model}}, nil
	}
	clients, err := New(cfg, env(map[string]string{"K": "secret"}), func(o *options) { o.newBase = fake })
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	want := []call{{"qwen3-coder", "secret"}, {"glm-4.6", "secret"}, {"qwen3-coder", "secret"}}
	if len(calls) != len(want) {
		t.Fatalf("factory calls = %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Errorf("call %d = %+v, want %+v", i, calls[i], want[i])
		}
	}
	if got := clients.For(config.StageVerify).Model().Name; got != "glm-4.6" {
		t.Errorf("verify client model = %q, want glm-4.6", got)
	}
	if clients.For(config.StageFind) == nil || clients.For(config.StageDedupe) == nil {
		t.Error("missing stage client")
	}
}

func TestNewRequiresKeysOnlyForHostedProviders(t *testing.T) {
	hosted := parse(t, `{"version": 1, "models": {"default": {"provider": "anthropic", "model": "m", "api_key_env": "ANTHROPIC_KEY"}}}`)
	_, err := New(hosted, env(nil))
	if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_KEY is empty or unset") {
		t.Fatalf("err = %v, want missing-key error naming the variable", err)
	}

	// An OpenAI-compatible endpoint may run without auth.
	open := parse(t, `{"version": 1, "models": {"default": {"provider": "openai-compatible", "base_url": "http://localhost:8000", "model": "m", "api_key_env": "UNSET"}}}`)
	if _, err := New(open, env(nil)); err != nil {
		t.Fatalf("New without optional key: %v", err)
	}
}

// TestNewConstructsRealProviders checks the provider mapping end to end.
// Construction makes no network calls.
func TestNewConstructsRealProviders(t *testing.T) {
	tests := []struct {
		provider, extra string
	}{
		{config.ProviderAnthropic, `"api_key_env": "K"`},
		{config.ProviderOpenAI, `"api_key_env": "K"`},
		{config.ProviderGoogle, `"api_key_env": "K"`},
		{config.ProviderOllama, `"base_url": "http://localhost:11434"`},
		{config.ProviderOpenAICompatible, `"base_url": "http://localhost:8000"`},
	}
	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			cfg := parse(t, `{"version": 1, "models": {"default": {"provider": "`+tt.provider+`", "model": "some-model", `+tt.extra+`}}}`)
			clients, err := New(cfg, env(map[string]string{"K": "secret"}))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if got := clients.Find.Model().Name; got != "some-model" {
				t.Errorf("model = %q, want some-model", got)
			}
		})
	}
}

// TestStagesShareTheConcurrencyLimit runs one call on each of two stages with
// max_concurrency 1 and checks they never overlap.
func TestStagesShareTheConcurrencyLimit(t *testing.T) {
	cfg := parse(t, `{"version": 1, "limits": {"max_concurrency": 1},
	  "models": {"default": {"provider": "ollama", "model": "m"}}}`)

	var inFlight, peak atomic.Int32
	fake := func(m *config.Model, _ string) (llms.ChatClient, error) {
		return &testllm.FakeChatClient{
			ModelRef: llms.ModelRef{Provider: llms.ProviderID(m.Provider), Name: m.Model},
			Func: func(context.Context, llms.ChatRequest) (llms.ChatResponse, error) {
				n := inFlight.Add(1)
				for {
					p := peak.Load()
					if n <= p || peak.CompareAndSwap(p, n) {
						break
					}
				}
				time.Sleep(20 * time.Millisecond)
				inFlight.Add(-1)
				return testllm.TextResponse("ok"), nil
			},
		}, nil
	}
	clients, err := New(cfg, func(o *options) { o.newBase = fake })
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	req := llms.ChatRequest{Messages: []llms.Message{llms.UserText("hi")}, MaxTokens: 16}
	errs := make(chan error, 2)
	for _, c := range []llms.ChatClient{clients.Find, clients.Verify} {
		go func() {
			_, err := c.Complete(ctx, req)
			errs <- err
		}()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("Complete: %v", err)
		}
	}
	if p := peak.Load(); p != 1 {
		t.Errorf("peak concurrent calls = %d, want 1", p)
	}
}

func TestMaxOutputTokensCapsRequests(t *testing.T) {
	cfg := parse(t, `{"version": 1, "models": {
	  "default": {"provider": "ollama", "model": "m", "max_output_tokens": 4096},
	  "verify": {"max_output_tokens": 1024},
	  "dedupe": {"max_output_tokens": 0}}}`)

	var seen []int
	fake := func(m *config.Model, _ string) (llms.ChatClient, error) {
		return &testllm.FakeChatClient{
			ModelRef: llms.ModelRef{Provider: llms.ProviderID(m.Provider), Name: m.Model},
			Func: func(_ context.Context, req llms.ChatRequest) (llms.ChatResponse, error) {
				seen = append(seen, req.MaxTokens)
				return testllm.TextResponse("ok"), nil
			},
		}, nil
	}
	clients, err := New(cfg, func(o *options) { o.newBase = fake })
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tests := []struct {
		name   string
		client llms.ChatClient
		asked  int
		want   int
	}{
		{"default cap fills an unset limit", clients.Find, 0, 4096},
		{"default cap lowers a larger limit", clients.Find, 100000, 4096},
		{"a smaller caller limit is kept", clients.Find, 512, 512},
		{"stage cap overrides the default", clients.Verify, 0, 1024},
		{"explicit 0 means no cap", clients.Dedupe, 100000, 100000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seen = nil
			req := llms.ChatRequest{Messages: []llms.Message{llms.UserText("hi")}, MaxTokens: tt.asked}
			if _, err := tt.client.Complete(context.Background(), req); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if len(seen) != 1 || seen[0] != tt.want {
				t.Errorf("provider saw MaxTokens %v, want [%d]", seen, tt.want)
			}
		})
	}
}

// Every reasoning_effort the config accepts is one of maestro-llms' levels,
// so none is silently omitted as unknown.
func TestReasoningEffortsMatchMaestro(t *testing.T) {
	known := []llms.ReasoningEffort{
		llms.ReasoningEffortNone, llms.ReasoningEffortMinimal, llms.ReasoningEffortLow, llms.ReasoningEffortMedium,
		llms.ReasoningEffortHigh, llms.ReasoningEffortXHigh, llms.ReasoningEffortMax,
	}
	got := config.ReasoningEfforts()
	if len(got) != len(known) {
		t.Fatalf("config has %d levels, maestro-llms %d", len(got), len(known))
	}
	for i := range known {
		if llms.ReasoningEffort(got[i]) != known[i] {
			t.Errorf("level %d = %q, want %q", i, got[i], known[i])
		}
	}
}

type reasoningLog struct {
	mu   sync.Mutex
	seen []string
}

func (l *reasoningLog) ObserveReasoning(s config.Stage, effort string, applied *llms.ReasoningConfig) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := "default"
	if applied != nil {
		a = string(applied.Effort)
	}
	l.seen = append(l.seen, string(s)+":"+effort+"->"+a)
}

func TestReasoningEffortSetsRequests(t *testing.T) {
	cfg := parse(t, `{"version": 1, "models": {
	  "default": {"provider": "ollama", "model": "m", "reasoning_effort": "high"},
	  "verify": {"reasoning_effort": "none"},
	  "dedupe": {"reasoning_effort": ""}}}`)

	var seen []*llms.ReasoningConfig
	fake := func(m *config.Model, _ string) (llms.ChatClient, error) {
		return &testllm.FakeChatClient{
			ModelRef: llms.ModelRef{Provider: llms.ProviderID(m.Provider), Name: m.Model},
			Func: func(_ context.Context, req llms.ChatRequest) (llms.ChatResponse, error) {
				seen = append(seen, req.Reasoning)
				resp := testllm.TextResponse("ok")
				if req.Reasoning != nil && req.Reasoning.Effort != llms.ReasoningEffortNone {
					resp.AppliedReasoning = &llms.ReasoningConfig{Effort: req.Reasoning.Effort}
				}
				return resp, nil
			},
		}, nil
	}
	log := &reasoningLog{}
	clients, err := New(cfg, WithReasoningObserver(log), func(o *options) { o.newBase = fake })
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tests := []struct {
		name   string
		client llms.ChatClient
		asked  *llms.ReasoningConfig
		want   llms.ReasoningEffort
	}{
		{"default effort", clients.Find, nil, llms.ReasoningEffortHigh},
		{"a caller's own setting is kept", clients.Find, &llms.ReasoningConfig{Effort: llms.ReasoningEffortLow}, llms.ReasoningEffortLow},
		{"stage override", clients.Verify, nil, llms.ReasoningEffortNone},
		{"explicit empty means the model default", clients.Dedupe, nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seen = nil
			req := llms.ChatRequest{Messages: []llms.Message{llms.UserText("hi")}, Reasoning: tt.asked}
			for range 2 {
				if _, err := tt.client.Complete(context.Background(), req); err != nil {
					t.Fatalf("Complete: %v", err)
				}
			}
			var got llms.ReasoningEffort
			if seen[0] != nil {
				got = seen[0].Effort
			}
			if got != tt.want {
				t.Errorf("provider saw effort %q, want %q", got, tt.want)
			}
		})
	}
	// Each stage reports once, and only for its own setting.
	want := []string{"find:high->high", "verify:none->default"}
	if strings.Join(log.seen, " ") != strings.Join(want, " ") {
		t.Errorf("reports = %v, want %v", log.seen, want)
	}
}
