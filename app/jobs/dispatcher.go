// Package jobs defines background jobs, like Laravel's queued jobs.
//
// Some work should not make the user wait: sending an email, talking to a
// slow provider, building a PDF statement. The API puts a small "job" message
// into Redis and answers the user straight away. A separate program, the
// worker (cmd/worker), picks the job up and does the work.
//
//	API:     jobs.Dispatcher.SendEmailCode(...)  -> job saved in Redis
//	Worker:  handlers.HandleSendEmailCode(...)   -> email actually sent
//
// We use the asynq library, which stores jobs in Redis and retries failed
// jobs automatically with a back-off.
//
// RULE: every job must be safe to run twice (Redis may deliver a job again
// after a crash). Handlers always check the current state first.
package jobs

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/hibiken/asynq"
)

// Job names (the "type" of each task in Redis).
const (
	TypeSendEmailCode        = "email:send_code"
	TypeSendDeviceCode       = "email:send_device_code"
	TypeProcessPayout        = "payouts:process"
	TypeProcessPurchase      = "purchases:process" // a bill payment or gift card purchase waiting for its provider
	TypeGenerateStatement    = "statements:generate"
	TypeSendPushNotification = "notifications:push"
)

// Queues, from most to least urgent. The worker gives more time to "critical".
const (
	QueueCritical = "critical"
	QueueDefault  = "default"
	QueueLow      = "low"
)

// SendEmailCodePayload is the data for TypeSendEmailCode.
type SendEmailCodePayload struct {
	Email   string `json:"email"`
	Code    string `json:"code"`
	Purpose string `json:"purpose"` // signup | password_reset | phone
}

// SendDeviceCodePayload is the data for TypeSendDeviceCode.
type SendDeviceCodePayload struct {
	UserID     string `json:"user_id"`
	Email      string `json:"email"`
	Code       string `json:"code"`
	DeviceName string `json:"device_name"`
}

// ProcessPayoutPayload is the data for TypeProcessPayout.
type ProcessPayoutPayload struct {
	TransactionID string `json:"transaction_id"`
}

// ProcessPurchasePayload is the data for TypeProcessPurchase.
type ProcessPurchasePayload struct {
	TransactionID string `json:"transaction_id"`
}

// GenerateStatementPayload is the data for TypeGenerateStatement.
type GenerateStatementPayload struct {
	UserID    string `json:"user_id"`
	AssetCode string `json:"asset_code"`
	FromDate  string `json:"from_date"`
	ToDate    string `json:"to_date"`
}

