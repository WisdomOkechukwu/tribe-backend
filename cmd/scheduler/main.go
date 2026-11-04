// Command scheduler runs the cron jobs AND the live rate collector
// (like `php artisan schedule:work`).
//
//	go run ./cmd/scheduler
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tribe-africa/tribe-backend/app/bootstrap"
	"github.com/tribe-africa/tribe-backend/app/cache"
	"github.com/tribe-africa/tribe-backend/app/console"
	"github.com/tribe-africa/tribe-backend/config"
	"github.com/tribe-africa/tribe-backend/pkg/ids"
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

	stopContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Cron jobs (daily snapshot, cleanup, recurring bills, ledger check).
	runner := console.Start(stopContext, application)
	defer runner.Stop()

	// The rate collector must run in only ONE scheduler, even if you start
	// two. We hold a Redis lock and keep extending it while we collect.
	go runRateCollectorWhileLeader(stopContext, application)

	// Push notifications for new inbox messages (see account.PushPendingInboxMessages).
	go runPushOutbox(stopContext, application)

	slog.Info("scheduler started", "rate_refresh", settings.Rates.RefreshEvery.String())
	<-stopContext.Done()
	slog.Info("scheduler stopping")
}

// runRateCollectorWhileLeader keeps trying to become the collector "leader".
func runRateCollectorWhileLeader(stopContext context.Context, application *bootstrap.App) {
	owner := ids.New()
	for stopContext.Err() == nil {
		lock, err := cache.TryLock(stopContext, application.Redis, "lock:rate-collector", owner, 30*time.Second)
		if err != nil || lock == nil {
			// Someone else is collecting. Check again soon.
			sleep(stopContext, 10*time.Second)
			continue
		}
		slog.Info("this scheduler is now collecting rates")
		collectorContext, stopCollector := context.WithCancel(stopContext)
		go application.Rates.RunCollector(collectorContext)

		// Keep the lock alive; if we ever lose it, stop collecting.
		for collectorContext.Err() == nil {
			sleep(collectorContext, 10*time.Second)
			if !lock.Extend(stopContext, 30*time.Second) {
				slog.Warn("lost the rate collector lock")
				break
			}
		}
		stopCollector()
		lock.Release(context.Background())
	}
}

// runPushOutbox queues pushes for new inbox messages every few seconds.
// Running it in two schedulers is safe: each push job is unique per message.
func runPushOutbox(stopContext context.Context, application *bootstrap.App) {
	for stopContext.Err() == nil {
		if queued, err := application.Account.PushPendingInboxMessages(stopContext); err != nil {
			slog.Warn("push outbox run failed", "error", err)
		} else if queued > 0 {
			slog.Info("queued push notifications", "count", queued)
		}
		sleep(stopContext, 3*time.Second)
	}
}

// sleep waits, but wakes up early if the program is stopping.
func sleep(stopContext context.Context, duration time.Duration) {
	select {
	case <-stopContext.Done():
	case <-time.After(duration):
	}
}
