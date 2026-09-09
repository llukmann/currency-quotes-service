// Package config reads the service configuration from environment variables.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"
)

// Defaults applied when the corresponding variable is unset or empty.
// They must stay in sync with .env.example.
const (
	defaultHTTPPort         = 8080
	defaultHTTPReadTimeout  = 5 * time.Second
	defaultHTTPWriteTimeout = 10 * time.Second
	defaultShutdownTimeout  = 10 * time.Second
	defaultLogLevel         = slog.LevelInfo

	defaultProviderBaseURL  = "https://api.frankfurter.dev"
	defaultProviderTimeout  = 3 * time.Second
	defaultProviderAttempts = 3
	defaultProviderBackoff  = 200 * time.Millisecond

	defaultWorkerConcurrency      = 4
	defaultWorkerPollInterval     = time.Second
	defaultWorkerTaskTimeout      = 15 * time.Second
	defaultWorkerStuckTimeout     = 60 * time.Second
	defaultWorkerRecoveryInterval = 30 * time.Second
	defaultWorkerMaxAttempts      = 3

	defaultIdempotencyTTL             = time.Hour
	defaultIdempotencyCleanupInterval = 10 * time.Minute
)

// Valid TCP port range. Port 0 is excluded: it would make the server pick an
// arbitrary free port, which cannot match the published compose port.
const (
	minPort = 1
	maxPort = 65535
)

// handlerTimeoutMargin is how much of HTTP_WRITE_TIMEOUT is kept back from the
// deadline of a request, so that a handler which runs out of time still has
// room to write its answer. Without the gap the two deadlines would expire
// together and the server would drop the connection in the middle of the very
// error the deadline was meant to produce, leaving a client that saw a broken
// connection where the contract promises an envelope.
const handlerTimeoutMargin = time.Second

// stuckTimeoutMargin is how many task deadlines a task may spend in_progress
// before the recovery pass treats it as abandoned. Plain "longer than one
// deadline" would not do: the deadline is measured by this process, the
// staleness threshold by the database clock, and between the claim commit and
// the start of the deadline lie things the deadline never counted -- waiting
// for a pooled connection, the finalising transaction itself, CPU throttling.
// Erring high costs a delay before a dead worker's task is picked up; erring
// low takes tasks away from workers that are still alive.
const stuckTimeoutMargin = 2

// Config holds the service parameters. Variable names are listed in
// .env.example, which is the source of truth for them.
type Config struct {
	DatabaseURL      string
	HTTPPort         int
	HTTPReadTimeout  time.Duration
	HTTPWriteTimeout time.Duration
	ShutdownTimeout  time.Duration
	LogLevel         slog.Level

	// ProviderBaseURL is the scheme and host of the rate API, without a path:
	// the path belongs to the client that knows the API.
	ProviderBaseURL string
	// ProviderTimeout bounds one HTTP attempt, not the whole retry schedule.
	ProviderTimeout  time.Duration
	ProviderAttempts int
	// ProviderBackoff is the pause before the second attempt, doubling before
	// each attempt after it.
	ProviderBackoff time.Duration

	// WorkerConcurrency is how many tasks are processed at the same time.
	WorkerConcurrency int
	// WorkerPollInterval is how long a worker waits before asking an empty
	// queue again.
	WorkerPollInterval time.Duration
	// WorkerTaskTimeout is the deadline of a single task. It covers the whole
	// path from the claim to the finalising commit: the provider call with all
	// of its retries, and the database work that follows it.
	WorkerTaskTimeout time.Duration
	// WorkerStuckTimeout is how long a task may sit in_progress before the
	// recovery pass takes it back, see stuckTimeoutMargin.
	WorkerStuckTimeout time.Duration
	// WorkerRecoveryInterval is how often that pass runs.
	WorkerRecoveryInterval time.Duration
	// WorkerMaxAttempts is how many times a task may be claimed before the
	// recovery pass closes it as failed instead of releasing it once more.
	WorkerMaxAttempts int

	// IdempotencyTTL is how long the binding between an Idempotency-Key and
	// the task it was answered with holds. It is also the longest a post can
	// silently perform no update at all, which is why it is far shorter than
	// the day such keys are conventionally kept: a client repeating a live key
	// is handed the earlier task rather than a fresh rate.
	IdempotencyTTL time.Duration
	// IdempotencyCleanupInterval is how often expired bindings are swept. The
	// sweep is the only thing enforcing the lifetime above -- a lookup never
	// checks the age of what it finds -- so this interval is the margin by
	// which a binding outlives it.
	IdempotencyCleanupInterval time.Duration
}

