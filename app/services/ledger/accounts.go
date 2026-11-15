// Package ledger is the accounting engine (Nexum double-entry).
//
// The three ideas you need:
//
//  1. Every account has a CATEGORY (asset, liability, revenue ...). The
//     category decides which side makes the balance go UP:
//
//     Category            | Increases with | Decreases with
//     --------------------|----------------|---------------
//     asset               | debit          | credit
//     liability           | credit         | debit
//     revenue             | credit         | debit
//     expense             | debit          | credit
//     equity              | credit         | debit
//     contra_asset        | debit          | credit
//     contra_liability    | credit         | debit
//     suspense_asset      | debit          | credit
//     suspense_liability  | credit         | debit
//     summary             | credit         | debit
//
//  2. Every money movement is a JOURNAL ENTRY made of LEGS. Each leg debits
//     or credits one account. For every asset in the entry,
//     total debits == total credits. Money is never created or lost.
//
//  3. An account's balance = its latest daily snapshot + every leg posted
//     after that snapshot. (See balances.go and snapshots.go.)
//
// A user's wallet is a LIABILITY for Tribe: it is money Tribe owes the user.
// So when a user receives ₦100 we CREDIT their account (liability goes up),
// and when they spend ₦100 we DEBIT it (liability goes down).
package ledger

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"time"

	"gorm.io/gorm"

	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/database"
)

// Sides of a leg.
const (
	Debit  = "debit"
	Credit = "credit"
)

// Account categories (the Nexum table).
const (
	CategoryAsset             = "asset"
	CategoryLiability         = "liability"
	CategoryRevenue           = "revenue"
	CategoryExpense           = "expense"
	CategoryEquity            = "equity"
	CategoryContraAsset       = "contra_asset"
	CategoryContraLiability   = "contra_liability"
	CategorySuspenseAsset     = "suspense_asset"
	CategorySuspenseLiability = "suspense_liability"
	CategorySummary           = "summary"
)

// NormalSideForCategory returns the side that INCREASES an account of this
// category. This is the Nexum "Account Type Rules" table in code.
func NormalSideForCategory(category string) string {
	switch category {
	case CategoryAsset, CategoryExpense, CategoryContraAsset, CategorySuspenseAsset:
		return Debit
	case CategoryLiability, CategoryRevenue, CategoryEquity, CategoryContraLiability, CategorySuspenseLiability, CategorySummary:
		return Credit
	}
	panic("unknown account category: " + category)
}

// Account codes. A code + an asset + an owner identifies one account.
//
// The chart has only three kinds of account, because the PROVIDERS hold the
// money and Tribe only keeps track of it:
//
//	provider vaults (asset)       what each provider holds for us, per currency
//	customer balances (liability) what Tribe owes its customers
//	Tribe Funds (equity)          Tribe's own share of the money in the vaults
//
// Every entry balances per currency, so for each currency:
//
//	Tribe Funds = sum of the provider vaults - everything owed to customers
const (
	// Accounts that belong to a user.
	CodeUserPrimary   = "user_primary"   // the user's wallet balance for one asset
	CodeUserReferral  = "user_referral"  // referral earnings (NGN) waiting to be withdrawn
	CodeUserCard      = "user_card"      // the balance loaded on one virtual card (USD)
	CodeUserCashpoint = "user_cashpoint" // Naira the user set aside for cashpoint codes not yet redeemed

	// One per provider per currency it handles (provider_assets).
	CodeProviderVault = "provider_vault" // what that provider holds for Tribe in that currency

	// One per currency (CompanyAccountTemplates).
	CodeTribeFunds = "tribe_funds" // Tribe's own money in the vaults: fees, margins, treasury top-ups
)

// ReferralAsset is the currency referral rewards are counted and paid in.
// Every share is converted to Naira at the market rate when it is earned.
const ReferralAsset = "NGN"

// Where a Tribe Funds leg came from. It is stored on the leg
// (journal_legs.earning_source) so one account can still be split in reports.
const (
	SourceFee            = "fee"             // a fee the customer paid
	SourceMargin         = "margin"          // our spread on a conversion
	SourceAdjustment     = "adjustment"      // a manual credit or debit made by admins
	SourceTreasury       = "treasury"        // the owners topped up a provider vault
	SourceReferralReward = "referral_reward" // an inviter's share, paid out of Tribe Funds
)

// CompanyAccountTemplate describes one company account created for every asset.
type CompanyAccountTemplate struct {
	Code     string
	Name     string
	Category string
}

// CompanyAccountTemplates is the company chart of accounts: the seeder
// creates one of each for every asset. It is only Tribe Funds now; the
// money itself is in the provider vaults (EnsureProviderVault).
var CompanyAccountTemplates = []CompanyAccountTemplate{
	{CodeTribeFunds, "Tribe Funds", CategoryEquity},
}

