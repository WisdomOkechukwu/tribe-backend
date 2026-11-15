package products

import (
	"context"
	"math/big"
	"net/http"
	"regexp"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/tribe-africa/tribe-backend/app/apperrors"
	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/app/services/ledger"
	"github.com/tribe-africa/tribe-backend/app/services/rates"
	"github.com/tribe-africa/tribe-backend/app/services/shared"
	"github.com/tribe-africa/tribe-backend/app/services/swaps"
	"github.com/tribe-africa/tribe-backend/database"
	"github.com/tribe-africa/tribe-backend/pkg/ids"
	"github.com/tribe-africa/tribe-backend/pkg/money"
)

// ─── Gift cards ──────────────────────────────────────────────────────

// ListGiftCardBrands returns the brands we trade.
func (service *Service) ListGiftCardBrands(requestContext context.Context) ([]models.GiftCardBrand, error) {
	var brands []models.GiftCardBrand
	err := service.database.WithContext(requestContext).Where("enabled = ?", true).Order("sort_order").Find(&brands).Error
	return brands, err
}

// GiftCardInput is what the gift card screens send.
type GiftCardInput struct {
	BrandID   string
	FaceUsd   string // card value in dollars, e.g. "50"
	Asset     string // pay with (buy) or get paid in (sell)
	Code      string // sell only
	Pin       string
	Reference string
}

// BuyGiftCard buys a card with any asset, in the same two steps as PayBill:
// the money is converted and paid out of the gift card provider's dollar
// vault, and the transaction saved as pending, in one commit; the provider
// is then asked OUTSIDE any transaction (see ProcessPurchase). On a refusal
// the posting is reversed.
//
// The code is returned when the provider answered straight away; otherwise
// it is empty and the customer reveals it later (RevealGiftCardCode).
func (service *Service) BuyGiftCard(requestContext context.Context, actor shared.Actor, input GiftCardInput) (*models.Transaction, string, error) {
	if err := service.auth.VerifyPin(requestContext, actor.UserID, input.Pin); err != nil {
		return nil, "", err
	}
	brand, faceValue, err := service.loadBrandAndFace(requestContext, input)
	if err != nil {
		return nil, "", err
	}
	converted, fee, price, err := service.conversionInto(requestContext, "gift_buy", input.Asset, "USD", faceValue, 2)
	if err != nil {
		return nil, "", err
	}
	sourceWallet, err := service.ledger.EnsureUserAccount(requestContext, actor.UserID, ledger.CodeUserPrimary, input.Asset)
	if err != nil {
		return nil, "", err
	}
	// Fail early (before taking money) if no gift card provider is configured.
	if _, err := service.providers.GiftCards(); err != nil {
		return nil, "", err
	}
	// The provider may still refuse, so an inviter's share is only recorded
	// now and paid when the card is confirmed (settlePurchase).
	invitation, err := service.referrals.Prepare(requestContext, actor.UserID, false)
	if err != nil {
		return nil, "", err
	}

	transaction := &models.Transaction{ID: ids.New(), Reference: input.Reference, UserID: actor.UserID}
	err = database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		if _, err := shared.LockActor(databaseTransaction, actor); err != nil {
			return err
		}
		giftVault, err := service.productVault(databaseTransaction, brand.ProviderID, models.ProviderCategoryGiftCard, "USD")
		if err != nil {
			return err
		}
		posting := ledger.ConversionPosting{
			Reference: input.Reference + ":POST", TransactionType: "gift_buy", Description: brand.Name + " gift card",
			CreatedBy: "user:" + actor.UserID, BusinessTransactionID: &transaction.ID,
			SourceAccountID: sourceWallet.ID, TargetAccountID: giftVault.ID,
			FromAsset: input.Asset, ToAsset: "USD", Converted: converted, Fee: fee, Received: faceValue,
			Rates: service.rates.RatesFor(databaseTransaction, input.Asset, "USD", "NGN"), ReferralShare: invitation.ShareOf(),
		}
		if price != nil {
			posting.NairaIn, posting.NairaOut, posting.MarketReceived = price.NairaIn, price.NairaOut, price.MarketReceived
		}
		postResult, err := service.ledger.PostConversion(databaseTransaction, posting)
		if err != nil {
			return err
		}
		usd, faceText, feeAsset := "USD", faceValue.UnitsString(), input.Asset
		*transaction = models.Transaction{
			ID: transaction.ID, Reference: input.Reference, UserID: actor.UserID,
			Category: "giftcard", Type: "gift_buy", Direction: "out", Status: models.TransactionStatusPending,
			Title: brand.Name + " gift card", Subtitle: "$" + faceValue.Decimal(2),
			FromAsset: input.Asset, FromAmount: converted.Add(fee).UnitsString(), ToAsset: &usd, ToAmount: &faceText,
			FeeAsset: &feeAsset, FeeAmount: fee.UnitsString(), JournalEntryID: &postResult.Entry.ID, UsdValue: faceValue.Decimal(2),
			Metadata: shared.JSON(map[string]any{"gift_brand": brand.ID, "face": faceValue.Decimal(2)}),
		}
		if err := shared.RecordTransaction(databaseTransaction, transaction); err != nil {
			return err
		}
		if err := service.referrals.Record(databaseTransaction, invitation, transaction, postResult); err != nil {
			return err
		}
		if price != nil {
			if err := swaps.SaveSwapLegs(databaseTransaction, transaction.ID, nil, price.Route); err != nil {
				return err
			}
		}
		// The code arrives when the provider answers (see ProcessPurchase),
		// and so does the "Gift card ready" inbox message.
		now := time.Now().UTC()
		return databaseTransaction.Create(&models.GiftCardOrder{
			ID: ids.New(), UserID: actor.UserID, TransactionID: transaction.ID, BrandID: brand.ID, Direction: "buy",
			FaceValueUsd: faceText, Status: "pending", CreatedAt: now, UpdatedAt: now,
		}).Error
	})
	if err != nil {
		return nil, "", err
	}
	// Step 2, after the commit: ask the provider (see finishPurchase).
	finished, err := service.finishPurchase(requestContext, transaction.ID, "The gift card provider could not complete this purchase, so your money was returned.")
	if err != nil {
		return nil, "", err
	}
	code := ""
	if finished.Status == models.TransactionStatusCompleted {
		code, err = service.giftCardCode(requestContext, actor.UserID, finished.ID)
		if err != nil {
			return nil, "", err
		}
	}
	return finished, code, nil
}

