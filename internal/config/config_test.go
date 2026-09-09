package config

import (
	"cmp"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestConfigCheckTimeouts pins the four invariants the service refuses to
// start without. They are worth a test of their own now that Load no longer
// runs them: the check is one call in main away from being forgotten, and it is
// cheap to state here what it must reject. The numbers are the configured
// defaults, so a change to them shows up as a failure with a name on it.
func TestConfigCheckTimeouts(t *testing.T) {
	tests := []struct {
		name           string
		writeTimeout   time.Duration
		providerBudget time.Duration
		taskTimeout    time.Duration
		stuckTimeout   time.Duration
		// The sweep of idempotency keys and the lifetime it enforces. Left at
		// zero by every case that is about one of the other three rules, which
		// the fixture then fills with a valid pair: a case states the rule it
		// is about and stays silent on the rest.
		cleanupInterval time.Duration
		idempotencyTTL  time.Duration
		// wantErr is the variable the failure has to name; empty means the
		// combination must be accepted.
		wantErr string
	}{
		{
			name:           "the configured defaults hold",
			writeTimeout:   10 * time.Second,
			providerBudget: 9900 * time.Millisecond,
			taskTimeout:    15 * time.Second,
			stuckTimeout:   time.Minute,
		},
		{
			// Equal leaves a handler deadline of zero, which time.Context
			// treats as already expired rather than as unlimited.
			name:           "a write timeout equal to the margin leaves no deadline",
			writeTimeout:   handlerTimeoutMargin,
			providerBudget: 5 * time.Second,
			taskTimeout:    15 * time.Second,
			stuckTimeout:   time.Minute,
			wantErr:        "HTTP_WRITE_TIMEOUT",
		},
		{
			name:           "the smallest write timeout above the margin is accepted",
			writeTimeout:   handlerTimeoutMargin + time.Millisecond,
			providerBudget: 5 * time.Second,
			taskTimeout:    15 * time.Second,
			stuckTimeout:   time.Minute,
		},
		{
			name:           "a deadline shorter than the budget is refused",
			writeTimeout:   10 * time.Second,
			providerBudget: 10 * time.Second,
			taskTimeout:    9 * time.Second,
			stuckTimeout:   time.Minute,
			wantErr:        "WORKER_TASK_TIMEOUT",
		},
		{
			// Equal is not enough: the deadline covers the finalising
			// transaction as well as the call.
			name:           "a deadline equal to the budget leaves nothing for the commit",
			writeTimeout:   10 * time.Second,
			providerBudget: 10 * time.Second,
			taskTimeout:    10 * time.Second,
			stuckTimeout:   time.Minute,
			wantErr:        "WORKER_TASK_TIMEOUT",
		},
		{
			name:           "a staleness threshold without a margin is refused",
			writeTimeout:   10 * time.Second,
			providerBudget: 5 * time.Second,
			taskTimeout:    15 * time.Second,
			stuckTimeout:   29 * time.Second,
			wantErr:        "WORKER_STUCK_TIMEOUT",
		},
		{
			name:           "twice the deadline is the smallest margin accepted",
			writeTimeout:   10 * time.Second,
			providerBudget: 5 * time.Second,
			taskTimeout:    15 * time.Second,
			stuckTimeout:   30 * time.Second,
		},
		{
			// Equal is already wrong: the sweep is the only thing that ends a
			// binding, so an interval that matches the lifetime doubles it.
			name:            "a sweep as rare as the lifetime it enforces is refused",
			writeTimeout:    10 * time.Second,
			providerBudget:  5 * time.Second,
			taskTimeout:     15 * time.Second,
			stuckTimeout:    time.Minute,
			cleanupInterval: time.Hour,
			idempotencyTTL:  time.Hour,
			wantErr:         "IDEMPOTENCY_CLEANUP_INTERVAL",
		},
		{
			name:            "the configured sweep and lifetime hold",
			writeTimeout:    10 * time.Second,
			providerBudget:  5 * time.Second,
			taskTimeout:     15 * time.Second,
			stuckTimeout:    time.Minute,
			cleanupInterval: defaultIdempotencyCleanupInterval,
			idempotencyTTL:  defaultIdempotencyTTL,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{
				HTTPWriteTimeout:   tt.writeTimeout,
				WorkerTaskTimeout:  tt.taskTimeout,
				WorkerStuckTimeout: tt.stuckTimeout,
				// A valid pair stands in wherever a case did not care, so that
				// the rule under test is the only one that can fail it.
				IdempotencyCleanupInterval: cmp.Or(tt.cleanupInterval, time.Minute),
				IdempotencyTTL:             cmp.Or(tt.idempotencyTTL, time.Hour),
			}

			err := cfg.CheckTimeouts(tt.providerBudget)

			if tt.wantErr == "" {
				require.NoError(t, err)

				return
			}

			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

// TestConfigHandlerTimeout states the relation the request deadline is derived
// by, since nothing downstream of it can tell a deadline that is too generous
// from one that is right: the difference only shows as a connection dropped
// mid-answer under load.
func TestConfigHandlerTimeout(t *testing.T) {
	cfg := Config{HTTPWriteTimeout: 10 * time.Second}

	require.Equal(t, 10*time.Second-handlerTimeoutMargin, cfg.HandlerTimeout())
	require.Less(t, cfg.HandlerTimeout(), cfg.HTTPWriteTimeout)
}

// serviceVariables is every environment variable Load reads, taken from the
// source rather than from a list kept here.
//
// The names already exist in three places -- this package, .env.example and
// docker-compose.yml -- and the tests below are about those three agreeing. A
// fourth copy written out here would be one more thing to keep in step, and it
// would be the copy that decides whether the others are checked at all: a
// variable forgotten in it is a variable no test asks about.
func serviceVariables(t *testing.T) []string {
	t.Helper()

	source, err := os.ReadFile("config.go")
	require.NoError(t, err)

	// The one shape every read has: a helper named *FromEnv taking the name as
	// a literal.
	pattern := regexp.MustCompile(`\w+FromEnv\("([A-Z][A-Z0-9_]*)"`)

	matches := pattern.FindAllStringSubmatch(string(source), -1)
	require.NotEmpty(t, matches, "no environment variables found in config.go")

	names := make([]string, 0, len(matches))
	for _, m := range matches {
		names = append(names, m[1])
	}

	return names
}

// clearEnv takes every service variable out of the environment, so that a test
// starts from a known state rather than from whatever the shell running it
// happens to hold.
//
// t.Setenv first, for the cleanup it registers, and os.Unsetenv after it: an
// empty value and an absent one travel different branches of the helpers, and
// this is the absent one.
func clearEnv(t *testing.T) {
	t.Helper()

	for _, name := range serviceVariables(t) {
		t.Setenv(name, "")
		require.NoError(t, os.Unsetenv(name))
	}
}

// testDatabaseDSN stands in for the one variable that has no default, so that a
// test about anything else can still get a configuration out of Load.
const testDatabaseDSN = "postgres://user:pass@localhost:5432/db?sslmode=disable"

// loadWith runs Load over env and nothing else. A name mapped to an empty
// string is left out of the environment entirely.
func loadWith(t *testing.T, env map[string]string) (Config, error) {
	t.Helper()

	clearEnv(t)

	for name, value := range env {
		if value == "" {
			continue
		}

		t.Setenv(name, value)
	}

	return Load()
}

// TestLoadDefaults pins every default to a number. They are the values the
// service runs with whenever a variable is left out, which is the ordinary case
// rather than the exceptional one, so a default that drifts is a change of
// behaviour with nothing else announcing it.
func TestLoadDefaults(t *testing.T) {
	cfg, err := loadWith(t, map[string]string{"DATABASE_URL": testDatabaseDSN})
	require.NoError(t, err)

	require.Equal(t, testDatabaseDSN, cfg.DatabaseURL)

	require.Equal(t, defaultHTTPPort, cfg.HTTPPort)
	require.Equal(t, defaultHTTPReadTimeout, cfg.HTTPReadTimeout)
	require.Equal(t, defaultHTTPWriteTimeout, cfg.HTTPWriteTimeout)
	require.Equal(t, defaultShutdownTimeout, cfg.ShutdownTimeout)
	require.Equal(t, defaultLogLevel, cfg.LogLevel)

	require.Equal(t, defaultProviderBaseURL, cfg.ProviderBaseURL)
	require.Equal(t, defaultProviderTimeout, cfg.ProviderTimeout)
	require.Equal(t, defaultProviderAttempts, cfg.ProviderAttempts)
	require.Equal(t, defaultProviderBackoff, cfg.ProviderBackoff)

	require.Equal(t, defaultWorkerConcurrency, cfg.WorkerConcurrency)
	require.Equal(t, defaultWorkerPollInterval, cfg.WorkerPollInterval)
	require.Equal(t, defaultWorkerTaskTimeout, cfg.WorkerTaskTimeout)
	require.Equal(t, defaultWorkerStuckTimeout, cfg.WorkerStuckTimeout)
	require.Equal(t, defaultWorkerRecoveryInterval, cfg.WorkerRecoveryInterval)
	require.Equal(t, defaultWorkerMaxAttempts, cfg.WorkerMaxAttempts)

	require.Equal(t, defaultIdempotencyTTL, cfg.IdempotencyTTL)
	require.Equal(t, defaultIdempotencyCleanupInterval, cfg.IdempotencyCleanupInterval)

	// The defaults have to survive the checks the service starts with, or an
	// empty environment is one the service refuses to run in. The budget is
	// written out rather than imported from the provider package, which this
	// one deliberately does not depend on; the figure is the one pinned by
	// TestBudget there, for these same three defaults.
	require.NoError(t, cfg.CheckTimeouts(9900*time.Millisecond))
}

// TestLoadReadsEveryVariableIntoItsOwnField is what eighteen near-identical
// lines are worth a test for. Every value below is distinct, so a field reading
// its neighbour's variable comes back holding the neighbour's value -- a
// mistake that compiles, that no other test in the project would notice, and
// that would surface as a recovery pass running on the wrong clock.
func TestLoadReadsEveryVariableIntoItsOwnField(t *testing.T) {
	cfg, err := loadWith(t, map[string]string{
		"DATABASE_URL":       testDatabaseDSN,
		"HTTP_PORT":          "9091",
		"HTTP_READ_TIMEOUT":  "11s",
		"HTTP_WRITE_TIMEOUT": "12s",
		"SHUTDOWN_TIMEOUT":   "13s",
		"LOG_LEVEL":          "debug",

		"PROVIDER_BASE_URL": "http://localhost:9999",
		"PROVIDER_TIMEOUT":  "14s",
		"PROVIDER_ATTEMPTS": "15",
		"PROVIDER_BACKOFF":  "16s",

		"WORKER_CONCURRENCY":       "17",
		"WORKER_POLL_INTERVAL":     "18s",
		"WORKER_TASK_TIMEOUT":      "19s",
		"WORKER_STUCK_TIMEOUT":     "20s",
		"WORKER_RECOVERY_INTERVAL": "21s",
		"WORKER_MAX_ATTEMPTS":      "22",

		"IDEMPOTENCY_TTL":              "23s",
		"IDEMPOTENCY_CLEANUP_INTERVAL": "24s",
	})
	require.NoError(t, err)

	require.Equal(t, Config{
		DatabaseURL:      testDatabaseDSN,
		HTTPPort:         9091,
		HTTPReadTimeout:  11 * time.Second,
		HTTPWriteTimeout: 12 * time.Second,
		ShutdownTimeout:  13 * time.Second,
		LogLevel:         slog.LevelDebug,

		ProviderBaseURL:  "http://localhost:9999",
		ProviderTimeout:  14 * time.Second,
		ProviderAttempts: 15,
		ProviderBackoff:  16 * time.Second,

		WorkerConcurrency:      17,
		WorkerPollInterval:     18 * time.Second,
		WorkerTaskTimeout:      19 * time.Second,
		WorkerStuckTimeout:     20 * time.Second,
		WorkerRecoveryInterval: 21 * time.Second,
		WorkerMaxAttempts:      22,

		IdempotencyTTL:             23 * time.Second,
		IdempotencyCleanupInterval: 24 * time.Second,
	}, cfg)
}

// TestLoadRequiresTheDatabaseURL covers the one variable with no default.
// Falling back to some arbitrary connection string would start the service
// against a database nobody meant, which is worse than not starting at all.
func TestLoadRequiresTheDatabaseURL(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		_, err := loadWith(t, nil)

		require.ErrorContains(t, err, "DATABASE_URL")
	})

	t.Run("present but empty", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("DATABASE_URL", "")

		_, err := Load()

		require.ErrorContains(t, err, "DATABASE_URL")
	})
}

// TestLoadTreatsAnEmptyValueAsUnset covers the shape a variable arrives in
// through compose: HTTP_PORT: ${HTTP_PORT:-8080} passes an empty string
// straight through when .env carries a bare "HTTP_PORT=". Read literally that
// would be a port of zero and a timeout of none.
func TestLoadTreatsAnEmptyValueAsUnset(t *testing.T) {
	clearEnv(t)

	t.Setenv("DATABASE_URL", testDatabaseDSN)
	t.Setenv("HTTP_PORT", "")
	t.Setenv("PROVIDER_TIMEOUT", "")
	t.Setenv("WORKER_CONCURRENCY", "")
	t.Setenv("LOG_LEVEL", "")
	t.Setenv("PROVIDER_BASE_URL", "")

	cfg, err := Load()

	require.NoError(t, err)
	require.Equal(t, defaultHTTPPort, cfg.HTTPPort)
	require.Equal(t, defaultProviderTimeout, cfg.ProviderTimeout)
	require.Equal(t, defaultWorkerConcurrency, cfg.WorkerConcurrency)
	require.Equal(t, defaultLogLevel, cfg.LogLevel)
	require.Equal(t, defaultProviderBaseURL, cfg.ProviderBaseURL)
}

// TestLoadRejectsBadValues covers what the service refuses to start on.
//
// Two of these are traps rather than typos. A duration of zero is not a
// tighter timeout but no timeout at all, which is how an invariant of the
// project -- every outbound call is bounded -- would be lifted by a setting
// that looks like it does the opposite. A count of zero is a pool of no
// workers, or a provider allowed no attempts: a service that starts, answers
// its healthcheck and quietly does nothing.
//
// Every failure has to name its variable. With eighteen of them, an error
// saying only that a value is malformed leaves the operator to find out which.
func TestLoadRejectsBadValues(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{name: "a port that is not a number", key: "HTTP_PORT", value: "eighty"},
		{name: "a port below the range", key: "HTTP_PORT", value: "0"},
		{name: "a port above the range", key: "HTTP_PORT", value: "65536"},
		{name: "a port that is negative", key: "HTTP_PORT", value: "-1"},

		{name: "a duration without a unit", key: "HTTP_READ_TIMEOUT", value: "5"},
		{name: "a duration that is not one", key: "SHUTDOWN_TIMEOUT", value: "soon"},
		{name: "a timeout of zero", key: "PROVIDER_TIMEOUT", value: "0s"},
		{name: "a timeout that is negative", key: "WORKER_TASK_TIMEOUT", value: "-1s"},
		{name: "a poll interval of zero", key: "WORKER_POLL_INTERVAL", value: "0"},
		{name: "a write timeout without a unit", key: "HTTP_WRITE_TIMEOUT", value: "10"},
		{name: "a backoff that is negative", key: "PROVIDER_BACKOFF", value: "-200ms"},
		{name: "a staleness threshold of zero", key: "WORKER_STUCK_TIMEOUT", value: "0"},
		{name: "a recovery interval spelled out", key: "WORKER_RECOVERY_INTERVAL", value: "half a minute"},
		{name: "a key lifetime with a space in it", key: "IDEMPOTENCY_TTL", value: "1 hour"},
		{name: "a sweep interval of zero", key: "IDEMPOTENCY_CLEANUP_INTERVAL", value: "0s"},

		{name: "a count that is not a number", key: "PROVIDER_ATTEMPTS", value: "three"},
		{name: "no attempts at all", key: "PROVIDER_ATTEMPTS", value: "0"},
		{name: "a pool of no workers", key: "WORKER_CONCURRENCY", value: "0"},
		{name: "a negative attempt limit", key: "WORKER_MAX_ATTEMPTS", value: "-1"},

		{name: "a log level that is not one", key: "LOG_LEVEL", value: "chatty"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadWith(t, map[string]string{
				"DATABASE_URL": testDatabaseDSN,
				tt.key:         tt.value,
			})

			require.Error(t, err)
			require.ErrorContains(t, err, tt.key)
		})
	}
}

