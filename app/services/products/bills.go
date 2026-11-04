// Package products holds the smaller money products: bills, gift cards,
// virtual cards, cashpoints and referrals. They all follow the same recipe
// as swaps (see app/services/shared).
package products

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"time"

	"gorm.io/gorm"

	"github.com/tribe-africa/tribe-backend/app/apperrors"
	"github.com/tribe-africa/tribe-backend/app/jobs"
	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/app/services/auth"
	"github.com/tribe-africa/tribe-backend/app/services/ledger"
	"github.com/tribe-africa/tribe-backend/app/services/providers"
	"github.com/tribe-africa/tribe-backend/app/services/rates"
	"github.com/tribe-africa/tribe-backend/app/services/security"
	"github.com/tribe-africa/tribe-backend/app/services/shared"
	"github.com/tribe-africa/tribe-backend/app/services/swaps"
	"github.com/tribe-africa/tribe-backend/config"
	"github.com/tribe-africa/tribe-backend/database"
	"github.com/tribe-africa/tribe-backend/pkg/ids"
	"github.com/tribe-africa/tribe-backend/pkg/money"
)

// Service runs the product flows.
type Service struct {
	database   *gorm.DB
	ledger     *ledger.Service
	rates      *rates.Service
	auth       *auth.Service
	providers  *providers.Registry
	dispatcher *jobs.Dispatcher
	toolkit    *security.Toolkit
	settings   config.Config
}

// NewService creates the products service.
func NewService(connection *gorm.DB, ledgerService *ledger.Service, ratesService *rates.Service, authService *auth.Service, registry *providers.Registry, dispatcher *jobs.Dispatcher, toolkit *security.Toolkit, settings config.Config) *Service {
	return &Service{database: connection, ledger: ledgerService, rates: ratesService, auth: authService, providers: registry, dispatcher: dispatcher, toolkit: toolkit, settings: settings}
}

// nairaPerKwh is the illustrative electricity price used for the units shown.
var nairaPerKwh = big.NewRat(4386, 10)

// ListBillers returns every enabled biller with its plans.
func (service *Service) ListBillers(requestContext context.Context) ([]models.Biller, error) {
	var billers []models.Biller
	err := service.database.WithContext(requestContext).Where("enabled = ?", true).Order("sort_order").Find(&billers).Error
	return billers, err
}

// VerifyBillCustomer asks the provider whose meter/phone/smartcard this is.
func (service *Service) VerifyBillCustomer(requestContext context.Context, billerID string, target string) (string, error) {
	billProvider, err := service.providers.Bills()
	if err != nil {
		return "", err
	}
	name, err := billProvider.VerifyCustomer(requestContext, billerID, target)
	if err != nil {
		return "", apperrors.New(http.StatusUnprocessableEntity, "CUSTOMER_NOT_FOUND", "We couldn't find that customer number.")
	}
	return name, nil
}

// PayBillInput is what the bill review screen sends.
type PayBillInput struct {
	BillerID  string
	Target    string
	PlanID    string
	AmountNgn string // whole Naira as typed, e.g. "5000"
	PayWith   string // asset code
	Pin       string
	Reference string
	SkipPin   bool // true only for scheduled recurring payments (consent given when created)
}