// SellGiftCard buys a user's gift card for Naira (at the brand's sell rate),
// optionally converted to another asset.
func (service *Service) SellGiftCard(requestContext context.Context, actor shared.Actor, input GiftCardInput) (*models.Transaction, error) {
	if err := service.auth.VerifyPin(requestContext, actor.UserID, input.Pin); err != nil {
		return nil, err
	}
	if len(input.Code) < 6 {
		return nil, apperrors.Validation(map[string][]string{"code": {"Enter the card code."}})
	}
	brand, faceValue, err := service.loadBrandAndFace(requestContext, input)
	if err != nil {
		return nil, err
	}
	giftProvider, err := service.providers.GiftCards()
	if err != nil {
		return nil, err
	}
	valid, err := giftProvider.CheckSoldCard(requestContext, input.Reference, brand.ID, input.Code)
	if err != nil || !valid {
		return nil, apperrors.New(http.StatusUnprocessableEntity, "CARD_REJECTED", "This gift card code was not accepted.")
	}

	// Naira paid = face value × the brand's sell rate.
	sellRate, err := money.ParseRate(brand.SellRateNgn)
	if err != nil {
		return nil, err
	}
	nairaPayout := money.FromRational(new(big.Rat).Mul(faceValue.ToRational(2), sellRate), 2, money.RoundDown)
	payoutAsset, received := "NGN", nairaPayout
	var price *rates.Price
	if input.Asset != "" && input.Asset != "NGN" {
		price, err = service.rates.PriceConversion(requestContext, rates.PriceInput{TransactionType: "gift_sell", FromAsset: "NGN", ToAsset: input.Asset, PayAmount: nairaPayout.Decimal(2)})
		if err != nil {
			return nil, err
		}
		payoutAsset, received = input.Asset, price.Received
	}
	userWallet, err := service.ledger.EnsureUserAccount(requestContext, actor.UserID, ledger.CodeUserPrimary, payoutAsset)
	if err != nil {
		return nil, err
	}
	encrypted, err := service.toolkit.Encrypt(input.Code)
	if err != nil {
		return nil, err
	}
	// A sold card is paid out at once, so an inviter's share is paid at once.
	invitation, err := service.referrals.Prepare(requestContext, actor.UserID, true)
	if err != nil {
		return nil, err
	}
	var marketReceived *money.Amount
	if price != nil {
		marketReceived = price.MarketReceived // the margin of the Naira -> payout asset step
	}

	transaction := &models.Transaction{ID: ids.New(), Reference: input.Reference, UserID: actor.UserID}
	err = database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		if _, err := shared.LockActor(databaseTransaction, actor); err != nil {
			return err
		}
		giftVault, err := service.productVault(databaseTransaction, brand.ProviderID, models.ProviderCategoryGiftCard, "USD")
		if err != nil {
			return err
		}
		// The gift card provider credits us the card's dollar value (Dr its
		// vault USD), the dollars are sold for Naira at the home provider, and
		// the user is paid. The Naira legs record the market value vs what we paid.
		usdRate := service.rates.RatesFor(databaseTransaction, "USD")["USD"][1]
		marketNaira := nairaPayout
		if rate, err := money.ParseRate(usdRate); err == nil {
			marketNaira = money.FromRational(new(big.Rat).Mul(faceValue.ToRational(2), rate), 2, money.RoundHalfUp)
		}
		postResult, err := service.ledger.PostConversion(databaseTransaction, ledger.ConversionPosting{
			Reference: input.Reference + ":POST", TransactionType: "gift_sell", Description: "Sold " + brand.Name + " gift card",
			CreatedBy: "user:" + actor.UserID, BusinessTransactionID: &transaction.ID,
			SourceAccountID: giftVault.ID, TargetAccountID: userWallet.ID,
			FromAsset: "USD", ToAsset: payoutAsset, Converted: faceValue, Fee: money.Zero(), Received: received,
			NairaIn: &marketNaira, NairaOut: &nairaPayout, MarketReceived: marketReceived,
			Rates: service.rates.RatesFor(databaseTransaction, "USD", payoutAsset, "NGN"), ReferralShare: invitation.ShareOf(),
		})
		if err != nil {
			return err
		}
		payoutCopy, receivedText := payoutAsset, received.UnitsString()
		*transaction = models.Transaction{
			ID: transaction.ID, Reference: input.Reference, UserID: actor.UserID,
			Category: "giftcard", Type: "gift_sell", Direction: "in", Status: models.TransactionStatusCompleted,
			Title: "Sold " + brand.Name + " gift card", Subtitle: "$" + faceValue.Decimal(2),
			FromAsset: "USD", FromAmount: faceValue.UnitsString(), ToAsset: &payoutCopy, ToAmount: &receivedText,
			JournalEntryID: &postResult.Entry.ID, UsdValue: faceValue.Decimal(2),
			Metadata: shared.JSON(map[string]any{"gift_brand": brand.ID, "face": faceValue.Decimal(2)}),
		}
		if err := shared.RecordTransaction(databaseTransaction, transaction); err != nil {
			return err
		}
		if err := service.referrals.Record(databaseTransaction, invitation, transaction, postResult); err != nil {
			return err
		}
		if err := shared.NotifyTransaction(databaseTransaction, transaction, "money", "Gift card sold"); err != nil {
			return err
		}
		now := time.Now().UTC()
		return databaseTransaction.Create(&models.GiftCardOrder{
			ID: ids.New(), UserID: actor.UserID, TransactionID: transaction.ID, BrandID: brand.ID, Direction: "sell",
			FaceValueUsd: faceValue.UnitsString(), CodeEncrypted: encrypted, Status: "completed", CreatedAt: now, UpdatedAt: now,
		}).Error
	})
	if err != nil {
		return nil, err
	}
	return transaction, nil
}

