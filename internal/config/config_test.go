package config

import (
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The two invariants the service refuses to start without. Load runs them, so
// a case here states what has to be rejected without having to reach for the
// environment. The numbers are the configured defaults, so a change to them
// shows up as a failure with a name on it.
func TestConfigCheckTimeouts(t *testing.T) {
	tests := []struct {
		name         string
		writeTimeout time.Duration
		taskTimeout  time.Duration
		stuckTimeout time.Duration
		// wantErr is the variable the failure has to name; empty means the
		// combination must be accepted.
		wantErr string
	}{
		{
			name:         "the configured defaults hold",
			writeTimeout: 10 * time.Second,
			taskTimeout:  15 * time.Second,
			stuckTimeout: time.Minute,
		},
		{
			// Equal leaves a handler deadline of zero, which time.Context
			// treats as already expired rather than as unlimited.
			name:         "a write timeout equal to the margin leaves no deadline",
			writeTimeout: handlerTimeoutMargin,
			taskTimeout:  15 * time.Second,
			stuckTimeout: time.Minute,
			wantErr:      "HTTP_WRITE_TIMEOUT",
		},
		{
			name:         "the smallest write timeout above the margin is accepted",
			writeTimeout: handlerTimeoutMargin + time.Millisecond,
			taskTimeout:  15 * time.Second,
			stuckTimeout: time.Minute,
		},
		{
			name:         "a staleness threshold without a margin is refused",
			writeTimeout: 10 * time.Second,
			taskTimeout:  15 * time.Second,
			stuckTimeout: 29 * time.Second,
			wantErr:      "WORKER_STUCK_TIMEOUT",
		},
		{
			name:         "twice the deadline is the smallest margin accepted",
			writeTimeout: 10 * time.Second,
			taskTimeout:  15 * time.Second,
			stuckTimeout: 30 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{
				HTTPWriteTimeout:   tt.writeTimeout,
				WorkerTaskTimeout:  tt.taskTimeout,
				WorkerStuckTimeout: tt.stuckTimeout,
			}

			err := cfg.check()

			if tt.wantErr == "" {
				require.NoError(t, err)

				return
			}

			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

// The relation the request deadline is derived by, since nothing downstream of
// it can tell a deadline that is too generous from one that is right: the
// difference only shows as a connection dropped mid-answer under load.
func TestConfigHandlerTimeout(t *testing.T) {
	cfg := Config{HTTPWriteTimeout: 10 * time.Second}

	require.Equal(t, 10*time.Second-handlerTimeoutMargin, cfg.HandlerTimeout())
	require.Less(t, cfg.HandlerTimeout(), cfg.HTTPWriteTimeout)
}

// A fourth copy of the names would be the copy that decides whether the other
// three are checked at all: a variable forgotten in it is a variable no test
// asks about.
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

// t.Setenv first, for the cleanup it registers, then os.Unsetenv: an empty
// value and an absent one travel different branches, and this is the absent
// one.
func clearEnv(t *testing.T) {
	t.Helper()

	for _, name := range serviceVariables(t) {
		t.Setenv(name, "")
		require.NoError(t, os.Unsetenv(name))
	}
}

// testDatabaseDSN stands in for the one variable that has no default, so that
// a test about anything else can still get a configuration out of Load.
const testDatabaseDSN = "postgres://user:pass@localhost:5432/db?sslmode=disable"

// A name mapped to an empty string is left out of the environment entirely.
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

// Every default to a number. They are the values the service runs with
// whenever a variable is left out, which is the ordinary case rather than the
// exceptional one, so a default that drifts is a change of behaviour with
// nothing else announcing it.
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

	// Load itself runs the checks, so reaching this point with no error is
	// already the assertion that the defaults hold together.
}

// What eighteen near-identical lines are worth a test for. Every value below
// is distinct, so a field reading its neighbour's variable comes back holding
// the neighbour's value -- a mistake that compiles, that no other test in the
// project would notice, and that would surface as a recovery pass running on
// the wrong clock.
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

		"WORKER_CONCURRENCY":   "17",
		"WORKER_POLL_INTERVAL": "18s",
		"WORKER_TASK_TIMEOUT":  "19s",
		// Out of the ascending sequence the rest of this map follows: Load now
		// runs the checks, and the staleness threshold has to be at least twice
		// the task deadline above. Still a value no other variable here holds.
		"WORKER_STUCK_TIMEOUT":     "40s",
		"WORKER_RECOVERY_INTERVAL": "21s",
		"WORKER_MAX_ATTEMPTS":      "22",

		"IDEMPOTENCY_TTL": "23s",
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
		WorkerStuckTimeout:     40 * time.Second,
		WorkerRecoveryInterval: 21 * time.Second,
		WorkerMaxAttempts:      22,

		IdempotencyTTL: 23 * time.Second,
	}, cfg)
}

// The one variable with no default. Falling back to some arbitrary connection
// string would start the service against a database nobody meant, which is
// worse than not starting at all.
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

// The shape a variable arrives in through compose: HTTP_PORT:
// ${HTTP_PORT:-8080} passes an empty string straight through when .env carries
// a bare "HTTP_PORT=". Read literally that would be a port of zero and a
// timeout of none.
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