// Load reads the configuration from the environment. Unset variables fall
// back to defaults; a malformed value is an error naming the variable.
func Load() (Config, error) {
	var cfg Config
	var err error

	if cfg.DatabaseURL, err = requiredFromEnv("DATABASE_URL"); err != nil {
		return Config{}, err
	}
	if cfg.HTTPPort, err = intFromEnv("HTTP_PORT", defaultHTTPPort); err != nil {
		return Config{}, err
	}
	if cfg.HTTPPort < minPort || cfg.HTTPPort > maxPort {
		return Config{}, fmt.Errorf("HTTP_PORT: %d is out of range %d-%d", cfg.HTTPPort, minPort, maxPort)
	}
	if cfg.HTTPReadTimeout, err = durationFromEnv("HTTP_READ_TIMEOUT", defaultHTTPReadTimeout); err != nil {
		return Config{}, err
	}
	if cfg.HTTPWriteTimeout, err = durationFromEnv("HTTP_WRITE_TIMEOUT", defaultHTTPWriteTimeout); err != nil {
		return Config{}, err
	}
	if cfg.ShutdownTimeout, err = durationFromEnv("SHUTDOWN_TIMEOUT", defaultShutdownTimeout); err != nil {
		return Config{}, err
	}
	if cfg.LogLevel, err = levelFromEnv("LOG_LEVEL", defaultLogLevel); err != nil {
		return Config{}, err
	}

	cfg.ProviderBaseURL = stringFromEnv("PROVIDER_BASE_URL", defaultProviderBaseURL)
	if cfg.ProviderTimeout, err = durationFromEnv("PROVIDER_TIMEOUT", defaultProviderTimeout); err != nil {
		return Config{}, err
	}
	if cfg.ProviderAttempts, err = positiveIntFromEnv("PROVIDER_ATTEMPTS", defaultProviderAttempts); err != nil {
		return Config{}, err
	}
	if cfg.ProviderBackoff, err = durationFromEnv("PROVIDER_BACKOFF", defaultProviderBackoff); err != nil {
		return Config{}, err
	}

	if cfg.WorkerConcurrency, err = positiveIntFromEnv("WORKER_CONCURRENCY", defaultWorkerConcurrency); err != nil {
		return Config{}, err
	}
	if cfg.WorkerPollInterval, err = durationFromEnv("WORKER_POLL_INTERVAL", defaultWorkerPollInterval); err != nil {
		return Config{}, err
	}
	if cfg.WorkerTaskTimeout, err = durationFromEnv("WORKER_TASK_TIMEOUT", defaultWorkerTaskTimeout); err != nil {
		return Config{}, err
	}
	if cfg.WorkerStuckTimeout, err = durationFromEnv("WORKER_STUCK_TIMEOUT", defaultWorkerStuckTimeout); err != nil {
		return Config{}, err
	}
	if cfg.WorkerRecoveryInterval, err = durationFromEnv("WORKER_RECOVERY_INTERVAL", defaultWorkerRecoveryInterval); err != nil {
		return Config{}, err
	}
	if cfg.WorkerMaxAttempts, err = positiveIntFromEnv("WORKER_MAX_ATTEMPTS", defaultWorkerMaxAttempts); err != nil {
		return Config{}, err
	}

	if cfg.IdempotencyTTL, err = durationFromEnv("IDEMPOTENCY_TTL", defaultIdempotencyTTL); err != nil {
		return Config{}, err
	}
	if cfg.IdempotencyCleanupInterval, err = durationFromEnv("IDEMPOTENCY_CLEANUP_INTERVAL", defaultIdempotencyCleanupInterval); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// CheckTimeouts rejects a combination of settings that cannot hold together, so
// that the service refuses to start rather than misbehave later: a write
// timeout with no room for a handler deadline under it would leave requests
// unbounded, a task deadline too short for a full retry schedule would cut the
// provider off halfway through every slow call, and a staleness threshold too
// close to that deadline would have the recovery pass take tasks away from
// workers that are still working on them, and a sweep of idempotency keys
// rarer than the lifetime it is there to enforce would quietly multiply it.
//
// providerBudget is the longest a single provider call can take, retries and
// backoff included. It is passed in rather than worked out here so that this
// package depends on nothing of its own: the formula belongs to the package
// that owns the retry schedule, and the caller that builds a provider is
// holding both. Load does not call this -- main does, once, right after it.
func (c Config) CheckTimeouts(providerBudget time.Duration) error {
	if c.HTTPWriteTimeout <= handlerTimeoutMargin {
		return fmt.Errorf(
			"HTTP_WRITE_TIMEOUT: %s leaves nothing above the %s reserved for writing the answer",
			c.HTTPWriteTimeout, handlerTimeoutMargin,
		)
	}

	if c.WorkerTaskTimeout <= providerBudget {
		return fmt.Errorf(
			"WORKER_TASK_TIMEOUT: %s does not cover the provider budget of %s, leaving nothing for the finalising transaction",
			c.WorkerTaskTimeout, providerBudget,
		)
	}

	if minStuck := stuckTimeoutMargin * c.WorkerTaskTimeout; c.WorkerStuckTimeout < minStuck {
		return fmt.Errorf(
			"WORKER_STUCK_TIMEOUT: %s is less than %s, which is %d times WORKER_TASK_TIMEOUT",
			c.WorkerStuckTimeout, minStuck, stuckTimeoutMargin,
		)
	}

	// A sweep rarer than the lifetime it enforces would not shorten a binding,
	// it would stretch it: nothing else expires one, so the interval is the
	// error bar on the whole setting, and it is only meaningful below it.
	if c.IdempotencyCleanupInterval >= c.IdempotencyTTL {
		return fmt.Errorf(
			"IDEMPOTENCY_CLEANUP_INTERVAL: %s is not shorter than the IDEMPOTENCY_TTL of %s it enforces",
			c.IdempotencyCleanupInterval, c.IdempotencyTTL,
		)
	}

	return nil
}

// HandlerTimeout is the deadline of a single request, bounding everything a
// handler does: it is what stops a query against an unreachable database from
// holding a goroutine and a pooled connection for as long as the outage lasts.
//
// Derived rather than configured. It has to stay strictly under
// HTTP_WRITE_TIMEOUT, which is the point at which the server stops writing at
// all, and a variable of its own would let the two be set the wrong way round.
// CheckTimeouts is what guarantees the result is positive.
func (c Config) HandlerTimeout() time.Duration {
	return c.HTTPWriteTimeout - handlerTimeoutMargin
}

// requiredFromEnv reads a variable that has no meaningful default. Connecting
// to some arbitrary fallback database is worse than refusing to start.
func requiredFromEnv(key string) (string, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return "", fmt.Errorf("%s is required", key)
	}

	return raw, nil
}

func stringFromEnv(key, def string) string {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return def
	}

	return raw
}

func intFromEnv(key string, def int) (int, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return def, nil
	}

	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}

	return v, nil
}

// positiveIntFromEnv reads a count. Zero is rejected rather than taken
// literally: a pool of no workers, or a provider allowed no attempts, is a
// service that quietly does nothing.
func positiveIntFromEnv(key string, def int) (int, error) {
	v, err := intFromEnv(key, def)
	if err != nil {
		return 0, err
	}
	if v < 1 {
		return 0, fmt.Errorf("%s: %d is not a positive count", key, v)
	}

	return v, nil
}

func durationFromEnv(key string, def time.Duration) (time.Duration, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return def, nil
	}

	v, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	// Go reads a non-positive timeout as "no timeout at all", so an attempt to
	// tighten the setting would silently disable it instead.
	if v <= 0 {
		return 0, fmt.Errorf("%s: %s is not a positive duration", key, raw)
	}

	return v, nil
}

func levelFromEnv(key string, def slog.Level) (slog.Level, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return def, nil
	}

	var v slog.Level
	if err := v.UnmarshalText([]byte(raw)); err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}

	return v, nil
}