// PayBill pays a bill with any asset, in two steps so the slow biller is
// never called while database rows are locked:
//
//  1. In ONE transaction: take the money and park it in Purchases In
//     Transit NGN, and save the transaction as "pending". (Paying with USD
//     for a ₦5,000 bill posts a USD -> NGN conversion that lands there;
//     paying with NGN is Dr User NGN / Cr Purchases In Transit NGN + fee.)
//     The pending row is the "outbox": it is saved in the same commit as
//     the money, so the purchase can never be forgotten.
//  2. After the commit, outside any transaction, ask the biller (see
//     ProcessPurchase). It confirms -> Dr In Transit / Cr Company Vault NGN
//     ("the money left the company"); it refuses -> the step-1 posting is
//     reversed and the customer gets everything back; it does not answer ->
//     the purchase stays pending and a queue job asks again.
func (service *Service) PayBill(requestContext context.Context, actor shared.Actor, input PayBillInput) (*models.Transaction, error) {
	if !input.SkipPin {
		if err := service.auth.VerifyPin(requestContext, actor.UserID, input.Pin); err != nil {
			return nil, err
		}
	}
	var biller models.Biller
	if err := service.database.WithContext(requestContext).Where("id = ? AND enabled = ?", input.BillerID, true).Take(&biller).Error; err != nil {
		return nil, apperrors.NotFound("Biller")
	}
	amountNgn, err := money.ParseDecimal(input.AmountNgn, 2)
	if err != nil || !amountNgn.IsPositive() {
		return nil, apperrors.Validation(map[string][]string{"amount_ngn": {"Enter a valid amount."}})
	}

	// Price: how much of the pay-with asset buys this many Naira (fee included).
	payWith := input.PayWith
	if payWith == "" {
		payWith = "NGN"
	}
	var price *rates.Price
	fee := money.Zero()
	cost := amountNgn
	if payWith != "NGN" {
		price, err = service.rates.PriceConversion(requestContext, rates.PriceInput{TransactionType: "bill", FromAsset: payWith, ToAsset: "NGN", ReceiveAmount: amountNgn.Decimal(2)})
		if err != nil {
			return nil, err
		}
		fee, cost = price.Fee, price.Converted
	} else if fee, err = service.rates.FeeFor(service.database.WithContext(requestContext), "bill", "NGN", amountNgn); err != nil {
		return nil, err
	}

	userWallet, err := service.ledger.EnsureUserAccount(requestContext, actor.UserID, ledger.CodeUserPrimary, payWith)
	if err != nil {
		return nil, err
	}
	// Fail early (before taking money) if no bill provider is configured.
	if _, err := service.providers.Bills(); err != nil {
		return nil, err
	}
	// Opened outside the transaction (databases seeded before this account existed).
	if _, err := service.ledger.EnsureCompanyAccount(requestContext, ledger.PurchaseInTransitTemplate(), "NGN"); err != nil {
		return nil, err
	}

	transaction := &models.Transaction{ID: ids.New(), Reference: input.Reference, UserID: actor.UserID}
	err = database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		if _, err := shared.LockActor(databaseTransaction, actor); err != nil {
			return err
		}
		inTransit, err := service.ledger.CompanyAccount(databaseTransaction, ledger.CodePurchaseInTransit, "NGN")
		if err != nil {
			return err
		}
		posting := ledger.ConversionPosting{
			Reference: input.Reference + ":POST", TransactionType: "bill", Description: biller.Name + " " + input.Target,
			CreatedBy: "user:" + actor.UserID, BusinessTransactionID: &transaction.ID,
			SourceAccountID: userWallet.ID, TargetAccountID: inTransit.ID,
			FromAsset: payWith, ToAsset: "NGN", Converted: cost, Fee: fee, Received: amountNgn,
			Rates: service.rates.RatesFor(databaseTransaction, payWith, "NGN"),
		}
		if price != nil {
			posting.NairaIn, posting.NairaOut = price.NairaIn, price.NairaOut
		}
		postResult, err := service.ledger.PostConversion(databaseTransaction, posting)
		if err != nil {
			return err
		}

		ngn := "NGN"
		amountText := amountNgn.UnitsString()
		feeAsset := payWith
		// Everything ProcessPurchase needs to ask the biller later.
		metadata := map[string]any{"biller_id": biller.ID, "bill_category": biller.Category, "target": input.Target, "plan": input.PlanID, "amount_ngn": amountText}
		*transaction = models.Transaction{
			ID: transaction.ID, Reference: transaction.Reference, UserID: actor.UserID,
			Category: "utility", Type: "bill", Direction: "out", Status: models.TransactionStatusPending,
			Title: biller.Name, Subtitle: input.Target,
			FromAsset: payWith, FromAmount: cost.Add(fee).UnitsString(), ToAsset: &ngn, ToAmount: &amountText,
			FeeAsset: &feeAsset, FeeAmount: fee.UnitsString(), JournalEntryID: &postResult.Entry.ID,
			UsdValue: service.rates.UsdValueOf(databaseTransaction, "NGN", amountNgn, 2),
			Metadata: shared.JSON(metadata),
		}
		if err := shared.RecordTransaction(databaseTransaction, transaction); err != nil {
			return err
		}
		if price != nil {
			if err := swaps.SaveSwapLegs(databaseTransaction, transaction.ID, nil, price.Route); err != nil {
				return err
			}
		}
		planID := input.PlanID
		payment := models.BillPayment{
			ID: ids.New(), UserID: actor.UserID, TransactionID: transaction.ID, BillerID: biller.ID, Target: input.Target,
			AmountNgn: amountText, PayWithAsset: payWith, Status: "pending", CreatedAt: time.Now().UTC(),
		}
		if planID != "" {
			payment.PlanID = &planID
		}
		// The "paid" or "refunded" inbox message is written when the biller answers.
		return databaseTransaction.Create(&payment).Error
	})
	if err != nil {
		return nil, err
	}
	// Step 2, after the commit: ask the biller (see finishPurchase).
	return service.finishPurchase(requestContext, transaction.ID, "The biller could not complete this payment, so your money was returned.")
}

