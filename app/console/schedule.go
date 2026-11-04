// Package console defines the scheduled (cron) jobs, like Laravel's
// app/Console/Kernel.php schedule() method.
//
// Cron expressions here have SIX fields (seconds first):
//
//	"0 5 0 * * *"  = at 00:05:00 every day
//	"0 0 * * * *"  = at minute 0 of every hour
//
// Every run is recorded in scheduled_job_runs with a UNIQUE(job, window)
// key, so even if two scheduler processes run, each job runs ONCE per window.
package console

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/tribe-africa/tribe-backend/app/bootstrap"
	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/app/services/ledger"
	"github.com/tribe-africa/tribe-backend/database"
)

// ScheduledJob is one cron job.
type ScheduledJob struct {
	Name     string
	Schedule string
	// WindowKey names the period a run belongs to (e.g. the date for a daily job).
	WindowKey func(now time.Time) string
	Run       func(requestContext context.Context, application *bootstrap.App) (string, error)
}

// Jobs is the list of every cron job.
func Jobs(application *bootstrap.App) []ScheduledJob {
	lagos := application.Settings.App.TimeZone
	return []ScheduledJob{
		{
			// Close the books for yesterday: one snapshot row per ledger
			// account. Any earlier day that was missed or failed is written
			// first (see CatchUpDailySnapshots).
			Name: "ledger:daily-snapshot", Schedule: "0 5 0 * * *",
			WindowKey: func(now time.Time) string { return now.In(lagos).AddDate(0, 0, -1).Format(time.DateOnly) },
			Run:       RunDailySnapshot,
		},
		{
			// Recovery: every hour, write any day of the last week whose
			// snapshots are missing (scheduler was down) or incomplete (the
			// nightly run failed part-way). Usually finds nothing to do.
			Name: "ledger:snapshot-catch-up", Schedule: "0 20 * * * *",
			WindowKey: func(now time.Time) string { return now.In(lagos).Format("2006-01-02T15") },
			Run:       RunDailySnapshot,
		},
		{
			// Remove idempotency keys older than 7 days and expired quotes.
			Name: "cleanup:hourly", Schedule: "0 0 * * * *",
			WindowKey: func(now time.Time) string { return now.In(lagos).Format("2006-01-02T15") },
			Run: func(requestContext context.Context, application *bootstrap.App) (string, error) {
				removedKeys, err := application.Idempotency.DeleteOlderThan(requestContext, 7*24*time.Hour)
				if err != nil {
					return "", err
				}
				result := application.Database.WithContext(requestContext).Where("expires_at < ? AND used_at IS NULL", time.Now().UTC().Add(-time.Hour)).Delete(&models.Quote{})
				return fmt.Sprintf("removed %d idempotency keys and %d expired quotes", removedKeys, result.RowsAffected), result.Error
			},
		},
		{
			// Trim old operational rows (API trail, used codes, ended
			// sessions, old webhooks). Never touches the books or audit logs.
			Name: "cleanup:daily", Schedule: "0 15 3 * * *",
			WindowKey: func(now time.Time) string { return now.In(lagos).Format(time.DateOnly) },
			Run:       RunDailyCleanup,
		},
		{
			// Re-queue bills, gift cards and payouts stuck in "pending"
			// (their queue job was lost or ran out of retries). Safe to
			// overlap with the jobs themselves; see products.ProcessPurchase.
			Name: "purchases:redrive", Schedule: "0 */5 * * * *",
			WindowKey: func(now time.Time) string { return now.UTC().Truncate(5 * time.Minute).Format("2006-01-02T15:04") },
			Run: func(requestContext context.Context, application *bootstrap.App) (string, error) {
				attempt := time.Now().UTC().Truncate(5 * time.Minute).Format("200601021504")
				return application.Products.RedrivePendingPurchases(requestContext, attempt)
			},
		},
		{
			// Pay recurring bills that are due.
			Name: "bills:recurring", Schedule: "0 0 7 * * *",
			WindowKey: func(now time.Time) string { return now.In(lagos).Format(time.DateOnly) },
			Run: func(requestContext context.Context, application *bootstrap.App) (string, error) {
				paid, skipped, err := application.Products.RunDueRecurringBills(requestContext)
				return fmt.Sprintf("paid %d, skipped %d", paid, skipped), err
			},
		},
		{
			// Check the books every night after the snapshot. Incremental:
			// only what was added since the last clean check.
			Name: "ledger:verify", Schedule: "0 30 0 * * *",
			WindowKey: func(now time.Time) string { return now.In(lagos).Format(time.DateOnly) },
			Run: func(requestContext context.Context, application *bootstrap.App) (string, error) {
				return runVerify(requestContext, application, ledger.VerifyIncremental)
			},
		},
		{
			// Every Sunday, re-prove the WHOLE ledger from the first leg.
			Name: "ledger:verify-full", Schedule: "0 30 2 * * 0",
			WindowKey: func(now time.Time) string { return now.In(lagos).Format(time.DateOnly) },
			Run: func(requestContext context.Context, application *bootstrap.App) (string, error) {
				return runVerify(requestContext, application, ledger.VerifyFull)
			},
		},
	}
}

