package wallets

import (
	"context"
	"net/http"
	"time"

	"gorm.io/gorm"

	"github.com/tribe-africa/tribe-backend/app/apperrors"
	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/app/services/ledger"
	"github.com/tribe-africa/tribe-backend/app/services/shared"
	"github.com/tribe-africa/tribe-backend/database"
	"github.com/tribe-africa/tribe-backend/pkg/ids"
	"github.com/tribe-africa/tribe-backend/pkg/money"
)

// customerAdjustmentsTemplate is the expense account manual credits and
// debits are booked to. It is created on first use, so databases seeded
// before it existed work.
var customerAdjustmentsTemplate = ledger.CompanyAccountTemplate{Code: ledger.CodeCustomerAdjustments, Name: "Customer Adjustments", Category: ledger.CategoryExpense}

// RequestCustomerAdjustment records that an admin wants to credit a customer
// from the company's free funds, or debit money back out of their wallet (the
// admin "Credit" / "Debit" buttons on a customer). Nothing is posted yet: it
// waits as "pending" until a DIFFERENT admin approves it (see
// ApproveCustomerAdjustment). The limit is checked here too, so an impossible
// request is refused straight away; approval checks again, because the
// balances may have changed in between.
func (service *Service) RequestCustomerAdjustment(requestContext context.Context, adminID string, adminName string, userID string, kind string, assetCode string, amountText string, reason string) (*models.CustomerAdjustment, error) {
	query := service.database.WithContext(requestContext)
	var user models.User
	if err := query.Where("id = ?", userID).Take(&user).Error; err != nil {
		return nil, apperrors.NotFound("Customer")
	}
	if user.Status == "closed" {
		return nil, apperrors.New(http.StatusConflict, "ACCOUNT_CLOSED", "This account is closed and cannot be adjusted.")
	}
	asset, err := shared.LoadAsset(query, assetCode)
	if err != nil {
		return nil, err
	}
	amount, err := money.ParseDecimal(amountText, asset.DecimalPlaces)
	if err != nil || !amount.IsPositive() {
		return nil, apperrors.Validation(map[string][]string{"amount": {"Enter a valid amount."}})
	}
	if err := service.checkAdjustmentLimit(query, kind, user.ID, asset, amount); err != nil {
		return nil, err
	}

	adjustment := &models.CustomerAdjustment{
		ID: ids.New(), UserID: user.ID, Kind: kind, AssetCode: asset.Code, Amount: amount.UnitsString(), Reason: reason,
		Status: models.FundingPending, RequestedBy: adminID, RequestedByName: adminName, CreatedAt: time.Now().UTC(),
	}
	if err := query.Create(adjustment).Error; err != nil {
		return nil, err
	}
	return adjustment, nil
}

