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

	// The budget of one provider call, which only the provider package can work
	// out: the jitter window is part of its retry schedule. Checked here, where
	// both packages are already in hand, so that the configuration stays a leaf
	// of the dependency graph.
	providerBudget := provider.Budget(cfg.ProviderAttempts, cfg.ProviderTimeout, cfg.ProviderBackoff)
	if err := cfg.CheckTimeouts(providerBudget); err != nil {
		return fmt.Errorf("check config: %w", err)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(logger)

	// Cancelled on SIGINT/SIGTERM. The errgroup derives its own context from
	// this one, which is also cancelled by the first error of any goroutine in
	// the group: the server, the workers and the recovery pass.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Before the server accepts anything: a schema older than the binary would
	// only surface later, as failing queries.
	if err := postgres.Migrate(ctx, cfg.DatabaseURL, logger); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	// Closed by the deferred call rather than by whoever uses it, and that
	// happens after the group below has been waited on -- a pool closed while
	// a worker still holds a connection would fail the very finalisation the
	// shutdown is waiting for.
	repo, err := postgres.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer repo.Close()

	// The retrier is what the worker holds: how many times an upstream is asked
	// is a property of the call, not a decision the worker makes each time.
	rates := provider.NewRetrier(
		provider.NewClient(cfg.ProviderBaseURL, cfg.ProviderTimeout),
		cfg.ProviderAttempts,
		cfg.ProviderBackoff,
	)

	// The signal that a task has just been posted: written by the handler path,
	// read by whichever worker is idle. Buffered by one and written without
	// blocking, so it says "the queue is worth a look" rather than counting
	// anything -- see Service.notify.
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

	cleanup := worker.NewCleanup(repo, worker.CleanupSettings{
		Interval: cfg.IdempotencyCleanupInterval,
		TTL:      cfg.IdempotencyTTL,
	}, logger)

	srv := &http.Server{
		Addr:         net.JoinHostPort("", strconv.Itoa(cfg.HTTPPort)),
		Handler:      api.NewRouter(svc, logger, cfg.HandlerTimeout()),
		ReadTimeout:  cfg.HTTPReadTimeout,
		WriteTimeout: cfg.HTTPWriteTimeout,
		// Everything net/http reports on its own -- a malformed request line, a
		// panic our middleware never saw -- goes through the same handler as
		// the rest of the service. Left unset it writes plain text to stderr,
		// which is the one thing that breaks a stream of JSON logs.
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
	// the arbitration is the queue's own -- claiming is a single statement with
	// SKIP LOCKED, so two goroutines asking at once get two different tasks
	// rather than one of them waiting.
	logger.Info("worker pool started", slog.Int("size", cfg.WorkerConcurrency))

	for range cfg.WorkerConcurrency {
		g.Go(func() error { return quotes.Run(ctx) })
	}

	g.Go(func() error { return recovery.Run(ctx) })

	g.Go(func() error { return cleanup.Run(ctx) })

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