// loadBrandAndFace validates the brand and the face value.
func (service *Service) loadBrandAndFace(requestContext context.Context, input GiftCardInput) (*models.GiftCardBrand, money.Amount, error) {
	var brand models.GiftCardBrand
	if err := service.database.WithContext(requestContext).Where("id = ? AND enabled = ?", input.BrandID, true).Take(&brand).Error; err != nil {
		return nil, money.Zero(), apperrors.NotFound("Gift card brand")
	}
	faceValue, err := money.ParseDecimal(input.FaceUsd, 2)
	minimum, maximum := money.FromUnits(int64(brand.MinimumUsd)*100), money.FromUnits(int64(brand.MaximumUsd)*100)
	if err != nil || faceValue.Compare(minimum) < 0 || faceValue.Compare(maximum) > 0 {
		return nil, money.Zero(), apperrors.Validation(map[string][]string{"face_usd": {"Choose a value the brand supports."}})
	}
	return &brand, faceValue, nil
}

// ─── Cashpoints ──────────────────────────────────────────────────────

var twelveDigits = regexp.MustCompile(`^[0-9]{12}$`)

// CreateCashpoint moves Naira from the user's wallet into their cashpoint
// balance (still money we owe them, set aside for the code) and returns a
// 12-digit code. The money stays at the provider the whole time.
func (service *Service) CreateCashpoint(requestContext context.Context, actor shared.Actor, amountNgn string, pin string, reference string) (*models.Cashpoint, string, error) {
	if err := service.auth.VerifyPin(requestContext, actor.UserID, pin); err != nil {
		return nil, "", err
	}
	amount, err := money.ParseDecimal(amountNgn, 2)
	if err != nil || !amount.IsPositive() {
		return nil, "", apperrors.Validation(map[string][]string{"amount_ngn": {"Enter a valid amount."}})
	}
	userWallet, err := service.ledger.EnsureUserAccount(requestContext, actor.UserID, ledger.CodeUserPrimary, "NGN")
	if err != nil {
		return nil, "", err
	}
	cashpointHold, err := service.ledger.EnsureUserAccount(requestContext, actor.UserID, ledger.CodeUserCashpoint, "NGN")
	if err != nil {
		return nil, "", err
	}
	code := ids.RandomDigits(12)
	encrypted, err := service.toolkit.Encrypt(code)
	if err != nil {
		return nil, "", err
	}
	now := time.Now().UTC()
	cashpoint := &models.Cashpoint{
		ID: ids.New(), CreatorUserID: actor.UserID, CodeHash: service.toolkit.HashCode(code), CodeEncrypted: encrypted,
		Amount: amount.UnitsString(), Status: "active", Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	transaction := &models.Transaction{ID: ids.New(), Reference: reference, UserID: actor.UserID}
	err = database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		if _, err := shared.LockActor(databaseTransaction, actor); err != nil {
			return err
		}
		postResult, err := service.ledger.Post(databaseTransaction, ledger.Entry{
			Reference: reference + ":POST", TransactionType: "cashpoint_create", Description: "Cashpoint created",
			CreatedBy: "user:" + actor.UserID, BusinessTransactionID: &transaction.ID,
			Legs: ledger.MoveLegs(userWallet.ID, cashpointHold.ID, amount, "Cashpoint code"),
		})
		if err != nil {
			return err
		}
		*transaction = models.Transaction{
			ID: transaction.ID, Reference: reference, UserID: actor.UserID,
			Category: "cashpoint", Type: "cashpoint_create", Direction: "out", Status: models.TransactionStatusPending,
			Title: "Cashpoint code", Subtitle: "Waiting to be redeemed", FromAsset: "NGN", FromAmount: amount.UnitsString(),
			JournalEntryID: &postResult.Entry.ID, Metadata: shared.JSON(map[string]any{"cashpoint_id": cashpoint.ID}),
		}
		if err := shared.RecordTransaction(databaseTransaction, transaction); err != nil {
			return err
		}
		if err := shared.NotifyTransaction(databaseTransaction, transaction, "cashpoint", "Cashpoint created"); err != nil {
			return err
		}
		cashpoint.TransactionID = transaction.ID
		return databaseTransaction.Create(cashpoint).Error
	})
	if err != nil {
		return nil, "", err
	}
	return cashpoint, code, nil
}

