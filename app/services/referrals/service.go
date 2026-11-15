// Package referrals pays the people who invite customers.
//
// The rule (set by the owner): when a customer who was invited makes a
// transaction, a share of what Tribe earns on it (the referral share, an
// admin setting, 2% to start) goes to the person who invited them. Nothing is
// paid on transactions of customers nobody invited, and nothing is ever paid
// that Tribe did not earn first.
//
// The share is worked out by ledger.Post from the transaction's own entry
// (ledger.ReferralShare): the fees and margins it credits to Tribe Funds,
// each currency valued in Naira at the market rate. It is paid out of Tribe
// Funds NGN into the inviter's referral balance (ledger.ReferralRewardLegs):
//
//   - transactions that finish at once (swaps, Tribe sends, cards, gift card
//     sales) pay it in the same entry;
//   - transactions a provider must confirm first (bills, gift card
//     purchases, payouts) only RECORD it ("accrued"); PayAccrued pays it in
//     its own entry when the provider confirms, and Cancel drops it if the
//     provider refuses.
//
// There is no referral pot: the money is Tribe's until the moment it is paid.
// Every share is recorded in referral_earnings, one row per transaction.
package referrals

import (
	"context"
	"math/big"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/tribe-africa/tribe-backend/app/apperrors"
	"github.com/tribe-africa/tribe-backend/app/cache"
	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/app/services/ledger"
	"github.com/tribe-africa/tribe-backend/app/services/shared"
	"github.com/tribe-africa/tribe-backend/database"
	"github.com/tribe-africa/tribe-backend/pkg/ids"
	"github.com/tribe-africa/tribe-backend/pkg/money"
)

// Service accrues and pays referral shares.
type Service struct {
	database *gorm.DB
	ledger   *ledger.Service
	config   *cache.ConfigCache
}

// NewService creates the referrals service.
func NewService(connection *gorm.DB, ledgerService *ledger.Service, configCache *cache.ConfigCache) *Service {
	return &Service{database: connection, ledger: ledgerService, config: configCache}
}

// Invitation is what one money request needs to share its earnings with the
// person who invited the customer. A nil *Invitation means "no share": the
// customer was not invited, or the referral share is set to zero.
type Invitation struct {
	InviterUserID string
	FriendUserID  string
	FriendName    string // how the inviter sees their friend, e.g. "Ada O."
	SharePercent  string // the setting in force, saved on the earning
	Share         *ledger.ReferralShare
}

// Prepare looks up whether this customer was invited and, if so, opens the
// inviter's referral balance. Call it BEFORE the money transaction: accounts must never be
// created inside one (see ledger.EnsureUserAccount).
//
// payNow is true for transactions that are final as soon as they are posted,
// and false for those a provider must still confirm (see the package comment).
func (service *Service) Prepare(requestContext context.Context, userID string, payNow bool) (*Invitation, error) {
	var user models.User
	if err := service.database.WithContext(requestContext).Where("id = ?", userID).Take(&user).Error; err != nil {
		return nil, err
	}
	if user.ReferredByUserID == nil || *user.ReferredByUserID == "" || *user.ReferredByUserID == user.ID {
		return nil, nil
	}
	snapshot, err := service.config.Snapshot(requestContext)
	if err != nil {
		return nil, err
	}
	percent, err := money.ParseRate(snapshot.Referral.SharePercent)
	if err != nil || percent.Sign() <= 0 {
		return nil, nil
	}
	inviterAccount, err := service.ledger.EnsureUserAccount(requestContext, *user.ReferredByUserID, ledger.CodeUserReferral, ledger.ReferralAsset)
	if err != nil {
		return nil, err
	}
	return &Invitation{
		InviterUserID: *user.ReferredByUserID,
		FriendUserID:  user.ID,
		FriendName:    shortName(user),
		SharePercent:  snapshot.Referral.SharePercent,
		Share:         &ledger.ReferralShare{Percent: percent, InviterAccountID: inviterAccount.ID, PayNow: payNow},
	}, nil
}

// ShareOf returns the ledger share to put on a posting (nil for no invitation).
func (invitation *Invitation) ShareOf() *ledger.ReferralShare {
	if invitation == nil {
		return nil
	}
	return invitation.Share
}

// Record saves the referral earning a posting just made, inside the same
// database transaction. When the share was paid at once, the inviter also
// gets an inbox message. It does nothing when there was no share.
func (service *Service) Record(databaseTransaction *gorm.DB, invitation *Invitation, transaction *models.Transaction, result *ledger.PostResult) error {
	if invitation == nil || result == nil || result.AlreadyPosted || !result.ReferralShareNgn.IsPositive() {
		return nil
	}
	now := time.Now().UTC()
	transactionID, friendID, percent, entryID := transaction.ID, invitation.FriendUserID, invitation.SharePercent, result.Entry.ID
	earning := models.ReferralEarning{
		ID: ids.New(), UserID: invitation.InviterUserID, FriendUserID: &friendID, TransactionID: &transactionID,
		FriendName: invitation.FriendName, Description: describe(transaction), AmountNgn: result.ReferralShareNgn.UnitsString(),
		Status: models.ReferralAccrued, SharePercent: &percent, AccrualEntryID: &entryID, CreatedAt: now,
	}
	if invitation.Share.PayNow {
		earning.Status, earning.PayoutEntryID, earning.PaidAt = models.ReferralPaid, &entryID, &now
	}
	if err := databaseTransaction.Create(&earning).Error; err != nil {
		return err
	}
	if earning.Status == models.ReferralPaid {
		return notifyPaid(databaseTransaction, earning)
	}
	return nil
}