// ApproveCustomerAdjustment is the second admin saying "yes, do it". Only a
// super admin may approve (approverIsSuperAdmin, from adminauth.IsSuperAdmin).
// In ONE database transaction it:
//
//  1. locks the request and checks it is still pending and raised by someone else;
//  2. locks this currency's Customer Adjustments account. Only adjustments
//     touch that account, so two approvals in the same currency queue up here
//     and the second one sees the first one's effect on the limit;
//  3. re-checks the limit (credit: the vault's free funds; debit: the wallet);
//  4. posts the legs, records the transaction the customer sees in History,
//     and writes their inbox message.
func (service *Service) ApproveCustomerAdjustment(requestContext context.Context, adminID string, adminName string, approverIsSuperAdmin bool, adjustmentID string) (*models.CustomerAdjustment, error) {
	if !approverIsSuperAdmin {
		return nil, apperrors.New(http.StatusForbidden, "SUPER_ADMIN_REQUIRED", "Only a super admin can approve manual credits and debits.")
	}
	// Accounts are created outside the transaction (ensureAccount uses its own
	// connection), so read the request once without a lock to know which ones.
	var preview models.CustomerAdjustment
	if err := service.database.WithContext(requestContext).Where("id = ?", adjustmentID).Take(&preview).Error; err != nil {
		return nil, apperrors.NotFound("Adjustment request")
	}
	asset, err := shared.LoadAsset(service.database.WithContext(requestContext), preview.AssetCode)
	if err != nil {
		return nil, err
	}
	expense, err := service.ledger.EnsureCompanyAccount(requestContext, customerAdjustmentsTemplate, asset.Code)
	if err != nil {
		return nil, err
	}
	userWallet, err := service.ledger.EnsureUserAccount(requestContext, preview.UserID, ledger.CodeUserPrimary, asset.Code)
	if err != nil {
		return nil, err
	}

	var adjustment models.CustomerAdjustment
	err = database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		if err := lockPendingAdjustment(databaseTransaction, &adjustment, adjustmentID, adminID); err != nil {
			return err
		}
		var lockedExpense models.LedgerAccount
		if err := database.LockForUpdate(databaseTransaction, &lockedExpense, "id = ?", expense.ID); err != nil {
			return err
		}
		amount, err := money.ParseUnits(adjustment.Amount)
		if err != nil {
			return err
		}
		if err := service.checkAdjustmentLimit(databaseTransaction, adjustment.Kind, adjustment.UserID, asset, amount); err != nil {
			return err
		}

		transaction := &models.Transaction{
			ID: ids.New(), Reference: "ADJ-" + adjustment.ID, UserID: adjustment.UserID,
			Category: "currency", Status: models.TransactionStatusCompleted, Subtitle: asset.Name,
			FromAsset: asset.Code, FromAmount: amount.UnitsString(),
			UsdValue: service.rates.UsdValueOf(databaseTransaction, asset.Code, amount, asset.DecimalPlaces),
		}
		legs := ledger.CustomerCreditLegs(expense.ID, userWallet.ID, amount)
		inboxBody := amount.Decimal(asset.DecimalPlaces) + " " + asset.Code + " from Tribe was added to your wallet."
		if adjustment.Kind == models.AdjustmentDebit {
			// ledger.Post refuses to take a user wallet below zero (INSUFFICIENT_FUNDS).
			legs = ledger.CustomerDebitLegs(userWallet.ID, expense.ID, amount)
			transaction.Type, transaction.Direction, transaction.Title = "adjustment", "out", "Adjustment by Tribe"
			inboxBody = amount.Decimal(asset.DecimalPlaces) + " " + asset.Code + " was taken from your wallet by Tribe: " + adjustment.Reason
		} else {
			transaction.Type, transaction.Direction, transaction.Title = "deposit", "in", "Credit from Tribe"
		}
		result, err := service.ledger.Post(databaseTransaction, ledger.Entry{
			Reference: "ADJ-" + adjustment.ID + ":POST", TransactionType: "customer_" + adjustment.Kind,
			Description: "Manual customer " + adjustment.Kind + ": " + adjustment.Reason, CreatedBy: "admin:" + adminID,
			BusinessTransactionID: &transaction.ID, Legs: legs,
		})
		if err != nil {
			return err
		}
		transaction.JournalEntryID = &result.Entry.ID
		if err := shared.RecordTransaction(databaseTransaction, transaction); err != nil {
			return err
		}
		inboxTitle := "Money received"
		if adjustment.Kind == models.AdjustmentDebit {
			inboxTitle = "Wallet adjusted"
		}
		if err := shared.Notify(databaseTransaction, adjustment.UserID, "money", inboxTitle, inboxBody, "/transaction/"+transaction.ID); err != nil {
			return err
		}

		now := time.Now().UTC()
		adjustment.Status = models.FundingApproved
		adjustment.TransactionID = transaction.ID
		adjustment.JournalEntryID = result.Entry.ID
		adjustment.ApprovedBy = &adminID
		adjustment.ApprovedByName = adminName
		adjustment.DecidedAt = &now
		return databaseTransaction.Model(&models.CustomerAdjustment{}).Where("id = ?", adjustment.ID).Updates(map[string]any{
			"status": adjustment.Status, "transaction_id": adjustment.TransactionID, "journal_entry_id": adjustment.JournalEntryID,
			"approved_by": adminID, "approved_by_name": adminName, "decided_at": now,
		}).Error
	})
	if err != nil {
		return nil, err
	}
	return &adjustment, nil
}

