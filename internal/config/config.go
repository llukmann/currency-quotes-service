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
)

// Valid TCP port range. Port 0 is excluded: it would make the server pick an
// arbitrary free port, which cannot match the published compose port.
const (
	minPort = 1
	maxPort = 65535
)

// Config holds the service parameters. Variable names are listed in
// .env.example, which is the source of truth for them.
type Config struct {
	HTTPPort         int
	HTTPReadTimeout  time.Duration
	HTTPWriteTimeout time.Duration
	ShutdownTimeout  time.Duration
	LogLevel         slog.Level
}

// Load reads the configuration from the environment. Unset variables fall
// back to defaults; a malformed value is an error naming the variable.
func Load() (Config, error) {
	var cfg Config
	var err error

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

	return cfg, nil
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