// runVerify runs a ledger check and turns problems into a failed job.
func runVerify(requestContext context.Context, application *bootstrap.App, mode string) (string, error) {
	report, err := application.Ledger.RunVerify(requestContext, mode)
	if err != nil {
		return "", err
	}
	if report.ProblemCount > 0 {
		return "", fmt.Errorf("%d ledger problems, first: %s", report.ProblemCount, report.Problems[0].Description)
	}
	return fmt.Sprintf("ledger OK (%s: %d entries, %d snapshots checked)", report.Mode, report.EntriesChecked, report.SnapshotsChecked), nil
}

// RunDailySnapshot writes every missing snapshot day up to yesterday (see
// CatchUpDailySnapshots). Used by the nightly job, the hourly catch-up and
// the admin "run now" endpoint.
func RunDailySnapshot(requestContext context.Context, application *bootstrap.App) (string, error) {
	return CatchUpDailySnapshots(requestContext, application)
}

// snapshotDayJob is the scheduled_job_runs name that marks one snapshot DAY
// as fully written (window = the date). It is how the catch-up knows which
// days are done, and it shows on the admin Jobs page.
const snapshotDayJob = "ledger:snapshot-day"

// catchUpDays is how far back the catch-up looks for missing days.
const catchUpDays = 7

// CatchUpDailySnapshots writes the snapshots of every day in the last week
// (up to yesterday) that is not marked complete yet, OLDEST FIRST, so each
// day builds on the one before it.
//
// Before, a failed nightly run stayed "failed" and was never retried, and a
// night the scheduler was down was never written at all. Balances stayed
// correct either way (a balance is "latest snapshot + every leg after it"),
// but they got slower to compute and the snapshot report had gaps.
//
// Writing a day twice is safe: WriteDailySnapshots skips accounts that
// already have that day, so a day that failed half-way is simply finished.
// Days before the very first snapshot are not invented.
func CatchUpDailySnapshots(requestContext context.Context, application *bootstrap.App) (string, error) {
	lagos := application.Settings.App.TimeZone
	today := time.Now().In(lagos)
	yesterday := today.AddDate(0, 0, -1).Format(time.DateOnly)

	var firstSnapshot sql.NullTime
	if err := application.Database.WithContext(requestContext).
		Raw("SELECT MIN(snapshot_date) FROM ledger_daily_snapshots").Scan(&firstSnapshot).Error; err != nil {
		return "", err
	}
	var completeDays []string
	if err := application.Database.WithContext(requestContext).Model(&models.ScheduledJobRun{}).
		Where("job_name = ? AND status = ?", snapshotDayJob, "succeeded").
		Where("window_key >= ?", today.AddDate(0, 0, -catchUpDays).Format(time.DateOnly)).
		Pluck("window_key", &completeDays).Error; err != nil {
		return "", err
	}
	complete := map[string]bool{}
	for _, day := range completeDays {
		complete[day] = true
	}

	var summary []string
	for daysAgo := catchUpDays; daysAgo >= 1; daysAgo-- {
		day := today.AddDate(0, 0, -daysAgo).Format(time.DateOnly)
		if complete[day] {
			continue
		}
		// With no snapshots at all yet, start with yesterday only.
		if !firstSnapshot.Valid && day != yesterday {
			continue
		}
		if firstSnapshot.Valid && day < firstSnapshot.Time.Format(time.DateOnly) {
			continue
		}
		written, err := application.Ledger.WriteDailySnapshots(requestContext, day)
		if err != nil {
			return strings.Join(summary, "; "), fmt.Errorf("snapshots for %s: %w", day, err)
		}
		if err := markSnapshotDayComplete(requestContext, application, day, written); err != nil {
			return strings.Join(summary, "; "), err
		}
		summary = append(summary, fmt.Sprintf("wrote %d snapshots for %s", written, day))
	}
	if len(summary) == 0 {
		return "every snapshot day up to " + yesterday + " is complete", nil
	}
	return strings.Join(summary, "; "), nil
}

