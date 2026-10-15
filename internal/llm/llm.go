// Package llm turns buggrit's model config into maestro-llms chat clients,
// one per review stage (docs/design.md §4, §9).
//
// Every client is wrapped in maestro-llms' recommended middleware chain
// (validation, retry, per-attempt timeout, circuit breaker, rate limit), and
// all stages share one limiter. That limiter is the single place model-call
// concurrency is bounded, today by limits.max_concurrency and later by
// lane limits.
package llm

import (
	"errors"
	"fmt"
	"os"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/middleware"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/anthropic"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/google"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/ollama"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/openai"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/vllm"
	"github.com/SnapdragonPartners/maestro-llms/llms/ratelimit"

	"github.com/SnapdragonPartners/buggrit/internal/config"
)

// Clients holds one ready-to-use chat client per stage.
type Clients struct {
	Find   llms.ChatClient
	Verify llms.ChatClient
	Dedupe llms.ChatClient
	// Limiter is shared by every client above.
	Limiter ratelimit.Limiter
}

// For returns the client for a stage.
func (c *Clients) For(s config.Stage) llms.ChatClient {
	switch s {
	case config.StageFind:
		return c.Find
	case config.StageVerify:
		return c.Verify
	case config.StageDedupe:
		return c.Dedupe
	}
	return nil
}

// baseFactory builds an unwrapped provider client. Tests replace it to avoid
// constructing real providers.
type baseFactory func(m *config.Model, apiKey string) (llms.ChatClient, error)

type options struct {
	getenv   func(string) string
	newBase  baseFactory
	observer middleware.Observer
}

// Option customizes New.
type Option func(*options)

// WithGetenv replaces os.Getenv for reading API keys.
func WithGetenv(f func(string) string) Option { return func(o *options) { o.getenv = f } }

// WithObserver adds maestro-llms' metrics middleware, reporting one event per
// provider attempt.
func WithObserver(obs middleware.Observer) Option { return func(o *options) { o.observer = obs } }

// New builds the stage clients for cfg. Stages that resolve to the same model
// still get separate clients, so each has its own circuit breaker; they share
// the limiter.
func New(cfg *config.Config, opts ...Option) (*Clients, error) {
	o := options{getenv: os.Getenv, newBase: newProviderClient}
	for _, opt := range opts {
		opt(&o)
	}

	lim := ratelimit.NewInMemoryLimiter(ratelimit.Config{MaxConcurrency: cfg.Limits.MaxConcurrency})
	out := &Clients{Limiter: lim}
	for _, s := range config.Stages() {
		m := cfg.Stage(s)
		key := ""
		if m.APIKeyEnv != "" {
			key = o.getenv(m.APIKeyEnv)
			if key == "" && keyRequired(m.Provider) {
				return nil, fmt.Errorf("stage %s: environment variable %s is empty or unset", s, m.APIKeyEnv)
			}
		}
		base, err := o.newBase(&m, key)
		if err != nil {
			return nil, fmt.Errorf("stage %s: %w", s, err)
		}
		c := middleware.RecommendedChat(base, middleware.RecommendedConfig{
			Limiter:  lim,
			Observer: o.observer,
			Retry:    middleware.DefaultRetryConfig(),
			Timeout:  m.RequestTimeout.Std(),
		})
		switch s {
		case config.StageFind:
			out.Find = c
		case config.StageVerify:
			out.Verify = c
		case config.StageDedupe:
			out.Dedupe = c
		}
	}
	return out, nil
}

// keyRequired reports whether a provider cannot work without an API key.
// Ollama and OpenAI-compatible endpoints may run without auth.
func keyRequired(provider string) bool {
	switch provider {
	case config.ProviderAnthropic, config.ProviderOpenAI, config.ProviderGoogle:
		return true
	}
	return false
}

// newProviderClient maps buggrit's provider names onto maestro-llms
// packages. This is the only place that mapping lives (§4).
func newProviderClient(m *config.Model, apiKey string) (llms.ChatClient, error) {
	var (
		c   llms.ChatClient
		err error
	)
	switch m.Provider {
	case config.ProviderAnthropic:
		opts := []anthropic.Option{anthropic.WithAPIKey(apiKey), anthropic.WithModel(m.Model)}
		if m.BaseURL != "" {
			opts = append(opts, anthropic.WithBaseURL(m.BaseURL))
		}
		c, err = anthropic.New(opts...)
	case config.ProviderOpenAI:
		opts := []openai.Option{openai.WithAPIKey(apiKey), openai.WithModel(m.Model)}
		if m.BaseURL != "" {
			opts = append(opts, openai.WithBaseURL(m.BaseURL))
		}
		c, err = openai.NewChat(opts...)
	case config.ProviderGoogle:
		opts := []google.Option{google.WithAPIKey(apiKey), google.WithModel(m.Model)}
		if m.BaseURL != "" {
			opts = append(opts, google.WithBaseURL(m.BaseURL))
		}
		c, err = google.New(opts...)
	case config.ProviderOllama:
		opts := []ollama.Option{ollama.WithModel(m.Model)}
		if m.BaseURL != "" {
			opts = append(opts, ollama.WithBaseURL(m.BaseURL))
		}
		c, err = ollama.New(opts...)
	case config.ProviderOpenAICompatible:
		// maestro-llms' vllm package speaks the generic OpenAI Chat
		// Completions surface (ADR-0015), which is what LunaRoute and other
		// OpenAI-compatible routers expose.
		opts := []vllm.Option{vllm.WithBaseURL(m.BaseURL), vllm.WithModel(m.Model)}
		if apiKey != "" {
			opts = append(opts, vllm.WithAPIKey(apiKey))
		}
		c, err = vllm.New(opts...)
	default:
		return nil, fmt.Errorf("unknown provider %q", m.Provider)
	}
	if err != nil {
		return nil, fmt.Errorf("create %s client: %w", m.Provider, err)
	}
	if c == nil {
		return nil, errors.New("provider returned a nil client")
	}
	return c, nil
}