// RedeemCashpoint pays a code's Naira to the person redeeming it.
// The cashpoint row is LOCKED, so if two people type the same code at the
// same moment, only the first one gets the money.
func (service *Service) RedeemCashpoint(requestContext context.Context, actor shared.Actor, code string, reference string) (*models.Transaction, error) {
	if !twelveDigits.MatchString(code) {
		return nil, apperrors.Validation(map[string][]string{"code": {"Enter the 12-digit code."}})
	}
	userWallet, err := service.ledger.EnsureUserAccount(requestContext, actor.UserID, ledger.CodeUserPrimary, "NGN")
	if err != nil {
		return nil, err
	}
	transaction := &models.Transaction{ID: ids.New(), Reference: reference, UserID: actor.UserID}
	err = database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		if _, err := shared.LockActor(databaseTransaction, actor); err != nil {
			return err
		}
		var cashpoint models.Cashpoint
		if err := database.LockForUpdate(databaseTransaction, &cashpoint, "code_hash = ?", service.toolkit.HashCode(code)); err != nil {
			return apperrors.New(http.StatusUnprocessableEntity, "CODE_INVALID", "That code is not valid.")
		}
		if cashpoint.Status != "active" {
			return apperrors.New(http.StatusUnprocessableEntity, "CODE_USED", "That code was already used or cancelled.")
		}
		if cashpoint.CreatorUserID == actor.UserID {
			return apperrors.New(http.StatusUnprocessableEntity, "OWN_CODE", "You can't redeem your own code. Cancel it instead.")
		}
		amount, _ := money.ParseUnits(cashpoint.Amount)
		creatorHold, err := service.ledger.UserAccount(databaseTransaction, cashpoint.CreatorUserID, ledger.CodeUserCashpoint, "NGN")
		if err != nil {
			return err // opened by CreateCashpoint, so it exists
		}
		postResult, err := service.ledger.Post(databaseTransaction, ledger.Entry{
			Reference: "CASHPOINT-" + cashpoint.ID + ":REDEEM", TransactionType: "cashpoint_redeem", Description: "Cashpoint redeemed",
			CreatedBy: "user:" + actor.UserID, BusinessTransactionID: &transaction.ID,
			Legs: ledger.MoveLegs(creatorHold.ID, userWallet.ID, amount, "Cashpoint redeemed"),
		})
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		if err := databaseTransaction.Model(&models.Cashpoint{}).Where("id = ?", cashpoint.ID).Updates(map[string]any{
			"status": "redeemed", "redeemed_by_user_id": actor.UserID, "redeem_transaction_id": transaction.ID,
			"redeemed_at": now, "updated_at": now, "version": cashpoint.Version + 1,
		}).Error; err != nil {
			return err
		}
		if err := databaseTransaction.Model(&models.Transaction{}).Where("id = ?", cashpoint.TransactionID).
			Updates(map[string]any{"status": models.TransactionStatusCompleted, "subtitle": "Redeemed", "completed_at": now, "updated_at": now}).Error; err != nil {
			return err
		}
		*transaction = models.Transaction{
			ID: transaction.ID, Reference: reference, UserID: actor.UserID,
			Category: "cashpoint", Type: "cashpoint_redeem", Direction: "in", Status: models.TransactionStatusCompleted,
			Title: "Cashpoint redeemed", FromAsset: "NGN", FromAmount: cashpoint.Amount, JournalEntryID: &postResult.Entry.ID,
			Metadata: shared.JSON(map[string]any{"cashpoint_id": cashpoint.ID}),
		}
		if err := shared.RecordTransaction(databaseTransaction, transaction); err != nil {
			return err
		}
		if err := shared.NotifyTransaction(databaseTransaction, transaction, "cashpoint", "Cashpoint collected"); err != nil {
			return err
		}
		return shared.Notify(databaseTransaction, cashpoint.CreatorUserID, "cashpoint", "Your cashpoint was redeemed", "₦"+amount.Display(2)+" was collected.", "")
	})
	if err != nil {
		return nil, err
	}
	return transaction, nil
}