// RejectCustomerAdjustment closes a pending adjustment without posting
// anything. The same four-eyes rule applies: the requester cannot reject their own.
func (service *Service) RejectCustomerAdjustment(requestContext context.Context, adminID string, adminName string, adjustmentID string, reason string) (*models.CustomerAdjustment, error) {
	var adjustment models.CustomerAdjustment
	err := database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		if err := lockPendingAdjustment(databaseTransaction, &adjustment, adjustmentID, adminID); err != nil {
			return err
		}
		now := time.Now().UTC()
		adjustment.Status = models.FundingRejected
		adjustment.ApprovedBy = &adminID
		adjustment.ApprovedByName = adminName
		adjustment.DecidedAt = &now
		adjustment.RejectionReason = reason
		return databaseTransaction.Model(&models.CustomerAdjustment{}).Where("id = ?", adjustment.ID).Updates(map[string]any{
			"status": adjustment.Status, "approved_by": adminID, "approved_by_name": adminName,
			"decided_at": now, "rejection_reason": reason,
		}).Error
	})
	if err != nil {
		return nil, err
	}
	return &adjustment, nil
}

// lockPendingAdjustment locks an adjustment request and checks that the
// deciding admin may decide it: it must still be pending and raised by someone else.
func lockPendingAdjustment(databaseTransaction *gorm.DB, adjustment *models.CustomerAdjustment, adjustmentID string, deciderID string) error {
	if err := database.LockForUpdate(databaseTransaction, adjustment, "id = ?", adjustmentID); err != nil {
		return apperrors.NotFound("Adjustment request")
	}
	if adjustment.Status != models.FundingPending {
		return apperrors.New(http.StatusConflict, "ALREADY_DECIDED", "This adjustment was already "+adjustment.Status+".")
	}
	if adjustment.RequestedBy == deciderID {
		return apperrors.New(http.StatusForbidden, "FOUR_EYES_REQUIRED", "A different admin must approve or reject your own adjustment.")
	}
	return nil
}

// checkAdjustmentLimit refuses a credit bigger than the currency's free funds
// (INSUFFICIENT_FREE_FUNDS), or a debit bigger than the customer's wallet
// balance (INSUFFICIENT_FUNDS). query is the transaction when approving.
func (service *Service) checkAdjustmentLimit(query *gorm.DB, kind string, userID string, asset *models.Asset, amount money.Amount) error {
	if kind == models.AdjustmentDebit {
		walletBalance := money.Zero()
		if wallet, err := service.ledger.UserAccount(query, userID, ledger.CodeUserPrimary, asset.Code); err == nil {
			if walletBalance, err = service.ledger.BalanceInQuery(query, *wallet); err != nil {
				return err
			}
		} else if !database.IsNotFound(err) {
			return err
		}
		if amount.Compare(walletBalance) > 0 {
			return apperrors.New(http.StatusUnprocessableEntity, "INSUFFICIENT_FUNDS",
				"The customer only has "+walletBalance.Decimal(asset.DecimalPlaces)+" "+asset.Code+" in this wallet.")
		}
		return nil
	}

	free, err := service.ledger.FreeFundsOf(query, asset.Code)
	if err != nil {
		return err
	}
	if amount.Compare(free.Free) > 0 {
		available := free.Free
		if available.IsNegative() {
			available = money.Zero()
		}
		return apperrors.New(http.StatusUnprocessableEntity, "INSUFFICIENT_FREE_FUNDS",
			"Only "+available.Decimal(asset.DecimalPlaces)+" "+asset.Code+" is free in the vault. Fund the vault first.")
	}
	return nil
}
