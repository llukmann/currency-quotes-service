// Command quotes is the entry point of the currency quotes service.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"golang.org/x/sync/errgroup"

	"github.com/llukmann/currency-quotes-service/internal/api"
	"github.com/llukmann/currency-quotes-service/internal/config"
	"github.com/llukmann/currency-quotes-service/internal/provider"
	"github.com/llukmann/currency-quotes-service/internal/service"
	"github.com/llukmann/currency-quotes-service/internal/storage/postgres"
	"github.com/llukmann/currency-quotes-service/internal/worker"
)

func main() {
	if err := run(); err != nil {
		slog.Error("service stopped with error", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(logger)

	// The errgroup derives its own context from this one, so the first error of
	// any goroutine in the group cancels the rest.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Closed by the deferred call, which runs after the group below has been
	// waited on: a pool closed while a worker still holds a connection would fail
	// the very finalisation the shutdown is waiting for.
	repo, err := postgres.New(ctx, cfg.DatabaseURL, cfg.IdempotencyTTL)
	if err != nil {
		return err
	}
	defer repo.Close()

	rates := provider.NewRetrier(
		provider.NewClient(cfg.ProviderBaseURL, cfg.ProviderTimeout),
		cfg.ProviderAttempts,
		cfg.ProviderBackoff,
	)

	// Buffered by one and written without blocking: it says "the queue is worth a
	// look" rather than counting anything.
	wake := make(chan struct{}, 1)

	svc := service.New(repo, wake)

	quotes := worker.New(repo, rates, wake, worker.Settings{
		TaskTimeout:      cfg.WorkerTaskTimeout,
		PollInterval:     cfg.WorkerPollInterval,
		ProviderAttempts: cfg.ProviderAttempts,
	}, logger)

	recovery := worker.NewRecovery(repo, worker.RecoverySettings{
		Interval:     cfg.WorkerRecoveryInterval,
		StuckTimeout: cfg.WorkerStuckTimeout,
		MaxAttempts:  cfg.WorkerMaxAttempts,
	}, logger)

	srv := &http.Server{
		Addr:         net.JoinHostPort("", strconv.Itoa(cfg.HTTPPort)),
		Handler:      api.NewRouter(svc, logger, cfg.HandlerTimeout()),
		ReadTimeout:  cfg.HTTPReadTimeout,
		WriteTimeout: cfg.HTTPWriteTimeout,
		// Left unset, net/http writes plain text to stderr for a malformed request
		// line or a panic our middleware never saw, which is the one thing that
		// breaks a stream of JSON logs.
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}

	g, ctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		logger.Info("http server started", slog.String("addr", srv.Addr))

		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}

		return nil
	})

	// One instance behind all of them: a worker holds nothing that changes, and
	// the arbitration is the queue.s own -- claiming with SKIP LOCKED gives two
	// goroutines asking at once two different tasks.
	logger.Info("worker pool started", slog.Int("size", cfg.WorkerConcurrency))

	for range cfg.WorkerConcurrency {
		g.Go(func() error { return quotes.Run(ctx) })
	}

	g.Go(func() error { return recovery.Run(ctx) })

	g.Go(func() error {
		<-ctx.Done()
		logger.Info("shutdown started", slog.String("timeout", cfg.ShutdownTimeout.String()))

		// The shutdown context must not inherit the cancellation that already
		// fired, otherwise Shutdown would abort in-flight requests instead of
		// letting them finish.
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownTimeout)
		defer cancel()

		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("http shutdown: %w", err)
		}

		return nil
	})

	if err := g.Wait(); err != nil {
		return err
	}

	logger.Info("service stopped")

	return nil
}