// What the service refuses to start on.
//
// Two of these are traps rather than typos. A duration of zero is no timeout
// at all, which lifts the invariant that every outbound call is bounded; a
// count of zero is a pool of no workers, a service that starts, answers its
// healthcheck and quietly does nothing.
//
// Every failure has to name its variable, or the operator is left to find out
// which of eighteen it was.
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

// The two ends stay inside. Port zero is excluded on purpose -- it makes the
// server pick an arbitrary free port, which cannot match the one compose
// published -- and that exclusion is what makes the lower bound worth stating.
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

// The names an operator is likely to write. The level is parsed by slog rather
// than by this package, and what it accepts is worth stating: a deployment
// that set LOG_LEVEL=DEBUG and got a refusal would be a surprise, and one that
// silently got info would be a worse one.
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

// The defaults of this service exist in three places: the constants above, the
// values in .env.example and the substitutions in docker-compose.yml. Two of
// those are documentation of the third, and nothing but the tests below keeps
// any of them in step -- the omission has already happened once, when the
// idempotency settings reached the code and .env.example but not the
// container.
//
// Both checks apply what the file says to the environment and require the
// result to be what an empty environment produces. Comparing configurations
// rather than strings means neither test knows how a duration is spelled.
const (
	envExamplePath = "../../.env.example"
	composePath    = "../../docker-compose.yml"
)

// The last of those is how the file marks a variable that has no default at
// all.
func envFileDefaults(t *testing.T) map[string]string {
	t.Helper()

	content, err := os.ReadFile(envExamplePath)
	require.NoError(t, err)

	values := make(map[string]string)

	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok || value == "" {
			continue
		}

		values[key] = value
	}

	return values
}

// ${NAME:-value}" substitutions out of docker-compose.yml. Only that exact
// shape, which is what makes DATABASE_URL fall out on its own: it is assembled
// there from the POSTGRES_* values rather than defaulted, so it has no default
// of its own to compare.
func composeDefaults(t *testing.T) map[string]string {
	t.Helper()

	content, err := os.ReadFile(composePath)
	require.NoError(t, err)

	pattern := regexp.MustCompile(`(?m)^\s*([A-Z][A-Z0-9_]*):\s*\$\{([A-Z][A-Z0-9_]*):-([^}]*)\}\s*$`)

	values := make(map[string]string)

	for _, m := range pattern.FindAllStringSubmatch(string(content), -1) {
		// A variable passed into the container under one name and defaulted
		// from another would be a different setting wearing its label.
		if m[1] != m[2] {
			continue
		}

		values[m[1]] = m[3]
	}

	return values
}

func requireLoadsToTheDefaults(t *testing.T, values map[string]string) {
	t.Helper()

	defaults, err := loadWith(t, map[string]string{"DATABASE_URL": testDatabaseDSN})
	require.NoError(t, err)

	withValues := make(map[string]string, len(values)+1)
	for name, value := range values {
		withValues[name] = value
	}
	// The one variable without a default is held the same on both sides, so
	// that the comparison is about everything else.
	withValues["DATABASE_URL"] = testDatabaseDSN

	got, err := loadWith(t, withValues)
	require.NoError(t, err)

	require.Equal(t, defaults, got)
}

// The claim the file opens with -- that its values mirror the ones compiled
// in. A reader who copies it to .env has to get the service the defaults
// describe, and an operator reading it to find out what a setting currently is
// has to be reading the truth.
func TestEnvExampleMatchesTheDefaults(t *testing.T) {
	requireLoadsToTheDefaults(t, envFileDefaults(t))
}

// The half the comparison above cannot make. A variable missing from the file
// loads its default on both sides and the values agree, so only the names can
// catch it -- and a setting that exists but is written down nowhere is one
// nobody knows to reach for.
func TestEnvExampleListsEveryVariable(t *testing.T) {
	documented := envFileDefaults(t)

	for _, name := range serviceVariables(t) {
		// DATABASE_URL has no default, and the file carries it as an example
		// rather than as one; it is listed all the same, which is what this
		// asks about.
		require.Containsf(t, documented, name, "%s is read by Load but not listed in .env.example", name)
	}
}

// The copy that actually runs. The defaults are repeated in the compose file
// so that a clean clone comes up without an .env beside it, which means an
// operator reading either file has to find the same service described.
func TestComposeMatchesTheDefaults(t *testing.T) {
	requireLoadsToTheDefaults(t, composeDefaults(t))
}

// The omission that has already happened: a setting added to the code and to
// .env.example, and forgotten in the compose file, leaves the container
// running on a default nobody chose while both documents say otherwise.
func TestComposePassesEveryVariable(t *testing.T) {
	passed := composeDefaults(t)

	for _, name := range serviceVariables(t) {
		// Assembled from the POSTGRES_* values rather than defaulted, so it is
		// deliberately not of the shape this test reads.
		if name == "DATABASE_URL" {
			continue
		}

		require.Containsf(t, passed, name, "%s is read by Load but not passed to the container", name)
	}
}
