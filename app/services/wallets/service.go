// Package wallets shows a user's balances, opens new currency wallets and
// handles deposits, manual admin credits & debits (adjustments.go) and the
// development "test top-up" used by seeders and tests.
package wallets

import (
	"context"
	"net/http"
	"time"

	"gorm.io/gorm"

	"github.com/tribe-africa/tribe-backend/app/apperrors"
	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/app/services/ledger"
	"github.com/tribe-africa/tribe-backend/app/services/rates"
	"github.com/tribe-africa/tribe-backend/app/services/shared"
	"github.com/tribe-africa/tribe-backend/config"
	"github.com/tribe-africa/tribe-backend/database"
	"github.com/tribe-africa/tribe-backend/pkg/ids"
	"github.com/tribe-africa/tribe-backend/pkg/money"
)

// Service handles wallets.
type Service struct {
	database *gorm.DB
	ledger   *ledger.Service
	rates    *rates.Service
	settings config.Config
}

// NewService creates the wallets service.
func NewService(connection *gorm.DB, ledgerService *ledger.Service, ratesService *rates.Service, settings config.Config) *Service {
	return &Service{database: connection, ledger: ledgerService, rates: ratesService, settings: settings}
}

// WalletBalance is one asset's balance for the app.
type WalletBalance struct {
	AssetCode     string `json:"asset_code"`
	Balance       string `json:"balance"`       // e.g. "1250.50"
	BalanceUnits  string `json:"balance_units"` // smallest units, e.g. "125050"
	DecimalPlaces int    `json:"decimal_places"`
	AccountNumber string `json:"account_number"`
}

// WalletsResult matches the app's accounts shape: {balances, opened}.
type WalletsResult struct {
	Balances        map[string]string `json:"balances"`
	Opened          []string          `json:"opened"`
	Wallets         []WalletBalance   `json:"wallets"`
	ReferralBalance string            `json:"referral_balance"`
}

// Wallets returns every opened wallet with its balance.
// Balances come from the ledger (snapshot + journal), never from a cache.
func (service *Service) Wallets(requestContext context.Context, userID string) (*WalletsResult, error) {
	decimals := shared.AssetDecimals(service.database.WithContext(requestContext))
	// Only wallets and the referral account: card balances are shown on
	// the Cards screen, so there is no point adding them up here.
	accountBalances, err := service.ledger.BalancesForUser(requestContext, userID, ledger.CodeUserPrimary, ledger.CodeUserReferral)
	if err != nil {
		return nil, err
	}
	result := &WalletsResult{Balances: map[string]string{}, Opened: []string{}, Wallets: []WalletBalance{}, ReferralBalance: "0.00"}
	for _, accountBalance := range accountBalances {
		account := accountBalance.Account
		places := decimals[account.AssetCode]
		switch account.Code {
		case ledger.CodeUserPrimary:
			text := accountBalance.Balance.Decimal(places)
			result.Balances[account.AssetCode] = text
			result.Opened = append(result.Opened, account.AssetCode)
			result.Wallets = append(result.Wallets, WalletBalance{
				AssetCode: account.AssetCode, Balance: text, BalanceUnits: accountBalance.Balance.UnitsString(),
				DecimalPlaces: places, AccountNumber: account.AccountNumber,
			})
		case ledger.CodeUserReferral:
			result.ReferralBalance = accountBalance.Balance.Decimal(places)
		}
	}
	return result, nil
}

// OpenWallet opens a wallet for a new currency (safe to call twice).
func (service *Service) OpenWallet(requestContext context.Context, userID string, assetCode string) error {
	if _, err := shared.LoadAsset(service.database.WithContext(requestContext), assetCode); err != nil {
		return err
	}
	_, err := service.ledger.EnsureUserAccount(requestContext, userID, ledger.CodeUserPrimary, assetCode)
	return err
}

// ReceiveDetails returns how to pay INTO a wallet. The values are
// placeholders until a real banking / crypto provider is connected.
func (service *Service) ReceiveDetails(requestContext context.Context, userID string, assetCode string) (map[string]any, error) {
	asset, err := shared.LoadAsset(service.database.WithContext(requestContext), assetCode)
	if err != nil {
		return nil, err
	}
	account, err := service.ledger.EnsureUserAccount(requestContext, userID, ledger.CodeUserPrimary, assetCode)
	if err != nil {
		return nil, err
	}
	if asset.Kind == "crypto" {
		return map[string]any{
			"asset_code": asset.Code, "kind": "crypto", "simulated": true,
			"address": "sim_" + asset.Code + "_" + account.AccountNumber,
		}, nil
	}
	return map[string]any{
		"asset_code": asset.Code, "kind": "fiat", "simulated": true,
		"fields": []map[string]string{
			{"label": "Account number", "value": account.AccountNumber},
			{"label": "Bank", "value": "Tribe Partner Bank (simulated)"},
		},
	}, nil
}