// PayAccrued pays the share still waiting for one transaction, if there is
// one: Dr Tribe Funds NGN / Cr the inviter's referral balance, in its own
// entry (reference "REFERRAL-<earning id>:PAY", so a retry never pays twice),
// then marks it paid and tells the inviter.
//
// Call it inside the transaction that completes the customer's transaction,
// AFTER locking that transaction's row (the earning row is locked next, then
// ledger.Post locks the accounts: always in that order).
func (service *Service) PayAccrued(databaseTransaction *gorm.DB, transactionID string) error {
	var earning models.ReferralEarning
	err := database.LockForUpdate(databaseTransaction, &earning, "transaction_id = ? AND status = ?", transactionID, models.ReferralAccrued)
	if database.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	amount, err := money.ParseUnits(earning.AmountNgn)
	if err != nil {
		return err
	}
	nairaFunds, err := service.ledger.TribeFunds(databaseTransaction, ledger.ReferralAsset)
	if err != nil {
		return err
	}
	inviterAccount, err := service.ledger.UserAccount(databaseTransaction, earning.UserID, ledger.CodeUserReferral, ledger.ReferralAsset)
	if err != nil {
		return err // opened by Prepare when the share was recorded, so it exists
	}
	result, err := service.ledger.Post(databaseTransaction, ledger.Entry{
		Reference: "REFERRAL-" + earning.ID + ":PAY", TransactionType: "referral_reward",
		Description: "Referral reward: " + earning.FriendName + "'s " + strings.ToLower(earning.Description), BusinessTransactionID: &transactionID,
		Legs: ledger.ReferralRewardLegs(nairaFunds.ID, inviterAccount.ID, amount),
	})
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if err := databaseTransaction.Model(&models.ReferralEarning{}).Where("id = ?", earning.ID).
		Updates(map[string]any{"status": models.ReferralPaid, "payout_entry_id": result.Entry.ID, "paid_at": now}).Error; err != nil {
		return err
	}
	return notifyPaid(databaseTransaction, earning)
}

// Cancel marks the share of a refused transaction as cancelled. Nothing was
// paid yet, so only the record changes.
func (service *Service) Cancel(databaseTransaction *gorm.DB, transactionID string) error {
	return databaseTransaction.Model(&models.ReferralEarning{}).
		Where("transaction_id = ? AND status = ?", transactionID, models.ReferralAccrued).
		Update("status", models.ReferralCancelled).Error
}

// ─── The setting ─────────────────────────────────────────────────────

// maximumSharePercent: the share is a part of what Tribe earned, so it can
// never be more than all of it.
var maximumSharePercent = big.NewRat(100, 1)

// Settings returns the referral share an admin set.
func (service *Service) Settings(requestContext context.Context) (*models.ReferralSettings, error) {
	var settings models.ReferralSettings
	if err := service.database.WithContext(requestContext).Where("id = ?", 1).Take(&settings).Error; err != nil {
		return nil, err
	}
	return &settings, nil
}

// SaveSettings changes the referral share (lock + version, like a fee rule).
// It applies to transactions from now on; earnings already made keep the
// share they were made with. Returns the row before and after, for the audit log.
func (service *Service) SaveSettings(requestContext context.Context, adminName string, sharePercent string, version int) (*models.ReferralSettings, *models.ReferralSettings, error) {
	percent, err := money.ParseRate(sharePercent)
	if err != nil || percent.Sign() < 0 || percent.Cmp(maximumSharePercent) > 0 {
		return nil, nil, apperrors.Validation(map[string][]string{"share_percent": {"The referral share must be between 0 and 100 percent."}})
	}
	var before, after models.ReferralSettings
	err = database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		if err := database.LockForUpdate(databaseTransaction, &before, "id = ?", 1); err != nil {
			return apperrors.NotFound("Referral settings")
		}
		if version != 0 && version != before.Version {
			return apperrors.ErrStaleData
		}
		if err := databaseTransaction.Model(&models.ReferralSettings{}).Where("id = ?", 1).Updates(map[string]any{
			"share_percent": money.FormatRate(percent, 6), "version": before.Version + 1,
			"updated_by": adminName, "updated_at": time.Now().UTC(),
		}).Error; err != nil {
			return err
		}
		return databaseTransaction.Where("id = ?", 1).Take(&after).Error
	})
	if err != nil {
		return nil, nil, err
	}
	service.config.ConfigChanged(requestContext) // every program uses the new share within a second
	return &before, &after, nil
}

// ─── Helpers ─────────────────────────────────────────────────────────

// notifyPaid tells the inviter about a reward in their referral balance.
func notifyPaid(databaseTransaction *gorm.DB, earning models.ReferralEarning) error {
	amount, err := money.ParseUnits(earning.AmountNgn)
	if err != nil {
		return err
	}
	return shared.Notify(databaseTransaction, earning.UserID, "referral", "Referral reward",
		"₦"+amount.Display(2)+" from "+earning.FriendName+"'s "+strings.ToLower(earning.Description)+".", "/me/invite")
}

// shortName is how an inviter sees a friend: first name and last initial.
func shortName(user models.User) string {
	name := strings.TrimSpace(user.FirstName)
	if lastName := []rune(strings.TrimSpace(user.LastName)); len(lastName) > 0 {
		name += " " + strings.ToUpper(string(lastName[0])) + "."
	}
	return name
}

// describe names the KIND of transaction for the inviter, never its details
// (who was paid, how much): the inviter must not learn what their friend does.
func describe(transaction *models.Transaction) string {
	switch transaction.Category {
	case "utility":
		return "Bill payment"
	case "giftcard":
		return "Gift card"
	case "virtual_card":
		return "Card"
	}
	if transaction.Type == "swap" {
		return "Swap"
	}
	return "Transfer"
}