// markSnapshotDayComplete records that every account has a snapshot for day.
// The UNIQUE (job, window) key means a row may already exist (for example a
// "failed" one from an older version); then it is updated instead.
func markSnapshotDayComplete(requestContext context.Context, application *bootstrap.App, day string, written int) error {
	now := time.Now().UTC()
	message := fmt.Sprintf("wrote %d snapshots", written)
	row := models.ScheduledJobRun{JobName: snapshotDayJob, WindowKey: day, Status: "succeeded", Message: message, StartedAt: now, FinishedAt: &now}
	err := application.Database.WithContext(requestContext).Create(&row).Error
	if database.IsDuplicateEntry(err) {
		return application.Database.WithContext(requestContext).Model(&models.ScheduledJobRun{}).
			Where("job_name = ? AND window_key = ?", snapshotDayJob, day).
			Updates(map[string]any{"status": "succeeded", "message": message, "finished_at": now}).Error
	}
	return err
}

// RunOnce runs a job for a window, recording it in scheduled_job_runs.
// If the (job, window) row already exists, the job already ran: skip it.
func RunOnce(requestContext context.Context, application *bootstrap.App, job ScheduledJob, windowKey string) {
	run := models.ScheduledJobRun{JobName: job.Name, WindowKey: windowKey, Status: "running", StartedAt: time.Now().UTC()}
	if err := application.Database.WithContext(requestContext).Create(&run).Error; err != nil {
		if database.IsDuplicateEntry(err) {
			slog.Info("cron job already ran for this window", "job", job.Name, "window", windowKey)
			return
		}
		slog.Error("cron job could not start", "job", job.Name, "error", err)
		return
	}
	message, err := job.Run(requestContext, application)
	status := "succeeded"
	if err != nil {
		status, message = "failed", err.Error()
		slog.Error("cron job failed", "job", job.Name, "error", err)
	} else {
		slog.Info("cron job finished", "job", job.Name, "result", message)
	}
	finished := time.Now().UTC()
	if len(message) > 500 {
		message = message[:500]
	}
	application.Database.WithContext(requestContext).Model(&models.ScheduledJobRun{}).Where("id = ?", run.ID).
		Updates(map[string]any{"status": status, "message": message, "finished_at": finished})
}

// Start registers every job with the cron runner and starts it.
func Start(requestContext context.Context, application *bootstrap.App) *cron.Cron {
	runner := cron.New(cron.WithSeconds(), cron.WithLocation(application.Settings.App.TimeZone))
	for _, job := range Jobs(application) {
		job := job // capture this loop's job for the function below
		_, err := runner.AddFunc(job.Schedule, func() {
			RunOnce(requestContext, application, job, job.WindowKey(time.Now()))
		})
		if err != nil {
			slog.Error("bad cron schedule", "job", job.Name, "error", err)
		}
	}
	runner.Start()
	return runner
}
