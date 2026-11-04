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
const (
	// Accounts that belong to a user.
	CodeUserPrimary  = "user_primary"  // the user's wallet balance for one asset
	CodeUserReferral = "user_referral" // referral earnings (NGN) waiting to be withdrawn
	CodeUserCard     = "user_card"     // the balance loaded on one virtual card (USD)

	// Accounts that belong to the company (one per asset).
	CodeCompanyVault        = "company_vault"        // money the company holds at providers and banks
	CodeTreasuryFunding     = "treasury_funding"     // money the owners put into the business
	CodeSwapClearing        = "swap_clearing"        // assets bought/sold during swaps, waiting for settlement
	CodePayoutInTransit     = "payout_in_transit"    // money sent out that a provider has not confirmed yet
	CodePurchaseInTransit   = "purchase_in_transit"  // bills and gift cards paid for, not yet confirmed by the provider
	CodeFeeRevenue          = "fee_revenue"          // fees we charge
	CodeFxMarginRevenue     = "fx_margin_revenue"    // the difference between the Naira we receive and pay on the NGN rail
	CodeProviderExpense     = "provider_expense"     // what providers charge us
	CodeReferralExpense     = "referral_expense"     // money we pay users for inviting friends
	CodeCustomerAdjustments = "customer_adjustments" // manual credits (and debits back) admins make to customer wallets
	CodeCashpointEscrow     = "cashpoint_escrow"     // Naira held for cashpoint codes not yet redeemed
	CodeSummary             = "summary"              // used when closing revenue and expenses at period end
)

// CompanyAccountTemplate describes one company account created for every asset.
type CompanyAccountTemplate struct {
	Code     string
	Name     string
	Category string
}

// CompanyAccountTemplates is the company chart of accounts. The seeder
// creates one of each for every asset.
var CompanyAccountTemplates = []CompanyAccountTemplate{
	{CodeCompanyVault, "Company Vault", CategoryAsset},
	{CodeTreasuryFunding, "Treasury Funding", CategoryEquity},
	{CodeSwapClearing, "Swap Clearing", CategorySuspenseLiability},
	{CodePayoutInTransit, "Payouts In Transit", CategorySuspenseLiability},
	{CodePurchaseInTransit, "Purchases In Transit", CategorySuspenseLiability},
	{CodeFeeRevenue, "Fee Revenue", CategoryRevenue},
	{CodeFxMarginRevenue, "FX Margin Revenue", CategoryRevenue},
	{CodeProviderExpense, "Provider Expense", CategoryExpense},
	{CodeReferralExpense, "Referral Expense", CategoryExpense},
	{CodeCustomerAdjustments, "Customer Adjustments", CategoryExpense},
	{CodeCashpointEscrow, "Cashpoint Escrow", CategoryLiability},
	{CodeSummary, "Income Summary", CategorySummary},
}

// PurchaseInTransitTemplate is the template of Purchases In Transit, so
// services can open it on databases seeded before it existed.
func PurchaseInTransitTemplate() CompanyAccountTemplate {
	for _, template := range CompanyAccountTemplates {
		if template.Code == CodePurchaseInTransit {
			return template
		}
	}
	panic("purchase_in_transit is missing from CompanyAccountTemplates")
}

// Owner keys.
const ownerCompany = "company"

func ownerKeyForUser(userID string) string { return "user:" + userID }
func ownerKeyForCard(cardID string) string { return "card:" + cardID }

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

// EnsureUserAccount makes sure a user has a wallet account for an asset.
// IMPORTANT: call this BEFORE starting a posting transaction, never inside
// one. Creating rows inside a money transaction can cause MySQL "gap lock"
// deadlocks when many users post at once.
func (service *Service) EnsureUserAccount(requestContext context.Context, userID string, code string, assetCode string) (*models.LedgerAccount, error) {
	names := map[string]string{CodeUserPrimary: "Wallet", CodeUserReferral: "Referral earnings"}
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

// CompanyAccount loads a company account, e.g. CompanyAccount(query, CodeFeeRevenue, "NGN").
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
