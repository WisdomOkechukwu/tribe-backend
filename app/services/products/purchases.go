package products

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"time"

	"gorm.io/gorm"

	"github.com/tribe-africa/tribe-backend/app/apperrors"
	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/app/services/ledger"
	"github.com/tribe-africa/tribe-backend/app/services/providers"
	"github.com/tribe-africa/tribe-backend/app/services/shared"
	"github.com/tribe-africa/tribe-backend/database"
	"github.com/tribe-africa/tribe-backend/pkg/ids"
	"github.com/tribe-africa/tribe-backend/pkg/money"
)

// ─── Purchases waiting for a provider (bills and gift cards) ──────────
//
// A bill payment or gift card purchase is saved as a PENDING transaction in
// the same commit that takes the customer's money (see PayBill and
// BuyGiftCard). That pending row is the "outbox": it cannot be lost, because
// it was written together with the money. Then, OUTSIDE any database
// transaction, the provider is asked to complete it:
//
//   - straight away by the API request itself (so a fast provider still
//     gives the customer their token or code on the same screen),
//   - by a queue job a couple of minutes later, as a safety net,
//   - and by the sweeper every 5 minutes for anything still pending
//     (RedrivePendingPurchases), in case Redis lost the job.
//
// All three may run at the same time. That is safe: the provider treats our
// reference as an idempotency key (asking twice never buys twice), and the
// result is recorded under a lock on the transaction row that first checks
// it is STILL pending, so only one of them ever posts.

// purchaseAttemptTimeout is how long the API request waits for the provider
// before answering "pending" and leaving the rest to the queue job.
const purchaseAttemptTimeout = 20 * time.Second

// purchaseSafetyNetDelay is when the queue job looks at the purchase. By
// then the API request has normally finished it, and the job does nothing.
const purchaseSafetyNetDelay = 2 * time.Minute

// finishPurchase queues the safety-net job, then asks the provider once
// itself, and returns the transaction as it is now (completed, failed, or
// still pending if the provider did not answer in time).
//
// refusedMessage is the error returned when the provider refused: the
// customer has already been refunded, but the request still answers with
// an error, as it did before purchases became two steps.
func (service *Service) finishPurchase(requestContext context.Context, transactionID string, refusedMessage string) (*models.Transaction, error) {
	// Never tied to the customer's connection: if the app disconnects, the
	// purchase must still be finished.
	backgroundContext := context.WithoutCancel(requestContext)
	if err := service.dispatcher.ProcessPurchase(backgroundContext, transactionID, "first", purchaseSafetyNetDelay); err != nil {
		slog.Warn("could not queue the purchase job; the sweeper will pick it up", "transaction", transactionID, "error", err)
	}
	attemptContext, cancel := context.WithTimeout(backgroundContext, purchaseAttemptTimeout)
	defer cancel()
	if err := service.ProcessPurchase(attemptContext, transactionID); err != nil {
		slog.Warn("provider did not answer yet; the purchase stays pending", "transaction", transactionID, "error", err)
	}

	var transaction models.Transaction
	if err := service.database.WithContext(backgroundContext).Where("id = ?", transactionID).Take(&transaction).Error; err != nil {
		return nil, err
	}
	if transaction.Status == models.TransactionStatusFailed {
		return nil, apperrors.New(http.StatusUnprocessableEntity, "PROVIDER_REFUSED", refusedMessage)
	}
	return &transaction, nil
}

// purchaseDetails is what a pending purchase keeps in its metadata.
type purchaseDetails struct {
	BillerID  string `json:"biller_id"`
	Target    string `json:"target"`
	AmountNgn string `json:"amount_ngn"` // smallest units (kobo)
	GiftBrand string `json:"gift_brand"`
	Face      string `json:"face"` // dollars, e.g. "50.00"
}

// purchaseOutcome is what the provider said, ready to record.
type purchaseOutcome struct {
	settledAmount     money.Amount // what leaves the vault (to-asset units)
	settledAsset      string       // NGN for bills, USD for gift cards
	providerReference string
	token             string // electricity token (bills)
	giftCode          string // gift card code
}