// TestTopUp adds money to a wallet through the REAL ledger (Dr the currency's
// home provider vault, Cr User Wallet). It only works outside production. There is no route for it
// any more (admins credit customers with RequestCustomerAdjustment instead);
// the seeders and integration tests call it to give demo users money.
func (service *Service) TestTopUp(requestContext context.Context, actor shared.Actor, assetCode string, amountText string, reference string) (*models.Transaction, error) {
	if service.settings.IsProduction() {
		return nil, apperrors.New(http.StatusForbidden, "NOT_AVAILABLE", "Test top-ups are disabled in production.")
	}
	asset, err := shared.LoadAsset(service.database.WithContext(requestContext), assetCode)
	if err != nil {
		return nil, err
	}
	amount, err := money.ParseDecimal(amountText, asset.DecimalPlaces)
	if err != nil || !amount.IsPositive() {
		return nil, apperrors.Validation(map[string][]string{"amount": {"Enter a valid amount."}})
	}
	userWallet, err := service.ledger.EnsureUserAccount(requestContext, actor.UserID, ledger.CodeUserPrimary, asset.Code)
	if err != nil {
		return nil, err
	}

	transaction := &models.Transaction{
		ID: ids.New(), Reference: reference, UserID: actor.UserID,
		Category: "currency", Type: "deposit", Direction: "in", Status: models.TransactionStatusCompleted,
		Title: "Test top-up", Subtitle: asset.Name + " (simulated)",
		FromAsset: asset.Code, FromAmount: amount.UnitsString(),
		UsdValue: service.rates.UsdValueOf(service.database.WithContext(requestContext), asset.Code, amount, asset.DecimalPlaces),
	}
	err = database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		if _, err := shared.LockActor(databaseTransaction, actor); err != nil {
			return err
		}
		vault, err := service.ledger.HomeVault(databaseTransaction, asset.Code)
		if err != nil {
			return err
		}
		result, err := service.ledger.Post(databaseTransaction, ledger.Entry{
			Reference: reference + ":POST", TransactionType: "deposit", Description: "Test top-up",
			CreatedBy: "user:" + actor.UserID, BusinessTransactionID: &transaction.ID,
			Legs: ledger.DepositLegs(vault.ID, userWallet.ID, amount),
		})
		if err != nil {
			return err
		}
		transaction.JournalEntryID = &result.Entry.ID
		if err := shared.RecordTransaction(databaseTransaction, transaction); err != nil {
			return err
		}
		return shared.Notify(databaseTransaction, actor.UserID, "money", "Money received", amount.Display(asset.DecimalPlaces)+" "+asset.Code+" added to your wallet.", "/transaction/"+transaction.ID)
	})
	if err != nil {
		return nil, err
	}
	return transaction, nil
}

// RequestTreasuryFunding records that the owners put Tribe's own money into a
// provider's vault (the admin "Record treasury funding" form). providerID
// picks the provider; empty means the currency's home provider. Nothing is posted yet: the
// request waits as "pending" until a DIFFERENT admin approves it
// (see ApproveTreasuryFunding). This "four eyes" rule stops one person from
// creating money on their own.
func (service *Service) RequestTreasuryFunding(requestContext context.Context, adminID string, adminName string, assetCode string, providerID string, amountText string, externalReference string, note string) (*models.TreasuryFunding, error) {
	asset, err := shared.LoadAsset(service.database.WithContext(requestContext), assetCode)
	if err != nil {
		return nil, err
	}
	if providerID == "" && asset.ConversionProviderID != nil {
		providerID = *asset.ConversionProviderID
	}
	// The provider must really hold this currency for us (it has a vault for it).
	if _, err := service.ledger.ProviderVault(service.database.WithContext(requestContext), providerID, asset.Code); err != nil {
		return nil, apperrors.Validation(map[string][]string{"provider_id": {"That provider does not hold " + asset.Code + "."}})
	}
	amount, err := money.ParseDecimal(amountText, asset.DecimalPlaces)
	if err != nil || !amount.IsPositive() {
		return nil, apperrors.Validation(map[string][]string{"amount": {"Enter a valid amount."}})
	}
	funding := &models.TreasuryFunding{
		ID: ids.New(), AssetCode: asset.Code, ProviderID: &providerID, Amount: amount.UnitsString(), Status: models.FundingPending,
		ExternalReference: externalReference, Note: note, RequestedBy: adminID, RequestedByName: adminName,
		CreatedAt: time.Now().UTC(),
	}
	if err := service.database.WithContext(requestContext).Create(funding).Error; err != nil {
		if database.IsDuplicateEntry(err) {
			return nil, apperrors.New(http.StatusConflict, "DUPLICATE_REFERENCE", "This bank reference was already recorded.")
		}
		return nil, err
	}
	return funding, nil
}

