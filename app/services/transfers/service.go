// Package transfers sends money: to another Tribe user (instant, inside the
// ledger) or out of Tribe to a bank account / crypto wallet (through a
// provider, finished later by a queue job).
package transfers

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/tribe-africa/tribe-backend/app/apperrors"
	"github.com/tribe-africa/tribe-backend/app/jobs"
	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/app/services/auth"
	"github.com/tribe-africa/tribe-backend/app/services/ledger"
	"github.com/tribe-africa/tribe-backend/app/services/providers"
	"github.com/tribe-africa/tribe-backend/app/services/rates"
	"github.com/tribe-africa/tribe-backend/app/services/shared"
	"github.com/tribe-africa/tribe-backend/app/services/swaps"
	"github.com/tribe-africa/tribe-backend/database"
	"github.com/tribe-africa/tribe-backend/pkg/ids"
	"github.com/tribe-africa/tribe-backend/pkg/money"
)

// Service sends money.
type Service struct {
	database   *gorm.DB
	ledger     *ledger.Service
	rates      *rates.Service
	auth       *auth.Service
	providers  *providers.Registry
	dispatcher *jobs.Dispatcher
}

// NewService creates the transfers service.
func NewService(connection *gorm.DB, ledgerService *ledger.Service, ratesService *rates.Service, authService *auth.Service, registry *providers.Registry, dispatcher *jobs.Dispatcher) *Service {
	return &Service{database: connection, ledger: ledgerService, rates: ratesService, auth: authService, providers: registry, dispatcher: dispatcher}
}

// SendInput is everything the send flow collects.
type SendInput struct {
	RecipientKind   string // tribe | bank | wallet
	Handle          string // @tag, account number or wallet address
	RecipientName   string
	AssetCode       string // what the user pays with
	Amount          string // how much they send (without fee), when no quote is used
	QuoteID         string // set when the recipient gets a DIFFERENT currency
	ChannelID       string
	Network         string
	Country         string
	Details         map[string]string // bank name, sort code, IBAN ...
	SaveBeneficiary bool
	Pin             string
	Reference       string
}

// Send moves the money. Tribe-to-Tribe sends finish instantly; sends to
// banks/wallets are "pending" until the payout job hears from the provider.
//
// Each kind of send is its own service an admin can switch off (see
// shared.SendServiceFor). sendToTribeUser and sendOut check theirs as soon
// as they know which one it is, before any money moves. ProcessPayout is
// NOT checked: a payout that already took the customer's money must still
// be settled or refunded while the service is off.
func (service *Service) Send(requestContext context.Context, actor shared.Actor, input SendInput) (*models.Transaction, error) {
	if err := service.auth.VerifyPin(requestContext, actor.UserID, input.Pin); err != nil {
		return nil, err
	}
	input.RecipientKind = strings.ToLower(input.RecipientKind)
	switch input.RecipientKind {
	case "tribe":
		return service.sendToTribeUser(requestContext, actor, input)
	case "bank", "wallet":
		return service.sendOut(requestContext, actor, input)
	}
	return nil, apperrors.Validation(map[string][]string{"recipient_kind": {"Choose tribe, bank or wallet."}})
}

// FindRecipient looks up a Tribe user by @tag (for the "Send to" screen).
func (service *Service) FindRecipient(requestContext context.Context, tag string) (*models.User, error) {
	var user models.User
	cleanTag := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(tag), "@"))
	if err := service.database.WithContext(requestContext).Where("tag = ? AND status = ?", cleanTag, "active").Take(&user).Error; err != nil {
		return nil, apperrors.NotFound("Tribe user @" + cleanTag)
	}
	return &user, nil
}