// SendPushNotificationPayload is the data for TypeSendPushNotification.
type SendPushNotificationPayload struct {
	UserID string `json:"user_id"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	Href   string `json:"href,omitempty"`
}

// Dispatcher puts jobs on the queue.
type Dispatcher struct {
	client *asynq.Client
}

// NewDispatcher connects the dispatcher to Redis.
func NewDispatcher(redisAddress string, redisPassword string) *Dispatcher {
	return &Dispatcher{client: asynq.NewClient(asynq.RedisClientOpt{Addr: redisAddress, Password: redisPassword})}
}

// Close disconnects from Redis.
func (dispatcher *Dispatcher) Close() error {
	return dispatcher.client.Close()
}

// SendEmailCode queues an email with a verification code.
func (dispatcher *Dispatcher) SendEmailCode(requestContext context.Context, payload SendEmailCodePayload) {
	dispatcher.enqueue(requestContext, TypeSendEmailCode, payload, asynq.Queue(QueueCritical), asynq.MaxRetry(5))
}

// SendDeviceCode queues the "is this your new phone?" email.
func (dispatcher *Dispatcher) SendDeviceCode(requestContext context.Context, payload SendDeviceCodePayload) {
	dispatcher.enqueue(requestContext, TypeSendDeviceCode, payload, asynq.Queue(QueueCritical), asynq.MaxRetry(5))
}

// ProcessPayout queues the job that asks the provider to send money out.
// TaskID makes the job unique: queueing the same transaction twice is ignored.
func (dispatcher *Dispatcher) ProcessPayout(requestContext context.Context, transactionID string) {
	dispatcher.enqueue(requestContext, TypeProcessPayout, ProcessPayoutPayload{TransactionID: transactionID},
		asynq.Queue(QueueCritical), asynq.MaxRetry(10), asynq.TaskID("payout:"+transactionID))
}

// ProcessPurchase queues the job that asks the bill or gift card provider to
// complete a pending purchase. It waits `delay` first: the API usually
// finishes the purchase itself straight away, and the job (finding nothing
// pending) then does nothing. It is the safety net if the API could not.
//
// attempt makes the TaskID unique per re-drive: asynq keeps a task's id
// while the task is archived, so the sweeper needs a new id to queue it again.
func (dispatcher *Dispatcher) ProcessPurchase(requestContext context.Context, transactionID string, attempt string, delay time.Duration) error {
	return dispatcher.enqueueOrFail(requestContext, TypeProcessPurchase, ProcessPurchasePayload{TransactionID: transactionID},
		asynq.Queue(QueueCritical), asynq.MaxRetry(10), asynq.ProcessIn(delay), asynq.TaskID("purchase:"+transactionID+":"+attempt))
}

// RedriveProcessPayout queues a payout again under a new TaskID (see
// ProcessPurchase). Used by the sweeper for payouts stuck in pending.
func (dispatcher *Dispatcher) RedriveProcessPayout(requestContext context.Context, transactionID string, attempt string) error {
	return dispatcher.enqueueOrFail(requestContext, TypeProcessPayout, ProcessPayoutPayload{TransactionID: transactionID},
		asynq.Queue(QueueCritical), asynq.MaxRetry(10), asynq.TaskID("payout:"+transactionID+":"+attempt))
}

// GenerateStatement queues building an account statement.
func (dispatcher *Dispatcher) GenerateStatement(requestContext context.Context, payload GenerateStatementPayload) {
	dispatcher.enqueue(requestContext, TypeGenerateStatement, payload, asynq.Queue(QueueLow), asynq.MaxRetry(3))
}

// SendPushNotification queues a push notification to the user's phone.
// inboxMessageID makes the job unique, so queueing the same message twice
// (for example after the scheduler restarts) does not push it twice.
func (dispatcher *Dispatcher) SendPushNotification(requestContext context.Context, inboxMessageID string, payload SendPushNotificationPayload) error {
	return dispatcher.enqueueOrFail(requestContext, TypeSendPushNotification, payload, asynq.Queue(QueueDefault), asynq.MaxRetry(3), asynq.TaskID("push:"+inboxMessageID))
}

// enqueueOrFail is enqueue for callers that must know whether the job was
// saved (the push outbox only marks a message as pushed when it was).
// A job that is already queued counts as success.
func (dispatcher *Dispatcher) enqueueOrFail(requestContext context.Context, jobType string, payload any, options ...asynq.Option) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = dispatcher.client.EnqueueContext(requestContext, asynq.NewTask(jobType, data), options...)
	if err == asynq.ErrTaskIDConflict || err == asynq.ErrDuplicateTask {
		return nil
	}
	return err
}

// enqueue turns the payload into JSON and saves the job in Redis.
// A failure to queue is logged but does not fail the user's request:
// the money part already committed, and the jobs can be re-driven.
func (dispatcher *Dispatcher) enqueue(requestContext context.Context, jobType string, payload any, options ...asynq.Option) {
	data, err := json.Marshal(payload)
	if err != nil {
		slog.Error("could not encode job", "type", jobType, "error", err)
		return
	}
	if _, err := dispatcher.client.EnqueueContext(requestContext, asynq.NewTask(jobType, data), options...); err != nil {
		if err == asynq.ErrTaskIDConflict || err == asynq.ErrDuplicateTask {
			return // already queued: that's fine
		}
		slog.Error("could not queue job", "type", jobType, "error", err)
	}
}