// ProcessPurchase asks the provider to complete one pending bill payment or
// gift card purchase and records the answer. It is run by the API request,
// the queue job and the sweeper, and is safe to run many times at once.
//
//  1. Not pending any more? Nothing to do.
//  2. Ask the provider OUTSIDE any database transaction, sending our
//     reference as its idempotency key. No row is locked while we wait.
//  3. "Yes": settle. "No" (*providers.RejectedError): refund. Any other
//     error: we do not know what happened, so return it; the queue asks
//     again later. A timeout is never treated as a "no".
func (service *Service) ProcessPurchase(requestContext context.Context, transactionID string) error {
	var transaction models.Transaction
	err := service.database.WithContext(requestContext).Where("id = ?", transactionID).Take(&transaction).Error
	if database.IsNotFound(err) {
		// Jobs are queued only after the commit, so the row always exists
		// for a real purchase; a missing one will never appear. Retrying
		// would only fill the queue.
		slog.Warn("purchase job for a transaction that does not exist; skipped", "transaction", transactionID)
		return nil
	}
	if err != nil {
		return err
	}
	if transaction.Status != models.TransactionStatusPending {
		return nil
	}
	var details purchaseDetails
	if err := json.Unmarshal(transaction.Metadata, &details); err != nil {
		return fmt.Errorf("purchase %s has unreadable metadata: %w", transactionID, err)
	}

	var outcome purchaseOutcome
	var providerError error
	switch transaction.Type {
	case "bill":
		billProvider, err := service.providers.Bills()
		if err != nil {
			return err
		}
		amountNgn, err := money.ParseUnits(details.AmountNgn)
		if err != nil {
			return err
		}
		result, err := billProvider.PayBill(requestContext, transaction.Reference, details.BillerID, details.Target, amountNgn.Decimal(2))
		providerError = err
		outcome = purchaseOutcome{settledAmount: amountNgn, settledAsset: "NGN", providerReference: result.ProviderReference, token: result.Token}
	case "gift_buy":
		giftProvider, err := service.providers.GiftCards()
		if err != nil {
			return err
		}
		faceValue, err := money.ParseDecimal(details.Face, 2)
		if err != nil {
			return err
		}
		code, err := giftProvider.BuyCard(requestContext, transaction.Reference, details.GiftBrand, details.Face)
		providerError = err
		outcome = purchaseOutcome{settledAmount: faceValue, settledAsset: "USD", giftCode: code}
	default:
		return fmt.Errorf("transaction %s (%s) is not a provider purchase", transactionID, transaction.Type)
	}

	if rejected, isRejected := providers.IsRejected(providerError); isRejected {
		return service.refundPurchase(requestContext, transactionID, rejected.Reason)
	}
	if providerError != nil {
		return providerError // unknown: stays pending, asked again later
	}
	return service.settlePurchase(requestContext, transactionID, details, outcome)
}

