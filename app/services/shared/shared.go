// Package shared holds the small helpers every money service uses, so each
// service reads the same way:
//
//  1. check the PIN                      (auth.VerifyPin, before the transaction)
//  2. make sure ledger accounts exist   (ledger.EnsureUserAccount, before the transaction)
//  3. database.WithTransaction(func(databaseTransaction) {
//     shared.LockActor(...)          // user still active? phone still the active one?
//     ... consume quote / lock product rows ...
//     ledger.Post(...)               // the money moves (or nothing does)
//     shared.RecordTransaction(...)  // the row the user sees in Activity
//     shared.Notify(...)             // inbox message
//     })
//  4. after commit: queue jobs (push notification, provider payout ...)
package shared

import (
	"context"
	"encoding/json"
	"time"

	"gorm.io/gorm"

	"github.com/tribe-africa/tribe-backend/app/apperrors"
	"github.com/tribe-africa/tribe-backend/app/cache"
	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/database"
	"github.com/tribe-africa/tribe-backend/pkg/ids"
)

// Actor is the signed-in user making a request, and the phone they use.
type Actor struct {
	UserID   string
	DeviceID string
}

// LockActor re-checks INSIDE the money transaction that:
//   - the user is not frozen or closed, and
//   - this phone is still the active device.
//
// It uses SHARED locks (LOCK IN SHARE MODE): many money requests can hold
// them at once, but a device switch (which needs an exclusive lock on the
// user row) must wait until they finish. So a phone that is being replaced
// can never sneak a payment through at the same moment.
func LockActor(databaseTransaction *gorm.DB, actor Actor) (*models.User, error) {
	var user models.User
	if err := database.LockForShare(databaseTransaction, &user, "id = ?", actor.UserID); err != nil {
		return nil, apperrors.ErrUnauthenticated
	}
	if user.Status != "active" {
		return nil, apperrors.ErrAccountFrozen
	}
	if actor.DeviceID != "" {
		var device models.UserDevice
		err := database.LockForShare(databaseTransaction, &device, "id = ? AND user_id = ? AND status = ?", actor.DeviceID, actor.UserID, models.DeviceStatusActive)
		if err != nil {
			return nil, apperrors.ErrDeviceNotActive
		}
	}
	return &user, nil
}

// RecordTransaction saves the user-facing transaction row.
func RecordTransaction(databaseTransaction *gorm.DB, transaction *models.Transaction) error {
	now := time.Now().UTC().Truncate(time.Microsecond)
	if transaction.ID == "" {
		transaction.ID = ids.New()
	}
	if transaction.Metadata == nil {
		transaction.Metadata = []byte("{}")
	}
	if transaction.Version == 0 {
		transaction.Version = 1
	}
	if transaction.FeeAmount == "" {
		transaction.FeeAmount = "0"
	}
	if transaction.UsdValue == "" {
		transaction.UsdValue = "0"
	}
	transaction.CreatedAt, transaction.UpdatedAt = now, now
	if transaction.Status == models.TransactionStatusCompleted {
		transaction.CompletedAt = &now
	}
	return databaseTransaction.Create(transaction).Error
}

// Notify adds a message to the user's in-app inbox.
func Notify(databaseTransaction *gorm.DB, userID string, kind string, title string, body string, href string) error {
	message := models.InboxMessage{ID: ids.New(), UserID: userID, Kind: kind, Title: title, Body: body, CreatedAt: time.Now().UTC()}
	if href != "" {
		message.Href = &href
	}
	return databaseTransaction.Create(&message).Error
}

// NotifyTransaction writes the inbox message for a money movement, linking
// to the transaction. Like Notify it runs INSIDE the money transaction, so
// the message (and the push the scheduler sends for it) only exists if the
// money really moved.
func NotifyTransaction(databaseTransaction *gorm.DB, transaction *models.Transaction, kind string, title string) error {
	return Notify(databaseTransaction, transaction.UserID, kind, title, transaction.Title, "/transaction/"+transaction.ID)
}

// JSON turns any value into a JSON column value.
func JSON(value any) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		return []byte("{}")
	}
	return encoded
}

// configCache is the shared copy of assets, providers, margins, fees and
// service switches (see cache.ConfigCache). bootstrap installs it with
// UseConfigCache. When it is not installed (a small tool), the helpers
// below and in services.go read MySQL.
var configCache *cache.ConfigCache

// UseConfigCache makes LoadAsset, AssetDecimals and the service switch
// checks read the shared configuration copy instead of MySQL on every
// call. Called once at start.
func UseConfigCache(shared *cache.ConfigCache) {
	configCache = shared
}

// configSnapshot returns the shared configuration, or nil to mean "read
// MySQL" (no cache installed, or it could not load).
func configSnapshot(query *gorm.DB) *cache.ConfigSnapshot {
	if configCache == nil {
		return nil
	}
	requestContext := query.Statement.Context
	if requestContext == nil {
		requestContext = context.Background()
	}
	snapshot, err := configCache.Snapshot(requestContext)
	if err != nil {
		return nil
	}
	return snapshot
}

// LoadAsset returns one enabled asset (or a NOT_FOUND error).
func LoadAsset(query *gorm.DB, assetCode string) (*models.Asset, error) {
	if snapshot := configSnapshot(query); snapshot != nil {
		asset, found := snapshot.Assets[assetCode]
		if !found || !asset.Enabled {
			return nil, apperrors.NotFound("Currency " + assetCode)
		}
		return &asset, nil // a copy: callers may change it safely
	}
	var asset models.Asset
	if err := query.Where("code = ? AND enabled = ?", assetCode, true).Take(&asset).Error; err != nil {
		return nil, apperrors.NotFound("Currency " + assetCode)
	}
	return &asset, nil
}

// AssetDecimals returns code -> decimal places for every asset.
func AssetDecimals(query *gorm.DB) map[string]int {
	var assets []models.Asset
	if snapshot := configSnapshot(query); snapshot != nil {
		for _, asset := range snapshot.Assets {
			assets = append(assets, asset)
		}
	} else {
		query.Find(&assets)
	}
	decimals := make(map[string]int, len(assets))
	for _, asset := range assets {
		decimals[asset.Code] = asset.DecimalPlaces
	}
	return decimals
}

// KycDailyLimits is the daily spending limit each KYC tier unlocks, as shown
// to the customer. It matches TIER_LIMITS in tribe-mobile
// (src/stores/use-session-store.ts); change both together.
var KycDailyLimits = map[int]string{
	0: "₦50,000",
	1: "₦1,000,000",
	2: "₦5,000,000",
	3: "₦25,000,000",
}