// ListVaultTokens returns the user's electricity tokens.
func (service *Service) ListVaultTokens(requestContext context.Context, userID string) ([]models.VaultToken, error) {
	var tokens []models.VaultToken
	err := service.database.WithContext(requestContext).Where("user_id = ?", userID).Order("bought_at DESC").Find(&tokens).Error
	return tokens, err
}

// MarkVaultTokenLoaded records that the user typed the token into their meter.
func (service *Service) MarkVaultTokenLoaded(requestContext context.Context, userID string, tokenID string, loaded bool) error {
	var loadedOn any = nil
	if loaded {
		loadedOn = time.Now().UTC()
	}
	result := service.database.WithContext(requestContext).Model(&models.VaultToken{}).Where("id = ? AND user_id = ?", tokenID, userID).Update("loaded_on", loadedOn)
	if result.RowsAffected == 0 {
		return apperrors.NotFound("Token")
	}
	return result.Error
}

// ─── Recurring bills ─────────────────────────────────────────────────

// RecurringInput creates or changes a recurring bill.
type RecurringInput struct {
	BillerID  string
	Target    string
	PlanID    string
	AmountNgn string
	PayWith   string
	Frequency string // weekly | biweekly | monthly
	StartDate string // YYYY-MM-DD
	Active    *bool
	Version   int // the version the app last saw (stops two phones overwriting each other)
}

// CreateRecurring saves a schedule. Consent is given with the PIN here, so
// the scheduler can pay it later without asking again.
func (service *Service) CreateRecurring(requestContext context.Context, userID string, pin string, input RecurringInput) (*models.RecurringBill, error) {
	if err := service.auth.VerifyPin(requestContext, userID, pin); err != nil {
		return nil, err
	}
	amount, err := money.ParseDecimal(input.AmountNgn, 2)
	if err != nil || !amount.IsPositive() {
		return nil, apperrors.Validation(map[string][]string{"amount_ngn": {"Enter a valid amount."}})
	}
	if input.Frequency != "weekly" && input.Frequency != "biweekly" && input.Frequency != "monthly" {
		return nil, apperrors.Validation(map[string][]string{"frequency": {"Choose weekly, biweekly or monthly."}})
	}
	start, err := time.ParseInLocation(time.DateOnly, input.StartDate, service.settings.App.TimeZone)
	if err != nil {
		start = time.Now().In(service.settings.App.TimeZone)
	}
	now := time.Now().UTC()
	schedule := models.RecurringBill{
		ID: ids.New(), UserID: userID, BillerID: input.BillerID, Target: input.Target, AmountNgn: amount.UnitsString(),
		PayWithAsset: input.PayWith, Frequency: input.Frequency, NextRunAt: start.Add(7 * time.Hour).UTC(),
		Active: true, Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	if input.PlanID != "" {
		schedule.PlanID = &input.PlanID
	}
	return &schedule, service.database.WithContext(requestContext).Create(&schedule).Error
}

// ListRecurring returns the user's schedules.
func (service *Service) ListRecurring(requestContext context.Context, userID string) ([]models.RecurringBill, error) {
	var schedules []models.RecurringBill
	err := service.database.WithContext(requestContext).Where("user_id = ?", userID).Order("created_at DESC").Find(&schedules).Error
	return schedules, err
}

// UpdateRecurring changes a schedule using the LOCK + VERSION pattern:
//  1. lock the row (FOR UPDATE) so nobody else changes it now,
//  2. if its version is not the one the app saw, someone changed it in
//     between: refuse with STALE_DATA instead of silently overwriting,
//  3. write the change and bump the version.
//
// Changing what gets paid (amount, wallet, frequency) or switching a schedule
// back on is new consent to future payments, so it needs the PIN again.
// Pausing (active=false) does not.
func (service *Service) UpdateRecurring(requestContext context.Context, userID string, scheduleID string, pin string, input RecurringInput) (*models.RecurringBill, error) {
	turningOn := input.Active != nil && *input.Active
	if input.AmountNgn != "" || input.PayWith != "" || input.Frequency != "" || turningOn {
		if err := service.auth.VerifyPin(requestContext, userID, pin); err != nil {
			return nil, err
		}
	}
	var schedule models.RecurringBill
	err := database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		if err := database.LockForUpdate(databaseTransaction, &schedule, "id = ? AND user_id = ?", scheduleID, userID); err != nil {
			return apperrors.NotFound("Recurring bill")
		}
		if input.Version != 0 && input.Version != schedule.Version {
			return apperrors.ErrStaleData
		}
		updates := map[string]any{"updated_at": time.Now().UTC(), "version": schedule.Version + 1}
		if input.Active != nil {
			updates["active"] = *input.Active
		}
		if input.AmountNgn != "" {
			amount, err := money.ParseDecimal(input.AmountNgn, 2)
			if err != nil || !amount.IsPositive() {
				return apperrors.Validation(map[string][]string{"amount_ngn": {"Enter a valid amount."}})
			}
			updates["amount_ngn"] = amount.UnitsString()
		}
		if input.Frequency != "" {
			updates["frequency"] = input.Frequency
		}
		if input.PayWith != "" {
			updates["pay_with_asset"] = input.PayWith
		}
		if err := databaseTransaction.Model(&models.RecurringBill{}).Where("id = ?", scheduleID).Updates(updates).Error; err != nil {
			return err
		}
		return databaseTransaction.Where("id = ?", scheduleID).Take(&schedule).Error
	})
	return &schedule, err
}

