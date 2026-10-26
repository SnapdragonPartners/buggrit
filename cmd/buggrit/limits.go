package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/bucket"
	bucketpg "github.com/SnapdragonPartners/maestro-llms/bucket/postgres"

	"github.com/SnapdragonPartners/buggrit/internal/config"
	"github.com/SnapdragonPartners/buggrit/internal/llm"
)

// rateLimitsEnv names the shared rate limits (design §9, "Shared rate
// limit"). It's an organization variable rather than repository config,
// since a limit belongs to a credential every repository shares. It holds
// no secret: api_key_env is the name of the variable with the key.
const rateLimitsEnv = "BUGGRIT_RATE_LIMITS"

// rateLimit is one shared bucket: the endpoint and credential it covers,
// and its limits.
type rateLimit struct {
	BaseURL         string          `json:"base_url"`
	APIKeyEnv       string          `json:"api_key_env"`
	MaxWait         config.Duration `json:"max_wait"`
	MaxConcurrency  int             `json:"max_concurrency"`
	TokensPerMinute int             `json:"tokens_per_minute"`
}

// parseRateLimits decodes BUGGRIT_RATE_LIMITS, strictly, as config is.
func parseRateLimits(raw string) (map[string]rateLimit, error) {
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.DisallowUnknownFields()
	var out map[string]rateLimit
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("%s: %w", rateLimitsEnv, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("%s: more than one JSON value", rateLimitsEnv)
	}
	for name := range out {
		l := out[name]
		switch {
		case strings.TrimSpace(name) == "":
			return nil, fmt.Errorf("%s: a bucket has an empty name", rateLimitsEnv)
		case l.BaseURL == "":
			return nil, fmt.Errorf("%s: bucket %q: base_url is required", rateLimitsEnv, name)
		case l.MaxConcurrency <= 0 && l.TokensPerMinute <= 0:
			return nil, fmt.Errorf("%s: bucket %q: set max_concurrency, tokens_per_minute or both", rateLimitsEnv, name)
		case l.MaxConcurrency < 0 || l.TokensPerMinute < 0 || l.MaxWait < 0:
			return nil, fmt.Errorf("%s: bucket %q: limits can't be negative", rateLimitsEnv, name)
		}
		if err := l.limits().Validate(); err != nil {
			return nil, fmt.Errorf("%s: bucket %q: %w", rateLimitsEnv, name, err)
		}
	}
	return out, nil
}

func (l *rateLimit) limits() bucket.Limits {
	return bucket.Limits{MaxConcurrency: l.MaxConcurrency, TokensPerMinute: l.TokensPerMinute}
}

// matches reports whether a stage's model uses this bucket's endpoint and
// credential.
func (l *rateLimit) matches(m *config.Model) bool {
	return strings.TrimRight(l.BaseURL, "/") == strings.TrimRight(m.BaseURL, "/") && l.APIKeyEnv == m.APIKeyEnv
}

// sharedBuckets is the shared rate limit's backend: bucket/postgres.
type sharedBuckets interface {
	bucket.Limiter
	Ensure(ctx context.Context, name string, limits bucket.Limits, overwrite bool) error
}

// sharedLimits decides which stages share a bucket, and gives each bucket
// they use the variable's limits. The variable wins over whatever the
// database had (Ensure with overwrite), so a limit is changed by editing
// it. A stage whose endpoint isn't listed has only the run's own limit.
// It returns nil when no stage shares a bucket.
func sharedLimits(ctx context.Context, cfg *config.Config, raw string, open func(bucketpg.Config) (sharedBuckets, error), log func(format string, args ...any)) (*llm.Shared, error) {
	if strings.TrimSpace(raw) == "" {
		log("buggrit: %s isn't set, so this run has only its own rate limit\n", rateLimitsEnv)
		return nil, nil //nolint:nilnil // no shared limit is a normal outcome, documented above
	}
	limits, err := parseRateLimits(raw)
	if err != nil {
		return nil, err
	}
	stages := map[config.Stage]llm.SharedStage{}
	var used []string
	for _, s := range config.Stages() {
		m := cfg.Stage(s)
		var names []string
		for name := range limits {
			if l := limits[name]; l.matches(&m) {
				names = append(names, name)
			}
		}
		slices.Sort(names)
		switch len(names) {
		case 0:
			log("buggrit: %s's endpoint isn't in %s, so it has only this run's rate limit\n", s, rateLimitsEnv)
			continue
		case 1:
		default:
			return nil, fmt.Errorf("%s: buckets %s all match the %s stage's endpoint", rateLimitsEnv, strings.Join(names, ", "), s)
		}
		l := limits[names[0]]
		wait := l.MaxWait.Std()
		if wait == 0 {
			wait = defaultMaxWait(cfg)
		}
		stages[s] = llm.SharedStage{Bucket: names[0], MaxWait: wait}
		if !slices.Contains(used, names[0]) {
			used = append(used, names[0])
		}
		log("buggrit: %s shares the rate limit %q\n", s, names[0])
	}
	if len(stages) == 0 {
		return nil, nil //nolint:nilnil // as above
	}
	b, err := open(bucketpg.Config{MaxLeaseDuration: maxLeaseDuration(cfg)})
	if err != nil {
		return nil, fmt.Errorf("the shared rate limit: %w", err)
	}
	for _, name := range used {
		l := limits[name]
		if err := b.Ensure(ctx, name, l.limits(), true); err != nil {
			return nil, fmt.Errorf("set the shared rate limit %q: %w", name, err)
		}
	}
	return &llm.Shared{Limiter: b, Stages: stages}, nil
}

// defaultMaxWait is how long a call waits for a lease before the run gives
// up on the shared limit: a quarter of the pass deadline, so a run still has
// most of its time when it does.
func defaultMaxWait(cfg *config.Config) time.Duration {
	return cfg.Limits.PassDeadline.Std() / 4
}

// maxLeaseDuration must outlast the longest call, or a long call's lease is
// lost while it runs.
func maxLeaseDuration(cfg *config.Config) time.Duration {
	d := bucketpg.DefaultMaxLeaseDuration
	for _, s := range config.Stages() {
		m := cfg.Stage(s)
		d = max(d, m.RequestTimeout.Std()+time.Minute)
	}
	return d
}

// sharedSummary is the job summary's account of the shared rate limit: how
// long the run waited for leases, so queueing behind other runs shows apart
// from model time. It's "" for a run without one.
func sharedSummary(s *llm.SharedState) string {
	if s == nil {
		return ""
	}
	waited, leases := s.Waited()
	return fmt.Sprintf("\n### Shared rate limit\n\nWaited %s in total for %d leases.\n", waited.Round(time.Second), leases)
}