// sendToTribeUser pays another Tribe user, same or different currency.
func (service *Service) sendToTribeUser(requestContext context.Context, actor shared.Actor, input SendInput) (*models.Transaction, error) {
	if err := shared.RequireService(service.database.WithContext(requestContext), shared.ServiceSendTribe); err != nil {
		return nil, err
	}
	recipient, err := service.FindRecipient(requestContext, input.Handle)
	if err != nil {
		return nil, err
	}
	if recipient.ID == actor.UserID {
		return nil, apperrors.New(http.StatusUnprocessableEntity, "SELF_TRANSFER", "You can't send money to yourself. Use Swap instead.")
	}
	decimals := shared.AssetDecimals(service.database.WithContext(requestContext))

	// Work out the currencies and amounts (from the quote, or same-currency).
	fromAsset, toAsset := input.AssetCode, input.AssetCode
	var preview *models.Quote
	if input.QuoteID != "" {
		preview = &models.Quote{}
		if err := service.database.WithContext(requestContext).Where("id = ? AND user_id = ?", input.QuoteID, actor.UserID).Take(preview).Error; err != nil {
			return nil, apperrors.NotFound("Quote")
		}
		fromAsset, toAsset = preview.FromAsset, preview.ToAsset
	}
	if _, err := shared.LoadAsset(service.database.WithContext(requestContext), fromAsset); err != nil {
		return nil, err
	}
	senderWallet, err := service.ledger.EnsureUserAccount(requestContext, actor.UserID, ledger.CodeUserPrimary, fromAsset)
	if err != nil {
		return nil, err
	}
	receiverWallet, err := service.ledger.EnsureUserAccount(requestContext, recipient.ID, ledger.CodeUserPrimary, toAsset)
	if err != nil {
		return nil, err
	}

	sent := &models.Transaction{ID: ids.New(), Reference: input.Reference, UserID: actor.UserID}
	received := &models.Transaction{ID: ids.New(), Reference: input.Reference + ":IN", UserID: recipient.ID}
	err = database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		sender, err := shared.LockActor(databaseTransaction, actor)
		if err != nil {
			return err
		}
		var converted, fee, arrived money.Amount
		var postResult *ledger.PostResult
		if preview != nil {
			quote, err := service.rates.ConsumeQuote(databaseTransaction, input.QuoteID, actor.UserID, "send")
			if err != nil {
				return err
			}
			totalPaid, _ := money.ParseUnits(quote.FromAmount)
			fee, _ = money.ParseUnits(quote.FeeAmount)
			arrived, _ = money.ParseUnits(quote.ToAmount)
			converted = totalPaid.Subtract(fee)
			var nairaIn, nairaOut *money.Amount
			if quote.NairaInAmount != nil {
				parsedIn, _ := money.ParseUnits(*quote.NairaInAmount)
				parsedOut, _ := money.ParseUnits(*quote.NairaOutAmount)
				nairaIn, nairaOut = &parsedIn, &parsedOut
			}
			postResult, err = service.ledger.PostConversion(databaseTransaction, ledger.ConversionPosting{
				Reference: input.Reference + ":POST", TransactionType: "send_tribe", Description: "Send to @" + recipient.Tag,
				CreatedBy: "user:" + actor.UserID, BusinessTransactionID: &sent.ID,
				SourceAccountID: senderWallet.ID, TargetAccountID: receiverWallet.ID,
				FromAsset: fromAsset, ToAsset: toAsset, Converted: converted, Fee: fee, Received: arrived,
				NairaIn: nairaIn, NairaOut: nairaOut, Rates: service.rates.RatesFor(databaseTransaction, fromAsset, toAsset, "NGN"),
			})
			if err != nil {
				return err
			}
			if err := swaps.SaveSwapLegs(databaseTransaction, sent.ID, &quote.ID, rates.RouteOf(quote)); err != nil {
				return err
			}
		} else {
			converted, err = money.ParseDecimal(input.Amount, decimals[fromAsset])
			if err != nil || !converted.IsPositive() {
				return apperrors.Validation(map[string][]string{"amount": {"Enter a valid amount."}})
			}
			fee, err = service.rates.FeeFor(databaseTransaction, "send_tribe", fromAsset, converted)
			if err != nil {
				return err
			}
			arrived = converted
			feeRevenue, err := service.ledger.CompanyAccount(databaseTransaction, ledger.CodeFeeRevenue, fromAsset)
			if err != nil {
				return err
			}
			postResult, err = service.ledger.Post(databaseTransaction, ledger.Entry{
				Reference: input.Reference + ":POST", TransactionType: "send_tribe", Description: "Send to @" + recipient.Tag,
				CreatedBy: "user:" + actor.UserID, BusinessTransactionID: &sent.ID,
				Legs: ledger.InternalTransferLegs(senderWallet.ID, receiverWallet.ID, feeRevenue.ID, converted, fee),
			})
			if err != nil {
				return err
			}
		}

		toAssetCopy, arrivedText, feeAsset := toAsset, arrived.UnitsString(), fromAsset
		usdValue := service.rates.UsdValueOf(databaseTransaction, fromAsset, converted, decimals[fromAsset])
		*sent = models.Transaction{
			ID: sent.ID, Reference: sent.Reference, UserID: actor.UserID,
			Category: "currency", Type: "send", Direction: "out", Status: models.TransactionStatusCompleted,
			Title: "Sent to " + recipient.FullName(), Subtitle: "@" + recipient.Tag,
			FromAsset: fromAsset, FromAmount: converted.Add(fee).UnitsString(), ToAsset: &toAssetCopy, ToAmount: &arrivedText,
			FeeAsset: &feeAsset, FeeAmount: fee.UnitsString(), UsdValue: usdValue, JournalEntryID: &postResult.Entry.ID,
			Metadata: shared.JSON(map[string]any{"recipient_kind": "tribe", "handle": "@" + recipient.Tag, "recipient": recipient.FullName(), "scope": "local"}),
		}
		*received = models.Transaction{
			ID: received.ID, Reference: received.Reference, UserID: recipient.ID,
			Category: "currency", Type: "receive", Direction: "in", Status: models.TransactionStatusCompleted,
			Title: "Received from " + sender.FullName(), Subtitle: "@" + sender.Tag,
			FromAsset: toAsset, FromAmount: arrivedText, UsdValue: usdValue, JournalEntryID: &postResult.Entry.ID,
			Metadata: shared.JSON(map[string]any{"sender": map[string]string{"name": sender.FullName(), "detail": "@" + sender.Tag, "kind": "tribe"}}),
		}
		if err := shared.RecordTransaction(databaseTransaction, sent); err != nil {
			return err
		}
		if err := shared.NotifyTransaction(databaseTransaction, sent, "money", "Money sent"); err != nil {
			return err
		}
		if err := shared.RecordTransaction(databaseTransaction, received); err != nil {
			return err
		}
		if input.SaveBeneficiary {
			service.saveBeneficiary(databaseTransaction, actor.UserID, "tribe", recipient.FullName(), "@"+recipient.Tag, toAsset, input)
		}
		return shared.Notify(databaseTransaction, recipient.ID, "money", "Money received",
			sender.FullName()+" sent you "+arrived.Decimal(decimals[toAsset])+" "+toAsset, "/transaction/"+received.ID)
	})
	if err != nil {
		return nil, err
	}
	return sent, nil
}