// CancelCashpoint returns an unused code's Naira to its creator.
func (service *Service) CancelCashpoint(requestContext context.Context, actor shared.Actor, cashpointID string) error {
	userWallet, err := service.ledger.EnsureUserAccount(requestContext, actor.UserID, ledger.CodeUserPrimary, "NGN")
	if err != nil {
		return err
	}
	return database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		if _, err := shared.LockActor(databaseTransaction, actor); err != nil {
			return err
		}
		var cashpoint models.Cashpoint
		if err := database.LockForUpdate(databaseTransaction, &cashpoint, "id = ? AND creator_user_id = ?", cashpointID, actor.UserID); err != nil {
			return apperrors.NotFound("Cashpoint")
		}
		if cashpoint.Status != "active" {
			return apperrors.New(http.StatusUnprocessableEntity, "CODE_USED", "This code was already used or cancelled.")
		}
		amount, _ := money.ParseUnits(cashpoint.Amount)
		cashpointHold, err := service.ledger.UserAccount(databaseTransaction, actor.UserID, ledger.CodeUserCashpoint, "NGN")
		if err != nil {
			return err // opened by CreateCashpoint, so it exists
		}
		if _, err := service.ledger.Post(databaseTransaction, ledger.Entry{
			Reference: "CASHPOINT-" + cashpoint.ID + ":CANCEL", TransactionType: "cashpoint_cancel", Description: "Cashpoint cancelled",
			CreatedBy: "user:" + actor.UserID, BusinessTransactionID: &cashpoint.TransactionID,
			Legs: ledger.MoveLegs(cashpointHold.ID, userWallet.ID, amount, "Cashpoint cancelled"),
		}); err != nil {
			return err
		}
		now := time.Now().UTC()
		if err := databaseTransaction.Model(&models.Cashpoint{}).Where("id = ?", cashpoint.ID).
			Updates(map[string]any{"status": "cancelled", "cancelled_at": now, "updated_at": now, "version": cashpoint.Version + 1}).Error; err != nil {
			return err
		}
		return databaseTransaction.Model(&models.Transaction{}).Where("id = ?", cashpoint.TransactionID).
			Updates(map[string]any{"status": models.TransactionStatusCancelled, "subtitle": "Cancelled", "updated_at": now}).Error
	})
}

