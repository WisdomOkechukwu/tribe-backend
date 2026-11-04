// Command worker runs queued jobs (like `php artisan queue:work`).
//
//	go run ./cmd/worker
package main

import (
	"log/slog"
	"os"

	"github.com/hibiken/asynq"

	"github.com/tribe-africa/tribe-backend/app/bootstrap"
	"github.com/tribe-africa/tribe-backend/app/jobs"
	"github.com/tribe-africa/tribe-backend/app/jobs/handlers"
	"github.com/tribe-africa/tribe-backend/config"
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

	server := asynq.NewServer(
		asynq.RedisClientOpt{Addr: settings.Redis.Host + ":" + settings.Redis.Port, Password: settings.Redis.Password},
		asynq.Config{
			Concurrency: 10, // up to 10 jobs at the same time
			// Out of every 10 picks: 6 from critical, 3 from default, 1 from low.
			Queues: map[string]int{jobs.QueueCritical: 6, jobs.QueueDefault: 3, jobs.QueueLow: 1},
		},
	)
	slog.Info("worker started: waiting for jobs")
	// Run blocks until Ctrl+C, then lets running jobs finish.
	if err := server.Run(handlers.Register(application)); err != nil {
		slog.Error("worker stopped", "error", err)
		os.Exit(1)
	}
}