// settlePurchase records a confirmed purchase:
//
//	lock the transaction row -> still pending? -> post Dr Purchases In
//	Transit / Cr Company Vault -> fill in the token or code -> completed
//	-> inbox message, all in one transaction.
//
// Locking order: the transaction row first, then ledger.Post locks the
// ledger accounts in ascending id order (the same order as ProcessPayout).
func (service *Service) settlePurchase(requestContext context.Context, transactionID string, details purchaseDetails, outcome purchaseOutcome) error {
	var encryptedCode []byte
	if outcome.giftCode != "" {
		var err error
		if encryptedCode, err = service.toolkit.Encrypt(outcome.giftCode); err != nil {
			return err
		}
	}
	return database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		var locked models.Transaction
		if err := database.LockForUpdate(databaseTransaction, &locked, "id = ?", transactionID); err != nil {
			return err
		}
		if locked.Status != models.TransactionStatusPending {
			return nil // another worker finished it while we waited for the provider
		}
		inTransit, err := service.ledger.CompanyAccount(databaseTransaction, ledger.CodePurchaseInTransit, outcome.settledAsset)
		if err != nil {
			return err
		}
		vault, err := service.ledger.CompanyAccount(databaseTransaction, ledger.CodeCompanyVault, outcome.settledAsset)
		if err != nil {
			return err
		}
		if _, err := service.ledger.Post(databaseTransaction, ledger.Entry{
			Reference: locked.Reference + ":SETTLE", TransactionType: locked.Type + "_settled", Description: locked.Title + " confirmed by provider",
			CreatedBy: "system", BusinessTransactionID: &locked.ID,
			Legs: ledger.PurchaseSucceededLegs(inTransit.ID, vault.ID, outcome.settledAmount),
		}); err != nil {
			return err
		}

		now := time.Now().UTC()
		metadata := map[string]any{}
		_ = json.Unmarshal(locked.Metadata, &metadata)
		title, body := locked.Title, ""
		switch locked.Type {
		case "bill":
			metadata["provider_reference"] = outcome.providerReference
			paymentUpdates := map[string]any{"status": "completed", "provider_reference": outcome.providerReference}
			if outcome.token != "" {
				kwh := new(big.Rat).Quo(outcome.settledAmount.ToRational(2), nairaPerKwh).FloatString(1)
				metadata["token"], metadata["units"] = outcome.token, kwh+" kWh"
				paymentUpdates["token"] = outcome.token
				var payment models.BillPayment
				if err := databaseTransaction.Where("transaction_id = ?", locked.ID).Take(&payment).Error; err != nil {
					return err
				}
				if err := databaseTransaction.Create(&models.VaultToken{
					ID: ids.New(), UserID: locked.UserID, BillPaymentID: payment.ID, Provider: locked.Title, Meter: details.Target,
					AmountNgn: outcome.settledAmount.UnitsString(), Kwh: kwh, Token: outcome.token, PaidWith: locked.FromAsset, BoughtAt: now,
				}).Error; err != nil {
					return err
				}
			}
			if err := databaseTransaction.Model(&models.BillPayment{}).Where("transaction_id = ?", locked.ID).Updates(paymentUpdates).Error; err != nil {
				return err
			}
			title, body = locked.Title+" paid", "₦"+outcome.settledAmount.Decimal(2)+" for "+details.Target
		case "gift_buy":
			if err := databaseTransaction.Model(&models.GiftCardOrder{}).Where("transaction_id = ?", locked.ID).
				Updates(map[string]any{"status": "completed", "code_encrypted": encryptedCode, "updated_at": now}).Error; err != nil {
				return err
			}
			title, body = "Gift card ready", locked.Title+" ($"+details.Face+"). Open the transaction to see the code."
		}
		if err := databaseTransaction.Model(&models.Transaction{}).Where("id = ?", locked.ID).Updates(map[string]any{
			"status": models.TransactionStatusCompleted, "metadata": shared.JSON(metadata),
			"completed_at": now, "updated_at": now, "version": gorm.Expr("version + 1"),
		}).Error; err != nil {
			return err
		}
		return shared.Notify(databaseTransaction, locked.UserID, "money", title, body, "/transaction/"+locked.ID)
	})
}

