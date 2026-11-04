// Package handlers holds the code the WORKER runs for each queued job.
// (The API only queues jobs; see app/jobs/dispatcher.go.)
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/hibiken/asynq"

	"github.com/tribe-africa/tribe-backend/app/bootstrap"
	"github.com/tribe-africa/tribe-backend/app/jobs"
)

// Register connects every job type to its handler (like Laravel's job classes).
func Register(application *bootstrap.App) *asynq.ServeMux {
	jobRouter := asynq.NewServeMux()
	jobRouter.HandleFunc(jobs.TypeSendEmailCode, sendEmailCode(application))
	jobRouter.HandleFunc(jobs.TypeSendDeviceCode, sendDeviceCode(application))
	jobRouter.HandleFunc(jobs.TypeProcessPayout, processPayout(application))
	jobRouter.HandleFunc(jobs.TypeProcessPurchase, processPurchase(application))
	jobRouter.HandleFunc(jobs.TypeGenerateStatement, generateStatement(application))
	jobRouter.HandleFunc(jobs.TypeSendPushNotification, sendPushNotification)
	return jobRouter
}

// errNoEmailProvider stops email jobs outside local development: the log
// mailer below is only for a developer's machine, and codes must never be
// written to shared logs.
var errNoEmailProvider = errors.New("no email provider is configured; the log mailer only runs when APP_ENV=local")

// sendEmailCode "sends" a verification code. No email provider is chosen
// yet, so on a developer machine it writes the email to the worker log
// (like Laravel's log mailer). Anywhere else the job fails without retrying.
func sendEmailCode(application *bootstrap.App) asynq.HandlerFunc {
	return func(requestContext context.Context, task *asynq.Task) error {
		var payload jobs.SendEmailCodePayload
		if err := json.Unmarshal(task.Payload(), &payload); err != nil {
			return fmt.Errorf("bad payload: %w: %w", err, asynq.SkipRetry)
		}
		if !application.Settings.IsLocal() {
			return fmt.Errorf("%w: %w", errNoEmailProvider, asynq.SkipRetry)
		}
		slog.Info("EMAIL (log mailer)", "to", payload.Email, "purpose", payload.Purpose, "code", payload.Code)
		return nil
	}
}

// sendDeviceCode emails the "new phone" code (same rules as sendEmailCode).
func sendDeviceCode(application *bootstrap.App) asynq.HandlerFunc {
	return func(requestContext context.Context, task *asynq.Task) error {
		var payload jobs.SendDeviceCodePayload
		if err := json.Unmarshal(task.Payload(), &payload); err != nil {
			return fmt.Errorf("bad payload: %w: %w", err, asynq.SkipRetry)
		}
		if !application.Settings.IsLocal() {
			return fmt.Errorf("%w: %w", errNoEmailProvider, asynq.SkipRetry)
		}
		slog.Info("EMAIL (log mailer): new phone sign-in", "to", payload.Email, "device", payload.DeviceName, "code", payload.Code)
		return nil
	}
}

// processPayout asks the provider to send money out. If it returns an
// error, asynq retries later with a growing delay.
func processPayout(application *bootstrap.App) asynq.HandlerFunc {
	return func(requestContext context.Context, task *asynq.Task) error {
		var payload jobs.ProcessPayoutPayload
		if err := json.Unmarshal(task.Payload(), &payload); err != nil {
			return fmt.Errorf("bad payload: %w: %w", err, asynq.SkipRetry)
		}
		return application.Transfers.ProcessPayout(requestContext, payload.TransactionID)
	}
}

// processPurchase asks the bill or gift card provider to finish a pending
// purchase (see products.ProcessPurchase). An error means "we do not know
// yet", and asynq retries later with a growing delay.
func processPurchase(application *bootstrap.App) asynq.HandlerFunc {
	return func(requestContext context.Context, task *asynq.Task) error {
		var payload jobs.ProcessPurchasePayload
		if err := json.Unmarshal(task.Payload(), &payload); err != nil {
			return fmt.Errorf("bad payload: %w: %w", err, asynq.SkipRetry)
		}
		return application.Products.ProcessPurchase(requestContext, payload.TransactionID)
	}
}

// generateStatement builds a statement. Here it counts the transactions and
// logs a summary; a real version would render a PDF and email it.
func generateStatement(application *bootstrap.App) asynq.HandlerFunc {
	return func(requestContext context.Context, task *asynq.Task) error {
		var payload jobs.GenerateStatementPayload
		if err := json.Unmarshal(task.Payload(), &payload); err != nil {
			return fmt.Errorf("bad payload: %w: %w", err, asynq.SkipRetry)
		}
		var transactionCount int64
		err := application.Database.WithContext(requestContext).Table("transactions").
			Where("user_id = ? AND created_at >= ? AND created_at < DATE_ADD(?, INTERVAL 1 DAY)", payload.UserID, payload.FromDate, payload.ToDate).
			Count(&transactionCount).Error
		if err != nil {
			return err
		}
		slog.Info("statement generated", "user", payload.UserID, "from", payload.FromDate, "to", payload.ToDate, "transactions", transactionCount)
		return nil
	}
}

// sendPushNotification would call the Expo push API. It logs for now.
func sendPushNotification(requestContext context.Context, task *asynq.Task) error {
	var payload jobs.SendPushNotificationPayload
	if err := json.Unmarshal(task.Payload(), &payload); err != nil {
		return fmt.Errorf("bad payload: %w: %w", err, asynq.SkipRetry)
	}
	slog.Info("PUSH (log only)", "user", payload.UserID, "title", payload.Title, "body", payload.Body)
	return nil
}