// DeleteRecurring removes a schedule.
func (service *Service) DeleteRecurring(requestContext context.Context, userID string, scheduleID string) error {
	result := service.database.WithContext(requestContext).Where("id = ? AND user_id = ?", scheduleID, userID).Delete(&models.RecurringBill{})
	if result.RowsAffected == 0 {
		return apperrors.NotFound("Recurring bill")
	}
	return result.Error
}

// RunDueRecurringBills pays every schedule that is due (called by cron at 07:00).
// Each schedule is claimed with an UPDATE that moves next_run_at forward;
// only the scheduler whose UPDATE changed the row pays it (no double payment).
func (service *Service) RunDueRecurringBills(requestContext context.Context) (paid int, skipped int, err error) {
	var due []models.RecurringBill
	if err := service.database.WithContext(requestContext).Where("active = ? AND next_run_at <= ?", true, time.Now().UTC()).Limit(500).Find(&due).Error; err != nil {
		return 0, 0, err
	}
	for _, schedule := range due {
		nextRun := nextRunAfter(schedule.NextRunAt, schedule.Frequency)
		claim := service.database.WithContext(requestContext).Model(&models.RecurringBill{}).
			Where("id = ? AND next_run_at = ?", schedule.ID, schedule.NextRunAt).
			Updates(map[string]any{"next_run_at": nextRun, "version": gorm.Expr("version + 1")})
		if claim.Error != nil || claim.RowsAffected != 1 {
			continue // another scheduler took it
		}
		amount, _ := money.ParseUnits(schedule.AmountNgn)
		reference := fmt.Sprintf("REC-%s-%s", schedule.ID, schedule.NextRunAt.Format("20060102"))
		planID := ""
		if schedule.PlanID != nil {
			planID = *schedule.PlanID
		}
		_, payError := service.PayBill(requestContext, shared.Actor{UserID: schedule.UserID}, PayBillInput{
			BillerID: schedule.BillerID, Target: schedule.Target, PlanID: planID, AmountNgn: amount.Decimal(2),
			PayWith: schedule.PayWithAsset, Reference: reference, SkipPin: true,
		})
		result := "paid"
		if payError != nil {
			skipped++
			result = "skipped: " + payError.Error()
			service.database.WithContext(requestContext).Create(&models.InboxMessage{
				ID: ids.New(), UserID: schedule.UserID, Kind: "money", Title: "Recurring bill skipped",
				Body: "We couldn't pay your scheduled bill for " + schedule.Target + ". Top up and pay it now.", CreatedAt: time.Now().UTC(),
			})
		} else {
			paid++
		}
		service.database.WithContext(requestContext).Model(&models.RecurringBill{}).Where("id = ?", schedule.ID).
			Updates(map[string]any{"last_result": result, "last_paid_at": time.Now().UTC()})
	}
	return paid, skipped, nil
}

// nextRunAfter works out the next payment date.
func nextRunAfter(previous time.Time, frequency string) time.Time {
	switch frequency {
	case "weekly":
		return previous.AddDate(0, 0, 7)
	case "biweekly":
		return previous.AddDate(0, 0, 14)
	}
	return previous.AddDate(0, 1, 0)
}

// encodeJSON is a tiny helper for JSON columns.
func encodeJSON(value any) []byte {
	encoded, _ := json.Marshal(value)
	return encoded
}
