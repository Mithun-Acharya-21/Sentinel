package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"sentinel/internal/auth"
	"sentinel/internal/config"
	"sentinel/internal/httpapi"
	"sentinel/internal/storage/postgres"
	"sentinel/internal/worker"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const (
	databaseStartupTimeout = 5 * time.Second
	shutdownTimeout        = 10 * time.Second
)

func main() {
	if err := run(); err != nil {
		log.Printf("sentinel stopped: %v", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	database, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		return errors.New("could not configure PostgreSQL driver")
	}
	defer database.Close()
	database.SetMaxOpenConns(10)
	database.SetMaxIdleConns(5)
	database.SetConnMaxLifetime(30 * time.Minute)
	database.SetConnMaxIdleTime(5 * time.Minute)

	startupContext, cancelStartup := context.WithTimeout(context.Background(), databaseStartupTimeout)
	err = database.PingContext(startupContext)
	cancelStartup()
	if err != nil {
		return errors.New("PostgreSQL startup check failed; verify SENTINEL_DATABASE_URL and database availability")
	}

	repository := postgres.NewRepository(database)
	credRepository := postgres.NewCredentialRepository(database)
	verifier := auth.NewStoreVerifier(credRepository)

	preAuthIPLimiter, err := httpapi.NewLimiter(httpapi.RateLimitConfig{
		Enabled:         cfg.PreAuthRateLimit.Enabled,
		RequestsPerSec:  cfg.PreAuthRateLimit.RequestsPerSec,
		BurstCapacity:   cfg.PreAuthRateLimit.BurstCapacity,
		BucketTTL:       httpapi.DefaultBucketTTL,
		CleanupInterval: httpapi.DefaultCleanupInterval,
		MaxBuckets:      httpapi.DefaultMaxBuckets,
		TrustedProxies:  cfg.PreAuthRateLimit.TrustedProxies,
	})
	if err != nil {
		return errors.New("could not configure pre-auth IP rate limiter")
	}
	defer preAuthIPLimiter.Stop()

	principalLimiter, err := httpapi.NewLimiter(httpapi.RateLimitConfig{
		Enabled:         cfg.RateLimit.Enabled,
		RequestsPerSec:  cfg.RateLimit.RequestsPerSec,
		BurstCapacity:   cfg.RateLimit.BurstCapacity,
		BucketTTL:       httpapi.DefaultBucketTTL,
		CleanupInterval: httpapi.DefaultCleanupInterval,
		MaxBuckets:      httpapi.DefaultMaxBuckets,
		TrustedProxies:  cfg.RateLimit.TrustedProxies,
	})
	if err != nil {
		return errors.New("could not configure principal rate limiter")
	}
	defer principalLimiter.Stop()

	root := http.NewServeMux()
	root.Handle("GET /health/live", httpapi.LivenessHandler())
	root.Handle("GET /health/ready", httpapi.ReadinessHandler(database))
	root.Handle("/", httpapi.NewProtectedHandlerWithLimiters(repository, verifier, preAuthIPLimiter, principalLimiter))

	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           root,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	signalContext, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	jobRepository := postgres.NewJobRepository(database)
	var workerEngine *worker.Engine
	if cfg.Worker.Count > 0 {
		workerEngine = worker.NewEngine(
			jobRepository,
			worker.NewDefaultIncidentProcessor(jobRepository),
			worker.Config{
				WorkerCount:   cfg.Worker.Count,
				PollInterval:  cfg.Worker.PollInterval,
				LeaseDuration: cfg.Worker.LeaseDuration,
				ShutdownGrace: shutdownTimeout,
			},
		)
		workerEngine.Start(signalContext)
	}

	serveResult := make(chan error, 1)
	go func() {
		serveResult <- server.ListenAndServe()
	}()

	select {
	case err := <-serveResult:
		if workerEngine != nil {
			stopCtx, cancelStop := context.WithTimeout(context.Background(), shutdownTimeout)
			_ = workerEngine.Stop(stopCtx)
			cancelStop()
		}
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-signalContext.Done():
		shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancelShutdown()
		if err := server.Shutdown(shutdownContext); err != nil {
			_ = server.Close()
			if workerEngine != nil {
				_ = workerEngine.Stop(shutdownContext)
			}
			return errors.New("HTTP server did not shut down cleanly before the deadline")
		}
		<-serveResult
		if workerEngine != nil {
			if err := workerEngine.Stop(shutdownContext); err != nil {
				return errors.New("background workers did not stop cleanly before the deadline")
			}
		}
		return nil
	}
}
