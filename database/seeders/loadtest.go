package seeders

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tribe-africa/tribe-backend/app/bootstrap"
	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/app/services/ledger"
	"github.com/tribe-africa/tribe-backend/app/services/shared"
	"github.com/tribe-africa/tribe-backend/pkg/ids"
)

// LoadTestPassword and LoadTestPin are shared by every load-test customer.
const (
	LoadTestPassword = "LoadTest@2026"
	LoadTestPin      = "2580"
)

// LoadTestEmail is the email of load-test customer number index (1, 2, ...).
// cmd/loadtest signs in with the same formula.
func LoadTestEmail(index int) string { return fmt.Sprintf("loadtest-%d@tribe.test", index) }

// LoadTestDevice is the phone id of load-test customer number index. It is
// already their ACTIVE phone, so signing in needs no emailed device code.
func LoadTestDevice(index int) string { return fmt.Sprintf("loadtest-device-%d", index) }

// SeedLoadTestCustomers creates `count` customers for a load test, each with
// an active phone, a PIN and funded NGN and USD wallets (through the real
// ledger, like the demo customers). It is safe to run again: existing
// customers are skipped and the funding references are fixed.
//
// Refused in production: these accounts share a known password and PIN.
func SeedLoadTestCustomers(requestContext context.Context, application *bootstrap.App, count int) error {
	if application.Settings.IsProduction() {
		return errors.New("load-test customers are never created in production")
	}
	query := application.Database.WithContext(requestContext)
	// Hash ONCE and reuse: each hash costs 64 MB and real time, and every
	// load-test customer has the same password and PIN anyway.
	passwordHash := application.Toolkit.HashPassword(LoadTestPassword)
	pinHash := application.Toolkit.HashPassword(LoadTestPin)
	now := time.Now().UTC()

	for index := 1; index <= count; index++ {
		email := LoadTestEmail(index)
		var user models.User
		if query.Where("email = ?", email).Take(&user).Error != nil {
			tag := fmt.Sprintf("lt%d", index)
			user = models.User{
				ID: ids.New(), Email: email, PasswordHash: passwordHash, FirstName: "Load", LastName: fmt.Sprintf("Test %d", index),
				Tag: tag, Country: "NG", Status: "active", KycTier: 1, KycStatus: "approved", GlobalAccountsStatus: "none",
				ReferralCode: fmt.Sprintf("LT-%d", index), Labels: []byte("[]"), EmailVerifiedAt: &now, Version: 1, CreatedAt: now, UpdatedAt: now,
			}
			if err := query.Create(&user).Error; err != nil {
				return err
			}
			if err := query.Create(&models.UserPin{UserID: user.ID, PinHash: pinHash, UpdatedAt: now}).Error; err != nil {
				return err
			}
			if err := query.Create(&models.UserDevice{
				ID: ids.New(), UserID: user.ID, DeviceImei: LoadTestDevice(index), Name: "Load test", Platform: "loadtest",
				Status: models.DeviceStatusActive, ActivatedAt: &now, CreatedAt: now, UpdatedAt: now,
			}).Error; err != nil {
				return err
			}
		}
		for _, assetCode := range []string{"NGN", "USD"} {
			if _, err := application.Ledger.EnsureUserAccount(requestContext, user.ID, ledger.CodeUserPrimary, assetCode); err != nil {
				return err
			}
		}
		if _, err := application.Ledger.EnsureUserAccount(requestContext, user.ID, ledger.CodeUserReferral, "NGN"); err != nil {
			return err
		}
		for assetCode, amount := range map[string]string{"NGN": "5000000", "USD": "5000"} {
			reference := fmt.Sprintf("LOADTEST-%d-%s", index, assetCode)
			var existing int64
			query.Model(&models.Transaction{}).Where("reference = ?", reference).Count(&existing)
			if existing > 0 {
				continue
			}
			if _, err := application.Wallets.TestTopUp(requestContext, shared.Actor{UserID: user.ID}, assetCode, amount, reference); err != nil {
				return fmt.Errorf("funding %s %s: %w", email, assetCode, err)
			}
		}
	}
	return nil
}
