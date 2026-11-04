// Package webhooks receives messages that providers send to us, such as
// "your payout was delivered".
//
// A webhook is an HTTP request FROM the provider TO us. Anyone on the
// internet could send one, so every webhook goes through three checks:
//
//  1. Signature: the provider signs the raw body with a shared secret
//     (HMAC-SHA256). We compute the same signature and compare. A wrong
//     signature is rejected before we read anything.
//  2. Once only: each event is stored with UNIQUE(provider_slug, event_id).
//     Providers often send the same event twice; the second copy is ignored.
//  3. Never trust the body for money: a payout event only queues the payout
//     job again, and that job ASKS the provider for the real status before
//     posting anything to the ledger.
package webhooks

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/tribe-africa/tribe-backend/app/apperrors"
	"github.com/tribe-africa/tribe-backend/app/jobs"
	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/config"
	"github.com/tribe-africa/tribe-backend/database"
	"github.com/tribe-africa/tribe-backend/pkg/ids"
)

// SimulatorSecret signs webhooks from simulator providers on a developer
// machine, so you can try the endpoint with curl (see docs/API.md).
const SimulatorSecret = "simulator-webhook-secret"

// Service stores and handles provider webhooks.
type Service struct {
	database   *gorm.DB
	dispatcher *jobs.Dispatcher
	settings   config.Config
}

// NewService creates the webhook service.
func NewService(connection *gorm.DB, dispatcher *jobs.Dispatcher, settings config.Config) *Service {
	return &Service{database: connection, dispatcher: dispatcher, settings: settings}
}

// Event is the body every provider webhook is turned into.
type Event struct {
	EventID string `json:"event_id"`
	Type    string `json:"type"` // e.g. payout.succeeded, payout.failed
	Data    struct {
		TransactionID string `json:"transaction_id"`
	} `json:"data"`
}

// Outcome tells the controller what happened.
type Outcome struct {
	EventID   string `json:"event_id"`
	Duplicate bool   `json:"duplicate"` // true when we had already received this event
	Action    string `json:"action"`    // what we did, e.g. "payout re-checked"
}

// Receive checks the signature, stores the event once and acts on it.
func (service *Service) Receive(requestContext context.Context, providerSlug string, rawBody []byte, signature string) (*Outcome, error) {
	var provider models.Provider
	if err := service.database.WithContext(requestContext).Where("slug = ? AND enabled = ?", providerSlug, true).Take(&provider).Error; err != nil {
		return nil, apperrors.NotFound("Provider")
	}
	secret := service.secretFor(provider)
	if secret == "" {
		return nil, apperrors.New(http.StatusServiceUnavailable, "WEBHOOK_NOT_CONFIGURED", "Webhooks are not set up for this provider.")
	}
	if !SignatureMatches(secret, rawBody, signature) {
		return nil, apperrors.New(http.StatusUnauthorized, "BAD_SIGNATURE", "The webhook signature is not valid.")
	}

	var event Event
	if err := json.Unmarshal(rawBody, &event); err != nil || event.EventID == "" || len(event.EventID) > 100 || event.Type == "" {
		return nil, apperrors.Validation(map[string][]string{"event_id": {"A webhook needs an event_id and a type."}})
	}

	record := models.ProviderWebhookEvent{
		ID: ids.New(), ProviderSlug: provider.Slug, EventID: event.EventID, EventType: event.Type,
		Payload: rawBody, Status: "received", ReceivedAt: time.Now().UTC(),
	}
	if err := service.database.WithContext(requestContext).Create(&record).Error; err != nil {
		if database.IsDuplicateEntry(err) {
			return &Outcome{EventID: event.EventID, Duplicate: true, Action: "ignored (already received)"}, nil
		}
		return nil, err
	}

	action, handleError := service.handle(requestContext, event)
	updates := map[string]any{"status": "processed", "processed_at": time.Now().UTC()}
	if handleError != nil {
		updates["status"] = "failed"
		updates["error_message"] = truncate(handleError.Error(), 255)
	}
	service.database.WithContext(requestContext).Model(&models.ProviderWebhookEvent{}).Where("id = ?", record.ID).Updates(updates)
	if handleError != nil {
		return nil, handleError
	}
	return &Outcome{EventID: event.EventID, Action: action}, nil
}

// handle acts on one stored event. Unknown event types are kept for the
// record but do nothing.
func (service *Service) handle(requestContext context.Context, event Event) (string, error) {
	switch event.Type {
	case "payout.succeeded", "payout.failed":
		var transaction models.Transaction
		err := service.database.WithContext(requestContext).Where("id = ?", event.Data.TransactionID).Take(&transaction).Error
		if err != nil {
			return "", apperrors.NotFound("Transaction")
		}
		// The payout job asks the provider for the real status and posts the
		// result. TaskID makes it a no-op if the job is already queued.
		service.dispatcher.ProcessPayout(requestContext, transaction.ID)
		return "payout re-checked", nil
	default:
		return "stored (no action for this type)", nil
	}
}

// secretFor returns the signing secret for a provider: the environment
// variable WEBHOOK_SECRET_<SLUG>, e.g. WEBHOOK_SECRET_GLOBAL_SIMULATOR.
// Simulators on a developer machine fall back to SimulatorSecret.
func (service *Service) secretFor(provider models.Provider) string {
	name := "WEBHOOK_SECRET_" + strings.ToUpper(strings.ReplaceAll(provider.Slug, "-", "_"))
	if secret := strings.TrimSpace(os.Getenv(name)); secret != "" {
		return secret
	}
	if provider.IsSimulator && !service.settings.IsProduction() {
		return SimulatorSecret
	}
	return ""
}

// Sign returns the signature header value for a body: "sha256=<hex>".
func Sign(secret string, rawBody []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(rawBody)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// SignatureMatches compares signatures in constant time, so an attacker
// cannot learn the right signature one character at a time.
func SignatureMatches(secret string, rawBody []byte, signature string) bool {
	return hmac.Equal([]byte(Sign(secret, rawBody)), []byte(strings.TrimSpace(signature)))
}

// truncate cuts text to a maximum length.
func truncate(text string, maximumLength int) string {
	if len(text) <= maximumLength {
		return text
	}
	return text[:maximumLength]
}
