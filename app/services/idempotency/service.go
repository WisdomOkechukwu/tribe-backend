// Package idempotency makes "do it once" requests safe to retry.
//
// The problem: a user taps "Send" and their signal drops. Did the money go?
// The app does not know, so it sends the SAME request again. Without
// protection the user would pay twice.
//
// The fix: the app sends a random Idempotency-Key header (a UUID) with every
// change, and REUSES the same key when it retries. We remember each key:
//
//	first time we see the key      -> do the work, save the answer
//	same key, work finished        -> send back the SAVED answer (no new work)
//	same key, work still running   -> 409 REQUEST_IN_PROGRESS
//	same key, DIFFERENT body       -> 422 IDEMPOTENCY_KEY_REUSED (a bug in the app)
package idempotency

import (
	"context"
	"net/http"
	"time"

	"gorm.io/gorm"

	"github.com/tribe-africa/tribe-backend/app/apperrors"
	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/database"
	"github.com/tribe-africa/tribe-backend/pkg/ids"
)

// Statuses of a remembered request.
const (
	StatusProcessing = "processing"
	StatusCompleted  = "completed"
)

// processingLease is how long a request may run before another attempt may take over.
const processingLease = 60 * time.Second

// Service stores and checks idempotency keys.
type Service struct {
	database *gorm.DB
}

// NewService creates the idempotency service.
func NewService(connection *gorm.DB) *Service {
	return &Service{database: connection}
}

// Request describes the incoming request.
type Request struct {
	ActorType   string // user | admin
	ActorID     string
	Key         string
	Method      string
	Path        string
	RequestHash string
}

// StoredResponse is a saved answer we can replay.
type StoredResponse struct {
	Status int
	Body   []byte
}

// Begin is called before the work. It returns either:
//   - record (start the work), or
//   - stored (replay this saved answer and do nothing else), or
//   - an error (in progress / key reused).
func (service *Service) Begin(requestContext context.Context, request Request) (record *models.IdempotencyKey, stored *StoredResponse, err error) {
	now := time.Now().UTC()
	newRecord := models.IdempotencyKey{
		ID:             ids.New(),
		ActorType:      request.ActorType,
		ActorID:        request.ActorID,
		IdempotencyKey: request.Key,
		RequestMethod:  request.Method,
		RequestPath:    request.Path,
		RequestHash:    request.RequestHash,
		Status:         StatusProcessing,
		LockedUntil:    now.Add(processingLease),
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	// The UNIQUE(actor_type, actor_id, idempotency_key) index decides who is first.
	err = service.database.WithContext(requestContext).Create(&newRecord).Error
	if err == nil {
		return &newRecord, nil, nil
	}
	if !database.IsDuplicateEntry(err) {
		return nil, nil, err
	}

	// We have seen this key before.
	var existing models.IdempotencyKey
	err = service.database.WithContext(requestContext).
		Where("actor_type = ? AND actor_id = ? AND idempotency_key = ?", request.ActorType, request.ActorID, request.Key).
		Take(&existing).Error
	if err != nil {
		return nil, nil, err
	}
	if existing.RequestHash != request.RequestHash || existing.RequestPath != request.Path {
		return nil, nil, apperrors.New(http.StatusUnprocessableEntity, "IDEMPOTENCY_KEY_REUSED", "This request key was already used for a different request.")
	}
	if existing.Status == StatusCompleted && existing.ResponseStatus != nil {
		return nil, &StoredResponse{Status: *existing.ResponseStatus, Body: existing.ResponseBody}, nil
	}
	if existing.LockedUntil.After(now) {
		return nil, nil, apperrors.New(http.StatusConflict, "REQUEST_IN_PROGRESS", "We're still working on this. Please wait a moment.")
	}

	// The first attempt crashed and its lease ran out: take it over. The
	// UPDATE only succeeds for ONE taker (the WHERE checks the old lease).
	result := service.database.WithContext(requestContext).Model(&models.IdempotencyKey{}).
		Where("id = ? AND status = ? AND locked_until <= ?", existing.ID, StatusProcessing, now).
		Updates(map[string]any{"locked_until": now.Add(processingLease), "updated_at": now})
	if result.Error != nil {
		return nil, nil, result.Error
	}
	if result.RowsAffected != 1 {
		return nil, nil, apperrors.New(http.StatusConflict, "REQUEST_IN_PROGRESS", "We're still working on this. Please wait a moment.")
	}
	return &existing, nil, nil
}

// Complete saves the answer so retries get the same answer.
func (service *Service) Complete(requestContext context.Context, recordID string, status int, body []byte) {
	service.database.WithContext(requestContext).Model(&models.IdempotencyKey{}).Where("id = ?", recordID).
		Updates(map[string]any{"status": StatusCompleted, "response_status": status, "response_body": body, "updated_at": time.Now().UTC()})
}

// Release forgets a request that failed with a server error, so the app may retry it.
func (service *Service) Release(requestContext context.Context, recordID string) {
	service.database.WithContext(requestContext).Where("id = ? AND status = ?", recordID, StatusProcessing).Delete(&models.IdempotencyKey{})
}

// DeleteOlderThan removes old keys (run by the hourly cleanup cron), in
// small batches so a big backlog never locks the table for long.
func (service *Service) DeleteOlderThan(requestContext context.Context, age time.Duration) (int64, error) {
	return database.DeleteInBatches(requestContext, service.database, "idempotency_keys", "created_at < ?", time.Now().UTC().Add(-age))
}
