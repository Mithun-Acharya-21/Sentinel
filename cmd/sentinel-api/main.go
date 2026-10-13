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

	"sentinel/internal/config"
	"sentinel/internal/httpapi"
	"sentinel/internal/storage/postgres"

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
	root := http.NewServeMux()
	root.Handle("GET /health/live", httpapi.LivenessHandler())
	root.Handle("GET /health/ready", httpapi.ReadinessHandler(database))
	root.Handle("/", httpapi.New(repository))

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

	serveResult := make(chan error, 1)
	go func() {
		serveResult <- server.ListenAndServe()
	}()

	select {
	case err := <-serveResult:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-signalContext.Done():
		shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancelShutdown()
		if err := server.Shutdown(shutdownContext); err != nil {
			_ = server.Close()
			return errors.New("HTTP server did not shut down cleanly before the deadline")
		}
		<-serveResult
		return nil
	}
}