// sendOut sends money to a bank account or crypto wallet outside Tribe.
// The user's money is taken now and parked in "Payouts In Transit"; the
// ProcessPayout job asks the provider and then confirms or refunds.
func (service *Service) sendOut(requestContext context.Context, actor shared.Actor, input SendInput) (*models.Transaction, error) {
	if strings.TrimSpace(input.Handle) == "" {
		return nil, apperrors.Validation(map[string][]string{"handle": {"Enter the account number or wallet address."}})
	}
	decimals := shared.AssetDecimals(service.database.WithContext(requestContext))
	fromAsset, payoutAsset := input.AssetCode, input.AssetCode
	var preview *models.Quote
	if input.QuoteID != "" {
		preview = &models.Quote{}
		if err := service.database.WithContext(requestContext).Where("id = ? AND user_id = ?", input.QuoteID, actor.UserID).Take(preview).Error; err != nil {
			return nil, apperrors.NotFound("Quote")
		}
		fromAsset, payoutAsset = preview.FromAsset, preview.ToAsset
	}
	// Now we know what arrives and where: a crypto wallet (send_crypto), a
	// Naira bank account (send_bank) or anything else abroad (send_global).
	if err := shared.RequireService(service.database.WithContext(requestContext), shared.SendServiceFor(input.RecipientKind, payoutAsset)); err != nil {
		return nil, err
	}
	if _, err := shared.LoadAsset(service.database.WithContext(requestContext), fromAsset); err != nil {
		return nil, err
	}
	userWallet, err := service.ledger.EnsureUserAccount(requestContext, actor.UserID, ledger.CodeUserPrimary, fromAsset)
	if err != nil {
		return nil, err
	}
	service.EnsurePayoutRefundWallet(requestContext, actor.UserID, payoutAsset)

	transaction := &models.Transaction{ID: ids.New(), Reference: input.Reference, UserID: actor.UserID}
	err = database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		if _, err := shared.LockActor(databaseTransaction, actor); err != nil {
			return err
		}
		payoutInTransit, err := service.ledger.CompanyAccount(databaseTransaction, ledger.CodePayoutInTransit, payoutAsset)
		if err != nil {
			return err
		}
		var converted, fee, payoutAmount money.Amount
		var postResult *ledger.PostResult
		if preview != nil {
			quote, err := service.rates.ConsumeQuote(databaseTransaction, input.QuoteID, actor.UserID, "send")
			if err != nil {
				return err
			}
			totalPaid, _ := money.ParseUnits(quote.FromAmount)
			fee, _ = money.ParseUnits(quote.FeeAmount)
			payoutAmount, _ = money.ParseUnits(quote.ToAmount)
			converted = totalPaid.Subtract(fee)
			var nairaIn, nairaOut *money.Amount
			if quote.NairaInAmount != nil {
				parsedIn, _ := money.ParseUnits(*quote.NairaInAmount)
				parsedOut, _ := money.ParseUnits(*quote.NairaOutAmount)
				nairaIn, nairaOut = &parsedIn, &parsedOut
			}
			// Convert straight into "Payouts In Transit" of the payout currency.
			postResult, err = service.ledger.PostConversion(databaseTransaction, ledger.ConversionPosting{
				Reference: input.Reference + ":POST", TransactionType: "send_external", Description: "Payout to " + input.Handle,
				CreatedBy: "user:" + actor.UserID, BusinessTransactionID: &transaction.ID,
				SourceAccountID: userWallet.ID, TargetAccountID: payoutInTransit.ID,
				FromAsset: fromAsset, ToAsset: payoutAsset, Converted: converted, Fee: fee, Received: payoutAmount,
				NairaIn: nairaIn, NairaOut: nairaOut, Rates: service.rates.RatesFor(databaseTransaction, fromAsset, payoutAsset, "NGN"),
			})
			if err != nil {
				return err
			}
			if err := swaps.SaveSwapLegs(databaseTransaction, transaction.ID, &quote.ID, rates.RouteOf(quote)); err != nil {
				return err
			}
		} else {
			converted, err = money.ParseDecimal(input.Amount, decimals[fromAsset])
			if err != nil || !converted.IsPositive() {
				return apperrors.Validation(map[string][]string{"amount": {"Enter a valid amount."}})
			}
			fee, err = service.rates.FeeFor(databaseTransaction, "send_external", fromAsset, converted)
			if err != nil {
				return err
			}
			payoutAmount = converted
			feeRevenue, err := service.ledger.CompanyAccount(databaseTransaction, ledger.CodeFeeRevenue, fromAsset)
			if err != nil {
				return err
			}
			postResult, err = service.ledger.Post(databaseTransaction, ledger.Entry{
				Reference: input.Reference + ":POST", TransactionType: "send_external", Description: "Payout to " + input.Handle,
				CreatedBy: "user:" + actor.UserID, BusinessTransactionID: &transaction.ID,
				Legs: ledger.PayoutStartLegs(userWallet.ID, payoutInTransit.ID, feeRevenue.ID, converted, fee),
			})
			if err != nil {
				return err
			}
		}

		payoutAssetCopy, payoutText, feeAsset := payoutAsset, payoutAmount.UnitsString(), fromAsset
		name := input.RecipientName
		if name == "" {
			name = input.Handle
		}
		scope := "global"
		if input.RecipientKind == "wallet" {
			scope = "crypto"
		} else if payoutAsset == "NGN" {
			scope = "local"
		}
		*transaction = models.Transaction{
			ID: transaction.ID, Reference: transaction.Reference, UserID: actor.UserID,
			Category: "currency", Type: "send", Direction: "out", Status: models.TransactionStatusPending,
			Title: "Sent to " + name, Subtitle: input.Handle,
			FromAsset: fromAsset, FromAmount: converted.Add(fee).UnitsString(), ToAsset: &payoutAssetCopy, ToAmount: &payoutText,
			FeeAsset: &feeAsset, FeeAmount: fee.UnitsString(),
			UsdValue:       service.rates.UsdValueOf(databaseTransaction, fromAsset, converted, decimals[fromAsset]),
			JournalEntryID: &postResult.Entry.ID,
			Metadata: shared.JSON(map[string]any{
				"recipient_kind": input.RecipientKind, "handle": input.Handle, "recipient": name, "channel": input.ChannelID,
				"network": input.Network, "country": input.Country, "scope": scope, "details": input.Details,
				"payout_asset": payoutAsset, "payout_amount": payoutText,
			}),
		}
		if err := shared.RecordTransaction(databaseTransaction, transaction); err != nil {
			return err
		}
		if err := shared.NotifyTransaction(databaseTransaction, transaction, "money", "Payment on its way"); err != nil {
			return err
		}
		if input.SaveBeneficiary {
			service.saveBeneficiary(databaseTransaction, actor.UserID, input.RecipientKind, name, input.Handle, payoutAsset, input)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// The provider is called by the worker, never inside a database transaction.
	service.dispatcher.ProcessPayout(requestContext, transaction.ID)
	return transaction, nil
}

// ProcessPayout is run by the ProcessPayout queue job.
//
//  1. If the transaction is no longer pending, stop (job ran twice: fine).
//  2. Ask the provider to pay (OUTSIDE any database transaction; slow calls
//     must never hold row locks). The reference is sent as the provider's own
//     idempotency key, so a retry can't pay twice at the provider either.
//  3. In a transaction: lock the transaction row, check it is STILL pending,
//     then post "confirmed" (Dr In Transit / Cr Vault) or "refund"
//     (Dr In Transit / Cr User Wallet) and update the status.
func (service *Service) ProcessPayout(requestContext context.Context, transactionID string) error {
	var transaction models.Transaction
	if err := service.database.WithContext(requestContext).Where("id = ?", transactionID).Take(&transaction).Error; err != nil {
		return err
	}
	if transaction.Status != models.TransactionStatusPending {
		return nil
	}
	var metadata struct {
		RecipientKind string            `json:"recipient_kind"`
		Handle        string            `json:"handle"`
		Details       map[string]string `json:"details"`
		PayoutAsset   string            `json:"payout_asset"`
		PayoutAmount  string            `json:"payout_amount"`
	}
	json.Unmarshal(transaction.Metadata, &metadata)
	destination := map[string]string{"handle": metadata.Handle}
	for key, value := range metadata.Details {
		destination[key] = value
	}

	category := "global"
	if metadata.RecipientKind == "wallet" {
		category = "crypto"
	}
	payoutProvider, err := service.providers.Payouts(category)
	if err != nil {
		return err
	}
	result, err := payoutProvider.SendPayout(requestContext, transaction.Reference, metadata.PayoutAsset, metadata.PayoutAmount, destination)
	if err != nil {
		return err // the queue retries later
	}
	if result.Status == "pending" {
		return nil // a webhook or the next retry will finish it
	}

	payoutAmount, _ := money.ParseUnits(metadata.PayoutAmount)
	return database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		var locked models.Transaction
		if err := database.LockForUpdate(databaseTransaction, &locked, "id = ?", transactionID); err != nil {
			return err
		}
		if locked.Status != models.TransactionStatusPending {
			return nil // someone else finished it while we waited for the provider
		}
		inTransit, err := service.ledger.CompanyAccount(databaseTransaction, ledger.CodePayoutInTransit, metadata.PayoutAsset)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		if result.Status == "succeeded" {
			vault, err := service.ledger.CompanyAccount(databaseTransaction, ledger.CodeCompanyVault, metadata.PayoutAsset)
			if err != nil {
				return err
			}
			if _, err := service.ledger.Post(databaseTransaction, ledger.Entry{
				Reference: locked.Reference + ":SETTLE", TransactionType: "send_external_settled", Description: "Payout confirmed " + result.ProviderReference,
				BusinessTransactionID: &locked.ID, Legs: ledger.PayoutSucceededLegs(inTransit.ID, vault.ID, payoutAmount),
			}); err != nil {
				return err
			}
			if err := databaseTransaction.Model(&models.Transaction{}).Where("id = ?", locked.ID).
				Updates(map[string]any{"status": models.TransactionStatusCompleted, "completed_at": now, "updated_at": now, "version": gorm.Expr("version + 1")}).Error; err != nil {
				return err
			}
			return shared.Notify(databaseTransaction, locked.UserID, "money", "Money delivered", locked.Title, "/transaction/"+locked.ID)
		}

		// Failed: refund into the user's wallet of the payout currency.
		var refundWallet models.LedgerAccount
		if err := databaseTransaction.Where("owner_key = ? AND code = ? AND asset_code = ?", "user:"+locked.UserID, ledger.CodeUserPrimary, metadata.PayoutAsset).Take(&refundWallet).Error; err != nil {
			slog.Error("refund wallet missing", "transaction", locked.ID, "asset", metadata.PayoutAsset)
			return err
		}
		if _, err := service.ledger.Post(databaseTransaction, ledger.Entry{
			Reference: locked.Reference + ":REFUND", TransactionType: "send_external_refunded", Description: "Payout failed: " + result.FailureReason,
			BusinessTransactionID: &locked.ID, Legs: ledger.PayoutFailedLegs(inTransit.ID, refundWallet.ID, payoutAmount),
		}); err != nil {
			return err
		}
		if err := databaseTransaction.Model(&models.Transaction{}).Where("id = ?", locked.ID).
			Updates(map[string]any{"status": models.TransactionStatusFailed, "failure_reason": result.FailureReason, "updated_at": now, "version": gorm.Expr("version + 1")}).Error; err != nil {
			return err
		}
		return shared.Notify(databaseTransaction, locked.UserID, "money", "Payment failed and refunded", result.FailureReason, "/transaction/"+locked.ID)
	})
}