// refundPurchase records a purchase the provider REFUSED: the original
// posting is reversed (the customer gets back exactly what they paid, fee
// included), the purchase is marked failed, and the customer is told.
func (service *Service) refundPurchase(requestContext context.Context, transactionID string, reason string) error {
	return database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		var locked models.Transaction
		if err := database.LockForUpdate(databaseTransaction, &locked, "id = ?", transactionID); err != nil {
			return err
		}
		if locked.Status != models.TransactionStatusPending {
			return nil
		}
		if locked.JournalEntryID == nil {
			return fmt.Errorf("purchase %s has no journal entry to reverse", locked.ID)
		}
		if _, err := service.ledger.Reverse(databaseTransaction, *locked.JournalEntryID, "provider refused: "+reason, "system"); err != nil {
			return err
		}
		now := time.Now().UTC()
		switch locked.Type {
		case "bill":
			if err := databaseTransaction.Model(&models.BillPayment{}).Where("transaction_id = ?", locked.ID).Update("status", "failed").Error; err != nil {
				return err
			}
		case "gift_buy":
			if err := databaseTransaction.Model(&models.GiftCardOrder{}).Where("transaction_id = ?", locked.ID).
				Updates(map[string]any{"status": "failed", "updated_at": now}).Error; err != nil {
				return err
			}
		}
		if err := databaseTransaction.Model(&models.Transaction{}).Where("id = ?", locked.ID).Updates(map[string]any{
			"status": models.TransactionStatusFailed, "failure_reason": reason, "updated_at": now, "version": gorm.Expr("version + 1"),
		}).Error; err != nil {
			return err
		}
		return shared.Notify(databaseTransaction, locked.UserID, "money", locked.Title+" failed",
			"It could not be completed, so we returned your money. "+reason, "/transaction/"+locked.ID)
	})
}

// RevealGiftCardCode returns the code of a gift card the user bought,
// after checking their PIN (the code is money, like a card number).
// Used when the purchase finished in the background, after the buy screen.
func (service *Service) RevealGiftCardCode(requestContext context.Context, userID string, transactionID string, pin string) (string, error) {
	if err := service.auth.VerifyPin(requestContext, userID, pin); err != nil {
		return "", err
	}
	return service.giftCardCode(requestContext, userID, transactionID)
}

// giftCardCode decrypts the code of one of the user's completed gift cards.
func (service *Service) giftCardCode(requestContext context.Context, userID string, transactionID string) (string, error) {
	var order models.GiftCardOrder
	err := service.database.WithContext(requestContext).
		Where("transaction_id = ? AND user_id = ? AND direction = ?", transactionID, userID, "buy").Take(&order).Error
	if err != nil {
		return "", apperrors.NotFound("Gift card")
	}
	if order.Status != "completed" || len(order.CodeEncrypted) == 0 {
		return "", apperrors.New(http.StatusConflict, "GIFT_CARD_PENDING", "Your gift card is still on its way. We'll let you know when it's ready.")
	}
	return service.toolkit.Decrypt(order.CodeEncrypted)
}

// pendingRedriveAfter is how old a pending purchase must be before the
// sweeper re-queues it (the API and the first job had their chance).
const pendingRedriveAfter = 3 * time.Minute

// pendingRedriveFor is how long the sweeper keeps trying. After a day a
// purchase that is still pending needs a person to look at it with the
// provider (it shows on the admin dashboard's "pending" count).
const pendingRedriveFor = 24 * time.Hour

// RedrivePendingPurchases re-queues every bill payment, gift card purchase
// and payout that has been pending for a while, in case its queue job was
// lost (Redis restarted, the job ran out of retries, the API crashed right
// after the commit). Run by the scheduler every 5 minutes; attempt makes
// the queue ids unique for this run.
func (service *Service) RedrivePendingPurchases(requestContext context.Context, attempt string) (string, error) {
	now := time.Now().UTC()
	var pending []models.Transaction
	err := service.database.WithContext(requestContext).
		Select("id", "type").
		Where("status = ? AND type IN ? AND created_at < ? AND created_at > ?",
			models.TransactionStatusPending, []string{"bill", "gift_buy", "send"}, now.Add(-pendingRedriveAfter), now.Add(-pendingRedriveFor)).
		Order("created_at ASC").Limit(500).
		Find(&pending).Error
	if err != nil {
		return "", err
	}
	queued := 0
	for _, transaction := range pending {
		var queueError error
		if transaction.Type == "send" {
			queueError = service.dispatcher.RedriveProcessPayout(requestContext, transaction.ID, attempt)
		} else {
			queueError = service.dispatcher.ProcessPurchase(requestContext, transaction.ID, attempt, 0)
		}
		if queueError != nil {
			return fmt.Sprintf("re-queued %d of %d", queued, len(pending)), queueError
		}
		queued++
	}
	return fmt.Sprintf("re-queued %d pending purchases and payouts", queued), nil
}
