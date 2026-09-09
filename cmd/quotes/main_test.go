package main

import (
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// unreachableDSN points at a port nothing listens on, so a connection attempt
// is refused at once rather than waiting out a timeout.
const unreachableDSN = "postgres://quotes:quotes@127.0.0.1:1/quotes?sslmode=disable"

// workingEnv is a configuration the service would start on, which the tests
// below then break in one place each.
//
// Written out in full rather than inherited, so that the environment of
// whoever runs the tests cannot decide their outcome: a stray LOG_LEVEL left
// over from a debugging session would otherwise fail Load and make every case
// here fail for the wrong reason. Incompleteness is harmless -- a variable
// missing from this map falls back to its default, which is what an empty
// environment gives anyway.
func workingEnv() map[string]string {
	return map[string]string{
		"DATABASE_URL":       unreachableDSN,
		"HTTP_PORT":          "8080",
		"HTTP_READ_TIMEOUT":  "5s",
		"HTTP_WRITE_TIMEOUT": "10s",
		"SHUTDOWN_TIMEOUT":   "10s",
		"LOG_LEVEL":          "error",

		"PROVIDER_BASE_URL": "http://127.0.0.1:1",
		"PROVIDER_TIMEOUT":  "3s",
		"PROVIDER_ATTEMPTS": "3",
		"PROVIDER_BACKOFF":  "200ms",

		"WORKER_CONCURRENCY":       "4",
		"WORKER_POLL_INTERVAL":     "1s",
		"WORKER_TASK_TIMEOUT":      "15s",
		"WORKER_STUCK_TIMEOUT":     "60s",
		"WORKER_RECOVERY_INTERVAL": "30s",
		"WORKER_MAX_ATTEMPTS":      "3",

		"IDEMPOTENCY_TTL":              "1h",
		"IDEMPOTENCY_CLEANUP_INTERVAL": "10m",
	}
}

// setEnv applies env, taking a name mapped to an empty string out of the
// environment entirely rather than setting it to nothing: the two are
// different to the configuration, and the tests below need the first.
func setEnv(t *testing.T, env map[string]string) {
	t.Helper()

	for name, value := range env {
		t.Setenv(name, value)

		if value == "" {
			require.NoError(t, os.Unsetenv(name))
		}
	}
}

// TestRunRefusesAnIncompleteConfiguration checks that the service does not
// start without the one variable that has no default. Connecting to some
// arbitrary fallback database is worse than not starting, and this is the last
// point at which that is still a choice.
func TestRunRefusesAnIncompleteConfiguration(t *testing.T) {
	env := workingEnv()
	env["DATABASE_URL"] = ""

	setEnv(t, env)

	err := run()

	require.Error(t, err)
	require.ErrorContains(t, err, "load config")
	require.ErrorContains(t, err, "DATABASE_URL")
}

// TestRunChecksTheTimeoutsBeforeStarting is the only test that the check is
// called at all.
//
// Load deliberately does not run it -- the budget it compares against belongs
// to the provider package, which the configuration must not depend on -- so
// the call lives here, in a function nothing else exercises. Dropped, the
// service would start on settings it is known not to work under: a task
// deadline that cuts every slow provider call short, or a staleness threshold
// that takes tasks away from workers still holding them.
//
// The database is unreachable in this environment, so reaching the migration
// would fail too. That it fails earlier, and says so, is the assertion.
func TestRunChecksTheTimeoutsBeforeStarting(t *testing.T) {
	env := workingEnv()
	// Equal to the margin kept back for writing an answer, which leaves a
	// request deadline of zero.
	env["HTTP_WRITE_TIMEOUT"] = "1s"

	setEnv(t, env)

	err := run()

	require.Error(t, err)
	require.ErrorContains(t, err, "check config")
	require.ErrorContains(t, err, "HTTP_WRITE_TIMEOUT")
	require.NotContains(t, err.Error(), "migrate")
}

// TestRunMigratesBeforeItServes checks the order of the last two steps of the
// startup. A schema older than the binary is not a failure the service can
// notice later: it surfaces as queries that do not work, one request at a
// time, on a port that is already accepting them.
//
// So the migration has to come first and its failure has to stop the start.
// The port is proof of that: it is free before run is called and still free
// after it returns.
func TestRunMigratesBeforeItServes(t *testing.T) {
	port := freePort(t)

	env := workingEnv()
	env["HTTP_PORT"] = strconv.Itoa(port)

	setEnv(t, env)

	require.False(t, listening(port), "the port was already in use before the test began")

	// Bounded, because the failure this guards against is not a wrong answer
	// but no answer: a run that carried on past a failed migration would reach
	// the group and sit there until a signal that is never sent.
	err := runWithin(t, 10*time.Second)

	require.Error(t, err)
	require.ErrorContains(t, err, "migrate")
	require.False(t, listening(port), "the server accepted connections against a schema it never checked")
}

// runWithin calls run and fails the test if it has not returned within d.
func runWithin(t *testing.T, d time.Duration) error {
	t.Helper()

	done := make(chan error, 1)

	go func() { done <- run() }()

	select {
	case err := <-done:
		return err
	case <-time.After(d):
		t.Fatal("run did not return: the startup carried on past a step that failed")

		return nil
	}
}

// freePort returns a port nothing is listening on, by taking one and giving it
// straight back.
func freePort(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	addr, ok := listener.Addr().(*net.TCPAddr)
	require.True(t, ok)

	require.NoError(t, listener.Close())

	return addr.Port
}

// listening reports whether anything answers on the port.
func listening(port int) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
	if err != nil {
		return false
	}

	_ = conn.Close()

	return true
}