// ListCashpoints returns the user's codes (with the code decrypted for the creator).
func (service *Service) ListCashpoints(requestContext context.Context, userID string) ([]map[string]any, error) {
	var cashpoints []models.Cashpoint
	if err := service.database.WithContext(requestContext).Where("creator_user_id = ?", userID).Order("created_at DESC").Find(&cashpoints).Error; err != nil {
		return nil, err
	}
	list := make([]map[string]any, 0, len(cashpoints))
	for _, cashpoint := range cashpoints {
		code, _ := service.toolkit.Decrypt(cashpoint.CodeEncrypted)
		amount, _ := money.ParseUnits(cashpoint.Amount)
		list = append(list, map[string]any{
			"id": cashpoint.ID, "code": code, "amount": amount.Decimal(2), "status": cashpoint.Status,
			"transaction_id": cashpoint.TransactionID, "created_at": cashpoint.CreatedAt, "redeemed_at": cashpoint.RedeemedAt,
		})
	}
	return list, nil
}

// ─── Referrals ───────────────────────────────────────────────────────

// ReferralSummary is the referral screen.
type ReferralSummary struct {
	Code         string           `json:"code"`
	Balance      string           `json:"balance"`
	SharePercent string           `json:"share_percent"` // the share of Tribe's earnings an inviter gets, e.g. "2"
	Earnings     []map[string]any `json:"earnings"`
	Withdrawals  []map[string]any `json:"withdrawals"`
	Invited      int64            `json:"invited"`
}