// Owner keys.
const ownerCompany = "company"

func ownerKeyForUser(userID string) string         { return "user:" + userID }
func ownerKeyForCard(cardID string) string         { return "card:" + cardID }
func ownerKeyForProvider(providerID string) string { return "provider:" + providerID }

// ProviderOwnerPrefix starts the owner key of every provider vault
// ("provider:<provider id>"), for queries that list them all.
const ProviderOwnerPrefix = "provider:"

// Service is the ledger engine. Create one with NewService and share it.
type Service struct {
	database *gorm.DB
	timeZone *time.Location
}

// NewService creates the ledger service.
func NewService(connection *gorm.DB, timeZone *time.Location) *Service {
	return &Service{database: connection, timeZone: timeZone}
}

// EnsureCompanyAccount creates a company account if it does not exist yet.
// Used by the seeder (and safe to run many times).
func (service *Service) EnsureCompanyAccount(requestContext context.Context, template CompanyAccountTemplate, assetCode string) (*models.LedgerAccount, error) {
	return service.ensureAccount(requestContext, models.LedgerAccount{
		OwnerKey:      ownerCompany,
		Code:          template.Code,
		Name:          template.Name + " " + assetCode,
		Category:      template.Category,
		NormalSide:    NormalSideForCategory(template.Category),
		AssetCode:     assetCode,
		AccountType:   "company",
		AllowNegative: true, // company accounts may go below zero (e.g. clearing)
	})
}

// EnsureProviderVault creates the vault that mirrors what one provider holds
// for Tribe in one currency, if it does not exist yet. Used by the seeder and
// when an admin adds a currency (safe to run many times).
//
// A vault may go below zero: that means we told the provider to pay more than
// we had funded with it, and an admin must top it up (Inventory shows it).
func (service *Service) EnsureProviderVault(requestContext context.Context, provider models.Provider, assetCode string) (*models.LedgerAccount, error) {
	return service.ensureAccount(requestContext, models.LedgerAccount{
		OwnerKey:      ownerKeyForProvider(provider.ID),
		Code:          CodeProviderVault,
		Name:          provider.Name + " · " + assetCode,
		Category:      CategoryAsset,
		NormalSide:    NormalSideForCategory(CategoryAsset),
		AssetCode:     assetCode,
		AccountType:   CodeProviderVault,
		AllowNegative: true,
	})
}

// EnsureUserAccount makes sure a user has a wallet account for an asset.
// IMPORTANT: call this BEFORE starting a posting transaction, never inside
// one. Creating rows inside a money transaction can cause MySQL "gap lock"
// deadlocks when many users post at once.
func (service *Service) EnsureUserAccount(requestContext context.Context, userID string, code string, assetCode string) (*models.LedgerAccount, error) {
	names := map[string]string{CodeUserPrimary: "Wallet", CodeUserReferral: "Referral earnings", CodeUserCashpoint: "Cashpoint codes"}
	userIDCopy := userID
	return service.ensureAccount(requestContext, models.LedgerAccount{
		OwnerKey:    ownerKeyForUser(userID),
		Code:        code,
		Name:        names[code] + " " + assetCode,
		Category:    CategoryLiability,
		NormalSide:  NormalSideForCategory(CategoryLiability),
		AssetCode:   assetCode,
		UserID:      &userIDCopy,
		AccountType: code,
	})
}

// EnsureCardAccount makes sure a virtual card has its own USD balance account.
func (service *Service) EnsureCardAccount(requestContext context.Context, userID string, cardID string, assetCode string) (*models.LedgerAccount, error) {
	userIDCopy, cardIDCopy := userID, cardID
	return service.ensureAccount(requestContext, models.LedgerAccount{
		OwnerKey:      ownerKeyForCard(cardID),
		Code:          CodeUserCard,
		Name:          "Virtual card " + assetCode,
		Category:      CategoryLiability,
		NormalSide:    NormalSideForCategory(CategoryLiability),
		AssetCode:     assetCode,
		UserID:        &userIDCopy,
		VirtualCardID: &cardIDCopy,
		AccountType:   CodeUserCard,
	})
}