// ApproveTreasuryFunding is the second admin saying "yes, this money really
// arrived". In ONE database transaction it locks the request, checks it is
// still pending and was raised by someone else, then posts:
//
//	Dr the provider's vault / Cr Tribe Funds (source: treasury)
func (service *Service) ApproveTreasuryFunding(requestContext context.Context, adminID string, adminName string, fundingID string) (*models.TreasuryFunding, error) {
	var funding models.TreasuryFunding
	err := database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		if err := lockPendingFunding(databaseTransaction, &funding, fundingID, adminID); err != nil {
			return err
		}
		amount, err := money.ParseUnits(funding.Amount)
		if err != nil {
			return err
		}
		var vault *models.LedgerAccount
		if funding.ProviderID != nil {
			vault, err = service.ledger.ProviderVault(databaseTransaction, *funding.ProviderID, funding.AssetCode)
		} else {
			vault, err = service.ledger.HomeVault(databaseTransaction, funding.AssetCode)
		}
		if err != nil {
			return err
		}
		equity, err := service.ledger.TribeFunds(databaseTransaction, funding.AssetCode)
		if err != nil {
			return err
		}
		result, err := service.ledger.Post(databaseTransaction, ledger.Entry{
			Reference: "FUND-" + funding.AssetCode + "-" + funding.ExternalReference, TransactionType: "treasury_funding",
			Description: "Treasury funding: " + funding.Note, CreatedBy: "admin:" + adminID,
			Legs: ledger.TreasuryFundingLegs(vault.ID, equity.ID, amount),
		})
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		funding.Status = models.FundingApproved
		funding.JournalEntryID = result.Entry.ID
		funding.ApprovedBy = &adminID
		funding.ApprovedByName = adminName
		funding.DecidedAt = &now
		return databaseTransaction.Model(&models.TreasuryFunding{}).Where("id = ?", funding.ID).Updates(map[string]any{
			"status": funding.Status, "journal_entry_id": funding.JournalEntryID,
			"approved_by": adminID, "approved_by_name": adminName, "decided_at": now,
		}).Error
	})
	if err != nil {
		return nil, err
	}
	return &funding, nil
}

// RejectTreasuryFunding closes a pending request without posting anything.
// The same four-eyes rule applies: the requester cannot reject their own.
func (service *Service) RejectTreasuryFunding(requestContext context.Context, adminID string, adminName string, fundingID string, reason string) (*models.TreasuryFunding, error) {
	var funding models.TreasuryFunding
	err := database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		if err := lockPendingFunding(databaseTransaction, &funding, fundingID, adminID); err != nil {
			return err
		}
		now := time.Now().UTC()
		funding.Status = models.FundingRejected
		funding.ApprovedBy = &adminID
		funding.ApprovedByName = adminName
		funding.DecidedAt = &now
		funding.RejectionReason = reason
		return databaseTransaction.Model(&models.TreasuryFunding{}).Where("id = ?", funding.ID).Updates(map[string]any{
			"status": funding.Status, "approved_by": adminID, "approved_by_name": adminName,
			"decided_at": now, "rejection_reason": reason,
		}).Error
	})
	if err != nil {
		return nil, err
	}
	return &funding, nil
}

// lockPendingFunding locks a funding request and checks that the deciding
// admin may decide it: it must still be pending and raised by someone else.
func lockPendingFunding(databaseTransaction *gorm.DB, funding *models.TreasuryFunding, fundingID string, deciderID string) error {
	if err := database.LockForUpdate(databaseTransaction, funding, "id = ?", fundingID); err != nil {
		return apperrors.NotFound("Funding request")
	}
	if funding.Status != models.FundingPending {
		return apperrors.New(http.StatusConflict, "ALREADY_DECIDED", "This funding request was already "+funding.Status+".")
	}
	if funding.RequestedBy == deciderID {
		return apperrors.New(http.StatusForbidden, "FOUR_EYES_REQUIRED", "A different admin must approve or reject your own funding request.")
	}
	return nil
}
