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

	defaultIdempotencyTTL = time.Hour
)

// Valid TCP port range. Port 0 is excluded: it would make the server pick an
// arbitrary free port, which cannot match the published compose port.
const (
	minPort = 1
	maxPort = 65535
)

// Kept back from the request deadline so that a handler which runs out of time
// still has room to write its answer. Without the gap the two deadlines expire
// together and the server drops the connection in the middle of the very error
// the deadline was meant to produce.
const handlerTimeoutMargin = time.Second

// How many task deadlines a task may spend in_progress before the recovery pass
// treats it as abandoned. Plain "longer than one deadline" would not do: the
// deadline is measured by this process, the staleness threshold by the database
// clock, and between the claim commit and the start of the deadline lie things
// the deadline never counted. Erring low takes tasks away from workers that are
// still alive.
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

	// Scheme and host without a path: the path belongs to the client that knows
	// the API.
	ProviderBaseURL string
	// Bounds one HTTP attempt, not the whole retry schedule.
	ProviderTimeout  time.Duration
	ProviderAttempts int
	// The pause before the second attempt, doubling before each after it.
	ProviderBackoff time.Duration

	WorkerConcurrency  int
	WorkerPollInterval time.Duration
	// Covers the whole path from the claim to the finalising commit.
	WorkerTaskTimeout time.Duration
	// See stuckTimeoutMargin for why it cannot simply be the task deadline.
	WorkerStuckTimeout     time.Duration
	WorkerRecoveryInterval time.Duration
	WorkerMaxAttempts      int

	// Also the longest a post can silently perform no update at all, which is
	// why it is far shorter than the day such keys are conventionally kept: a
	// client repeating a live key is handed the earlier task rather than a fresh
	// rate.
	IdempotencyTTL time.Duration
}

// Unset variables fall back to defaults; a malformed value is an error naming
// the variable.
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

	if err := cfg.check(); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// check rejects a combination of settings that cannot hold together, so that
// the service refuses to start rather than misbehave later: a write timeout
// with no room for a handler deadline under it would leave requests unbounded,
// and a staleness threshold too close to the task deadline would have the
// recovery pass take tasks away from workers that are still working on them.
func (c Config) check() error {
	if c.HTTPWriteTimeout <= handlerTimeoutMargin {
		return fmt.Errorf(
			"HTTP_WRITE_TIMEOUT: %s leaves nothing above the %s reserved for writing the answer",
			c.HTTPWriteTimeout, handlerTimeoutMargin,
		)
	}

	if minStuck := stuckTimeoutMargin * c.WorkerTaskTimeout; c.WorkerStuckTimeout < minStuck {
		return fmt.Errorf(
			"WORKER_STUCK_TIMEOUT: %s is less than %s, which is %d times WORKER_TASK_TIMEOUT",
			c.WorkerStuckTimeout, minStuck, stuckTimeoutMargin,
		)
	}

	return nil
}

// Has to stay strictly under HTTP_WRITE_TIMEOUT, which check is what guarantees.
func (c Config) HandlerTimeout() time.Duration {
	return c.HTTPWriteTimeout - handlerTimeoutMargin
}

// Connecting to some arbitrary fallback database is worse than refusing to
// start.
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

// Zero is rejected rather than taken literally: a pool of no workers, or a
// provider allowed no attempts, is a service that quietly does nothing.
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