// EnsurePayoutRefundWallet makes sure the user has a wallet in the payout
// currency BEFORE a payout starts, so a refund always has somewhere to go.
func (service *Service) EnsurePayoutRefundWallet(requestContext context.Context, userID string, assetCode string) {
	service.ledger.EnsureUserAccount(requestContext, userID, ledger.CodeUserPrimary, assetCode)
}

// saveBeneficiary stores the recipient for next time (ignored if already saved).
func (service *Service) saveBeneficiary(databaseTransaction *gorm.DB, userID string, kind string, name string, handle string, assetCode string, input SendInput) {
	beneficiary := models.Beneficiary{
		ID: ids.New(), UserID: userID, Kind: kind, Name: name, Handle: handle, AssetCode: assetCode,
		Details: shared.JSON(input.Details), DedupeKey: kind + "|" + assetCode + "|" + strings.ToLower(handle), CreatedAt: time.Now().UTC(),
	}
	if input.ChannelID != "" {
		beneficiary.ChannelID = &input.ChannelID
	}
	if input.Network != "" {
		beneficiary.Network = &input.Network
	}
	if input.Country != "" {
		beneficiary.Country = &input.Country
	}
	// A SAVEPOINT keeps a duplicate from breaking the surrounding transaction.
	databaseTransaction.SavePoint("beneficiary")
	if err := databaseTransaction.Create(&beneficiary).Error; err != nil {
		databaseTransaction.RollbackTo("beneficiary")
	}
}