// ensureAccount finds an account by (owner, code, asset) or creates it.
// Two requests creating the same account at once is fine: one INSERT wins,
// the other hits the UNIQUE index and simply loads the winner's row.
func (service *Service) ensureAccount(requestContext context.Context, wanted models.LedgerAccount) (*models.LedgerAccount, error) {
	existing, err := service.findAccount(service.database.WithContext(requestContext), wanted.OwnerKey, wanted.Code, wanted.AssetCode)
	if err == nil {
		return existing, nil
	}
	if !database.IsNotFound(err) {
		return nil, err
	}

	for attempt := 0; attempt < 5; attempt++ {
		wanted.ID = 0
		wanted.AccountNumber = newAccountNumber()
		wanted.Status = "active"
		wanted.CreatedAt = time.Now().UTC()
		err = service.database.WithContext(requestContext).Create(&wanted).Error
		if err == nil {
			return &wanted, nil
		}
		if !database.IsDuplicateEntry(err) {
			return nil, err
		}
		// Either someone created the same account first, or (very rarely)
		// the random account number was taken. Check which.
		existing, findError := service.findAccount(service.database.WithContext(requestContext), wanted.OwnerKey, wanted.Code, wanted.AssetCode)
		if findError == nil {
			return existing, nil
		}
	}
	return nil, fmt.Errorf("could not create ledger account %s/%s/%s", wanted.OwnerKey, wanted.Code, wanted.AssetCode)
}

// findAccount loads an account by (owner, code, asset).
func (service *Service) findAccount(query *gorm.DB, ownerKey string, code string, assetCode string) (*models.LedgerAccount, error) {
	var account models.LedgerAccount
	err := query.Where("owner_key = ? AND code = ? AND asset_code = ?", ownerKey, code, assetCode).Take(&account).Error
	if err != nil {
		return nil, err
	}
	return &account, nil
}

// ProviderVault loads one provider's vault for a currency.
func (service *Service) ProviderVault(query *gorm.DB, providerID string, assetCode string) (*models.LedgerAccount, error) {
	account, err := service.findAccount(query, ownerKeyForProvider(providerID), CodeProviderVault, assetCode)
	if err != nil {
		return nil, fmt.Errorf("provider vault %s/%s missing (run db:seed): %w", providerID, assetCode, err)
	}
	return account, nil
}

// HomeVault loads the vault of a currency's HOME provider: the provider that
// converts it (assets.conversion_provider_id). Deposits, manual credits,
// conversions and payouts of that currency use this vault.
func (service *Service) HomeVault(query *gorm.DB, assetCode string) (*models.LedgerAccount, error) {
	var asset models.Asset
	if err := query.Select("code", "conversion_provider_id").Where("code = ?", assetCode).Take(&asset).Error; err != nil {
		return nil, fmt.Errorf("asset %s: %w", assetCode, err)
	}
	if asset.ConversionProviderID == nil {
		return nil, fmt.Errorf("asset %s has no provider, so no vault", assetCode)
	}
	return service.ProviderVault(query, *asset.ConversionProviderID, assetCode)
}

// TribeFunds loads Tribe's own account for a currency.
func (service *Service) TribeFunds(query *gorm.DB, assetCode string) (*models.LedgerAccount, error) {
	return service.CompanyAccount(query, CodeTribeFunds, assetCode)
}

// CompanyAccount loads a company account, e.g. CompanyAccount(query, CodeTribeFunds, "NGN").
func (service *Service) CompanyAccount(query *gorm.DB, code string, assetCode string) (*models.LedgerAccount, error) {
	account, err := service.findAccount(query, ownerCompany, code, assetCode)
	if err != nil {
		return nil, fmt.Errorf("company account %s/%s missing (run db:seed): %w", code, assetCode, err)
	}
	return account, nil
}

// UserAccount loads a user's account (it must already exist; see EnsureUserAccount).
func (service *Service) UserAccount(query *gorm.DB, userID string, code string, assetCode string) (*models.LedgerAccount, error) {
	return service.findAccount(query, ownerKeyForUser(userID), code, assetCode)
}

// CardAccount loads a card's balance account.
func (service *Service) CardAccount(query *gorm.DB, cardID string, assetCode string) (*models.LedgerAccount, error) {
	return service.findAccount(query, ownerKeyForCard(cardID), CodeUserCard, assetCode)
}

// newAccountNumber makes a random 10-digit account number whose last digit
// is a Luhn check digit (so a typo in a number is easy to spot).
func newAccountNumber() string {
	randomNumber, _ := rand.Int(rand.Reader, big.NewInt(900_000_000))
	digits := fmt.Sprintf("%09d", randomNumber.Int64()+100_000_000)
	return digits + luhnCheckDigit(digits)
}

// luhnCheckDigit computes the Luhn check digit for a string of digits.
func luhnCheckDigit(digits string) string {
	total := 0
	double := true
	for index := len(digits) - 1; index >= 0; index-- {
		digit := int(digits[index] - '0')
		if double {
			digit *= 2
			if digit > 9 {
				digit -= 9
			}
		}
		total += digit
		double = !double
	}
	return fmt.Sprint((10 - total%10) % 10)
}
