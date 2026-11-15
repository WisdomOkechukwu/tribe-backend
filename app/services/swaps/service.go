// Package swaps converts money between two of the user's own wallets, at
// the price of a quote the user already saw.
package swaps

import (
	"context"
	"time"

	"gorm.io/gorm"

	"github.com/tribe-africa/tribe-backend/app/apperrors"
	"github.com/tribe-africa/tribe-backend/app/jobs"
	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/app/services/auth"
	"github.com/tribe-africa/tribe-backend/app/services/ledger"
	"github.com/tribe-africa/tribe-backend/app/services/rates"
	"github.com/tribe-africa/tribe-backend/app/services/referrals"
	"github.com/tribe-africa/tribe-backend/app/services/shared"
	"github.com/tribe-africa/tribe-backend/database"
	"github.com/tribe-africa/tribe-backend/pkg/ids"
	"github.com/tribe-africa/tribe-backend/pkg/money"
)

// Service runs swaps.
type Service struct {
	database   *gorm.DB
	ledger     *ledger.Service
	rates      *rates.Service
	auth       *auth.Service
	referrals  *referrals.Service
	dispatcher *jobs.Dispatcher
}

// NewService creates the swaps service.
func NewService(connection *gorm.DB, ledgerService *ledger.Service, ratesService *rates.Service, authService *auth.Service, referralsService *referrals.Service, dispatcher *jobs.Dispatcher) *Service {
	return &Service{database: connection, ledger: ledgerService, rates: ratesService, auth: authService, referrals: referralsService, dispatcher: dispatcher}
}

// SwapInput is what the confirm button sends.
type SwapInput struct {
	QuoteID   string
	Pin       string
	Reference string // from the idempotency key, so a retry reuses it
}