// SaveBeneficiaryInput is a recipient saved from the "Add beneficiary" form.
type SaveBeneficiaryInput struct {
	Kind      string // tribe | bank | wallet
	Name      string
	Handle    string
	AssetCode string
	ChannelID string
	Network   string
	Country   string
	Details   map[string]string
}

// SaveBeneficiary stores a recipient for later. The same recipient twice
// (same kind, currency and handle) is refused with 409 BENEFICIARY_EXISTS,
// enforced by the UNIQUE dedupe key, so two taps cannot create two rows.
func (service *Service) SaveBeneficiary(requestContext context.Context, userID string, input SaveBeneficiaryInput) (*models.Beneficiary, error) {
	query := service.database.WithContext(requestContext)
	if _, err := shared.LoadAsset(query, input.AssetCode); err != nil {
		return nil, err
	}
	if input.Kind == "tribe" {
		if _, err := service.FindRecipient(requestContext, input.Handle); err != nil {
			return nil, err
		}
	}
	beneficiary := models.Beneficiary{
		ID: ids.New(), UserID: userID, Kind: input.Kind, Name: input.Name, Handle: input.Handle, AssetCode: input.AssetCode,
		Details: shared.JSON(input.Details), DedupeKey: input.Kind + "|" + input.AssetCode + "|" + strings.ToLower(input.Handle), CreatedAt: time.Now().UTC(),
	}
	if input.ChannelID != "" {
		beneficiary.ChannelID = &input.ChannelID
	}
	if input.Network != "" {
		beneficiary.Network = &input.Network
	}
	if input.Country != "" {
		beneficiary.Country = &input.Country
	}
	if err := query.Create(&beneficiary).Error; err != nil {
		if database.IsDuplicateEntry(err) {
			return nil, apperrors.New(http.StatusConflict, "BENEFICIARY_EXISTS", "You already saved this recipient.")
		}
		return nil, err
	}
	return &beneficiary, nil
}

