// Command server starts the HTTP API (like `php artisan serve`).
//
//	go run ./cmd/server
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tribe-africa/tribe-backend/app/bootstrap"
	"github.com/tribe-africa/tribe-backend/config"
	"github.com/tribe-africa/tribe-backend/routes"
)

func main() {
	settings, err := config.Load()
	if err != nil {
		slog.Error("bad configuration", "error", err)
		os.Exit(1)
	}
	application, err := bootstrap.New(settings)
	if err != nil {
		slog.Error("could not start", "error", err)
		os.Exit(1)
	}
	defer application.Close()

	// The context is cancelled when you press Ctrl+C (or the server is told to stop).
	stopContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// One goroutine listens to Redis for rate updates and pushes them to phones.
	go application.RateHub.Run(stopContext)
	go application.SupportHub.Run(stopContext)

	server := &http.Server{
		Addr:              ":" + settings.App.Port,
		Handler:           routes.Build(application),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: the live-rates stream stays open for as long as the screen is open.
	}

	go func() {
		slog.Info("API listening", "address", "http://localhost:"+settings.App.Port, "environment", settings.App.Environment)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server stopped", "error", err)
			stop()
		}
	}()

	<-stopContext.Done()
	slog.Info("shutting down: finishing requests that are already running")
	shutdownContext, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	server.Shutdown(shutdownContext)
}