// TestLoadAcceptsTheEdgesOfThePortRange checks that the two ends stay inside.
// Port zero is excluded on purpose -- it makes the server pick an arbitrary
// free port, which cannot match the one compose published -- and that
// exclusion is what makes the lower bound worth stating.
func TestLoadAcceptsTheEdgesOfThePortRange(t *testing.T) {
	for _, port := range []int{minPort, maxPort} {
		t.Run(strconv.Itoa(port), func(t *testing.T) {
			cfg, err := loadWith(t, map[string]string{
				"DATABASE_URL": testDatabaseDSN,
				"HTTP_PORT":    strconv.Itoa(port),
			})

			require.NoError(t, err)
			require.Equal(t, port, cfg.HTTPPort)
		})
	}
}

// TestLoadLogLevelSpellings checks the names an operator is likely to write.
// The level is parsed by slog rather than by this package, and what it accepts
// is worth stating: a deployment that set LOG_LEVEL=DEBUG and got a refusal
// would be a surprise, and one that silently got info would be a worse one.
func TestLoadLogLevelSpellings(t *testing.T) {
	tests := []struct {
		value string
		want  slog.Level
	}{
		{value: "debug", want: slog.LevelDebug},
		{value: "DEBUG", want: slog.LevelDebug},
		{value: "info", want: slog.LevelInfo},
		{value: "warn", want: slog.LevelWarn},
		{value: "error", want: slog.LevelError},
	}

	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			cfg, err := loadWith(t, map[string]string{
				"DATABASE_URL": testDatabaseDSN,
				"LOG_LEVEL":    tt.value,
			})

			require.NoError(t, err)
			require.Equal(t, tt.want, cfg.LogLevel)
		})
	}
}