// ListBeneficiaries returns saved recipients.
func (service *Service) ListBeneficiaries(requestContext context.Context, userID string) ([]models.Beneficiary, error) {
	var beneficiaries []models.Beneficiary
	err := service.database.WithContext(requestContext).Where("user_id = ?", userID).Order("created_at DESC").Find(&beneficiaries).Error
	return beneficiaries, err
}

// DeleteBeneficiary removes a saved recipient.
func (service *Service) DeleteBeneficiary(requestContext context.Context, userID string, beneficiaryID string) error {
	result := service.database.WithContext(requestContext).Where("id = ? AND user_id = ?", beneficiaryID, userID).Delete(&models.Beneficiary{})
	if result.RowsAffected == 0 {
		return apperrors.NotFound("Beneficiary")
	}
	return result.Error
}

// Channel is a way to send money out (shown in the "Select channel" picker).
type Channel struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Eta        string   `json:"eta"`
	FeeUsd     string   `json:"fee_usd"`
	Currencies []string `json:"currencies"`
}

// Channels lists payout channels (same ids as the app's channels.ts).
func Channels() []Channel {
	return []Channel{
		{"ngn-bank", "Bank transfer (NIP)", "Within minutes", "0.02", []string{"NGN"}},
		{"ach", "ACH", "1-3 business days", "1.00", []string{"USD"}},
		{"fedwire", "Fedwire", "Same day", "15.00", []string{"USD"}},
		{"card", "Debit card", "Within minutes", "2.00", []string{"USD"}},
		{"fps", "Faster Payments", "Within minutes", "0.50", []string{"GBP"}},
		{"bacs", "BACS", "3 business days", "0.30", []string{"GBP"}},
		{"sepa", "SEPA", "1 business day", "0.50", []string{"EUR"}},
		{"sepa-instant", "SEPA Instant", "Within seconds", "1.00", []string{"EUR"}},
		{"swift", "SWIFT", "2-5 business days", "20.00", []string{"USD", "GBP", "EUR", "CAD"}},
		{"interac", "Interac e-Transfer", "Within minutes", "1.00", []string{"CAD"}},
		{"eft", "EFT", "1-2 business days", "0.75", []string{"CAD"}},
		{"momo", "Mobile money", "Within minutes", "0.40", []string{"KES", "GHS"}},
	}
}