// Swap executes a quoted swap. Step by step:
//
//  1. Check the PIN (outside the transaction so wrong tries are counted).
//  2. Make sure the user has a wallet for the target asset, and (when they
//     were invited) the accounts their inviter's referral share needs.
//  3. In ONE transaction:
//     a. re-check the user and phone (shared locks),
//     b. use the quote (only once, only before it expires),
//     c. post the journal entry (this locks the wallets and checks the
//     balance); fee and margin go to the Earnings Vault, and an inviter's
//     share of them to their referral balance,
//     d. save the transaction row, each route leg (swap_legs) and the
//     referral earning,
//     e. add an inbox message.
//  4. After commit: queue a push notification.
//
// If ANY step fails, the whole transaction rolls back: no money moves and the
// quote stays unused, so the user can try again.
func (service *Service) Swap(requestContext context.Context, actor shared.Actor, input SwapInput) (*models.Transaction, error) {
	if err := service.auth.VerifyPin(requestContext, actor.UserID, input.Pin); err != nil {
		return nil, err
	}

	// Read the quote (no lock yet) just to know which accounts we need.
	var preview models.Quote
	if err := service.database.WithContext(requestContext).Where("id = ? AND user_id = ?", input.QuoteID, actor.UserID).Take(&preview).Error; err != nil {
		return nil, apperrors.NotFound("Quote")
	}
	sourceWallet, err := service.ledger.EnsureUserAccount(requestContext, actor.UserID, ledger.CodeUserPrimary, preview.FromAsset)
	if err != nil {
		return nil, err
	}
	targetWallet, err := service.ledger.EnsureUserAccount(requestContext, actor.UserID, ledger.CodeUserPrimary, preview.ToAsset)
	if err != nil {
		return nil, err
	}
	decimals := shared.AssetDecimals(service.database.WithContext(requestContext))
	// A swap is final as soon as it is posted, so an inviter's share is paid at once.
	invitation, err := service.referrals.Prepare(requestContext, actor.UserID, true)
	if err != nil {
		return nil, err
	}

	transaction := &models.Transaction{ID: ids.New(), Reference: input.Reference, UserID: actor.UserID}
	err = database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		if _, err := shared.LockActor(databaseTransaction, actor); err != nil {
			return err
		}
		quote, err := service.rates.ConsumeQuote(databaseTransaction, input.QuoteID, actor.UserID, "swap")
		if err != nil {
			return err
		}
		totalPaid, _ := money.ParseUnits(quote.FromAmount)
		fee, _ := money.ParseUnits(quote.FeeAmount)
		received, _ := money.ParseUnits(quote.ToAmount)
		nairaIn, nairaOut, marketReceived := rates.MarginAmountsOf(quote)

		result, err := service.ledger.PostConversion(databaseTransaction, ledger.ConversionPosting{
			Reference: input.Reference + ":POST", TransactionType: "swap",
			Description: "Swap " + quote.FromAsset + " to " + quote.ToAsset, CreatedBy: "user:" + actor.UserID,
			BusinessTransactionID: &transaction.ID,
			SourceAccountID:       sourceWallet.ID, TargetAccountID: targetWallet.ID,
			FromAsset: quote.FromAsset, ToAsset: quote.ToAsset,
			Converted: totalPaid.Subtract(fee), Fee: fee, Received: received,
			NairaIn: nairaIn, NairaOut: nairaOut, MarketReceived: marketReceived,
			Rates:         service.rates.RatesFor(databaseTransaction, quote.FromAsset, quote.ToAsset, "NGN"),
			Metadata:      map[string]any{"quote_id": quote.ID, "uses_ngn_rail": quote.UsesNgnRail},
			ReferralShare: invitation.ShareOf(),
		})
		if err != nil {
			return err
		}

		toAsset, toAmount, feeAsset := quote.ToAsset, quote.ToAmount, quote.FromAsset
		transaction.Category, transaction.Type, transaction.Direction = "currency", "swap", "out"
		transaction.Status = models.TransactionStatusCompleted
		transaction.Title = "Swapped " + quote.FromAsset + " to " + quote.ToAsset
		transaction.Subtitle = "Rate " + quote.Rate
		transaction.FromAsset, transaction.FromAmount = quote.FromAsset, quote.FromAmount
		transaction.ToAsset, transaction.ToAmount = &toAsset, &toAmount
		transaction.FeeAsset, transaction.FeeAmount = &feeAsset, quote.FeeAmount
		transaction.QuoteID, transaction.JournalEntryID = &quote.ID, &result.Entry.ID
		transaction.UsdValue = service.rates.UsdValueOf(databaseTransaction, quote.FromAsset, totalPaid, decimals[quote.FromAsset])
		transaction.Metadata = shared.JSON(map[string]any{"uses_ngn_rail": quote.UsesNgnRail, "rate": quote.Rate})
		if err := shared.RecordTransaction(databaseTransaction, transaction); err != nil {
			return err
		}
		if err := SaveSwapLegs(databaseTransaction, transaction.ID, &quote.ID, rates.RouteOf(quote)); err != nil {
			return err
		}
		if err := service.referrals.Record(databaseTransaction, invitation, transaction, result); err != nil {
			return err
		}
		return shared.Notify(databaseTransaction, actor.UserID, "swap", "Swap complete",
			totalPaid.Decimal(decimals[quote.FromAsset])+" "+quote.FromAsset+" → "+received.Decimal(decimals[quote.ToAsset])+" "+quote.ToAsset,
			"/transaction/"+transaction.ID)
	})
	if err != nil {
		return nil, err
	}
	return transaction, nil
}

// SaveSwapLegs logs each hop of the route with its rate (USD -> NGN -> GBP is two rows).
func SaveSwapLegs(databaseTransaction *gorm.DB, transactionID string, quoteID *string, route []rates.RouteLeg) error {
	now := time.Now().UTC()
	for _, leg := range route {
		providerID := leg.ProviderID
		row := models.SwapLeg{
			TransactionID: transactionID, QuoteID: quoteID, Step: leg.Step,
			FromAsset: leg.FromAsset, ToAsset: leg.ToAsset, FromAmount: leg.FromAmount, ToAmount: leg.ToAmount,
			Rate: leg.Rate, ViaNgnRail: leg.ViaNgnRail, CreatedAt: now,
		}
		if providerID != "" {
			row.ProviderID = &providerID
		}
		if err := databaseTransaction.Create(&row).Error; err != nil {
			return err
		}
	}
	return nil
}