// Referrals returns the referral balance and history.
func (service *Service) Referrals(requestContext context.Context, userID string) (*ReferralSummary, error) {
	query := service.database.WithContext(requestContext)
	var user models.User
	if err := query.Where("id = ?", userID).Take(&user).Error; err != nil {
		return nil, err
	}
	account, err := service.ledger.EnsureUserAccount(requestContext, userID, ledger.CodeUserReferral, "NGN")
	if err != nil {
		return nil, err
	}
	balance, err := service.ledger.BalanceOf(requestContext, account.ID)
	if err != nil {
		return nil, err
	}
	settings, err := service.referrals.Settings(requestContext)
	if err != nil {
		return nil, err
	}
	sharePercent, err := money.ParseRate(settings.SharePercent)
	if err != nil {
		return nil, err
	}
	summary := &ReferralSummary{
		Code: user.ReferralCode, Balance: balance.Decimal(2), SharePercent: trimmedPercent(sharePercent),
		Earnings: []map[string]any{}, Withdrawals: []map[string]any{},
	}
	query.Model(&models.User{}).Where("referred_by_user_id = ?", userID).Count(&summary.Invited)

	// Only earnings already in the referral balance. A share still waiting
	// for a provider to confirm the friend's transaction (accrued) is not
	// the inviter's money yet, and a cancelled one never will be.
	var earnings []models.ReferralEarning
	query.Where("user_id = ? AND status = ?", userID, models.ReferralPaid).Order("created_at DESC").Limit(100).Find(&earnings)
	for _, earning := range earnings {
		amount, _ := money.ParseUnits(earning.AmountNgn)
		summary.Earnings = append(summary.Earnings, map[string]any{"id": earning.ID, "friend": earning.FriendName, "what": earning.Description, "amount": amount.Decimal(2), "date": earning.CreatedAt})
	}
	var withdrawals []models.Transaction
	query.Where("user_id = ? AND type = ?", userID, "referral_withdraw").Order("created_at DESC").Limit(100).Find(&withdrawals)
	for _, withdrawal := range withdrawals {
		amount, _ := money.ParseUnits(withdrawal.FromAmount)
		summary.Withdrawals = append(summary.Withdrawals, map[string]any{"id": withdrawal.ID, "amount_ngn": amount.Decimal(2), "to": "NGN", "tx_id": withdrawal.ID, "date": withdrawal.CreatedAt})
	}
	return summary, nil
}

// trimmedPercent writes a percentage without trailing zeros ("2", "2.5").
func trimmedPercent(percent *big.Rat) string {
	text := percent.FloatString(6)
	text = strings.TrimRight(text, "0")
	return strings.TrimSuffix(text, ".")
}

// WithdrawReferral moves referral earnings into the NGN wallet.
func (service *Service) WithdrawReferral(requestContext context.Context, actor shared.Actor, amountNgn string, pin string, reference string) (*models.Transaction, error) {
	if err := service.auth.VerifyPin(requestContext, actor.UserID, pin); err != nil {
		return nil, err
	}
	amount, err := money.ParseDecimal(amountNgn, 2)
	if err != nil || !amount.IsPositive() {
		return nil, apperrors.Validation(map[string][]string{"amount_ngn": {"Enter a valid amount."}})
	}
	referralAccount, err := service.ledger.EnsureUserAccount(requestContext, actor.UserID, ledger.CodeUserReferral, "NGN")
	if err != nil {
		return nil, err
	}
	wallet, err := service.ledger.EnsureUserAccount(requestContext, actor.UserID, ledger.CodeUserPrimary, "NGN")
	if err != nil {
		return nil, err
	}
	transaction := &models.Transaction{ID: ids.New(), Reference: reference, UserID: actor.UserID}
	err = database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		if _, err := shared.LockActor(databaseTransaction, actor); err != nil {
			return err
		}
		postResult, err := service.ledger.Post(databaseTransaction, ledger.Entry{
			Reference: reference + ":POST", TransactionType: "referral_withdraw", Description: "Referral earnings withdrawn",
			CreatedBy: "user:" + actor.UserID, BusinessTransactionID: &transaction.ID,
			Legs: ledger.MoveLegs(referralAccount.ID, wallet.ID, amount, "Referral withdrawal"),
		})
		if err != nil {
			return err
		}
		*transaction = models.Transaction{
			ID: transaction.ID, Reference: reference, UserID: actor.UserID,
			Category: "referral", Type: "referral_withdraw", Direction: "in", Status: models.TransactionStatusCompleted,
			Title: "Referral earnings", Subtitle: "Moved to your NGN wallet", FromAsset: "NGN", FromAmount: amount.UnitsString(),
			JournalEntryID: &postResult.Entry.ID,
		}
		if err := shared.RecordTransaction(databaseTransaction, transaction); err != nil {
			return err
		}
		return shared.NotifyTransaction(databaseTransaction, transaction, "referral", "Referral earnings withdrawn")
	})
	if err != nil {
		return nil, err
	}
	return transaction, nil
}
