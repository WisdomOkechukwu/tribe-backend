package backoffice

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/tribe-africa/tribe-backend/app/apperrors"
	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/app/services/adminauth"
	"github.com/tribe-africa/tribe-backend/app/services/ledger"
	"github.com/tribe-africa/tribe-backend/app/services/shared"
	"github.com/tribe-africa/tribe-backend/database"
	"github.com/tribe-africa/tribe-backend/pkg/money"
)

// TransactionFilters filter the admin transactions list.
type TransactionFilters struct {
	Category   string
	Type       string
	Status     string
	Asset      string
	Search     string
	CustomerID string
	From       string // YYYY-MM-DD (Lagos)
	To         string
	Page       int
	// ShowCustomers adds customer names to the rows. Only admins with
	// customers.view may see who made a transaction.
	ShowCustomers bool
}

// ListTransactions returns transactions for the admin.
func (service *Service) ListTransactions(requestContext context.Context, filters TransactionFilters) (Page[TransactionRow], error) {
	query := service.database.WithContext(requestContext).Model(&models.Transaction{})
	if filters.CustomerID != "" {
		query = query.Where("user_id = ?", filters.CustomerID)
	}
	if filters.Category != "" {
		query = query.Where("category = ?", filters.Category)
	}
	if filters.Type != "" {
		query = query.Where("type = ?", filters.Type)
	}
	if filters.Status != "" {
		query = query.Where("status = ?", filters.Status)
	}
	if filters.Asset != "" {
		query = query.Where("(from_asset = ? OR to_asset = ?)", filters.Asset, filters.Asset)
	}
	if search := strings.TrimSpace(filters.Search); search != "" {
		query = query.Where("reference LIKE ? OR id = ? OR title LIKE ?", "%"+search+"%", search, "%"+search+"%")
	}
	if from, err := time.ParseInLocation(time.DateOnly, filters.From, service.settings.App.TimeZone); err == nil {
		query = query.Where("created_at >= ?", from.UTC())
	}
	if to, err := time.ParseInLocation(time.DateOnly, filters.To, service.settings.App.TimeZone); err == nil {
		query = query.Where("created_at < ?", to.AddDate(0, 0, 1).UTC())
	}
	var total int64
	query.Count(&total)
	var transactions []models.Transaction
	if err := query.Order("created_at DESC").Offset(offset(filters.Page)).Limit(PageSize).Find(&transactions).Error; err != nil {
		return Page[TransactionRow]{}, err
	}
	return newPage(service.withCustomers(service.database.WithContext(requestContext), transactions, filters.ShowCustomers), total, filters.Page), nil
}

// TransactionRow is a transaction with its customer's name and tag, so the
// admin list does not have to look each customer up separately.
type TransactionRow struct {
	models.Transaction
	CustomerName string `json:"customer_name"`
	CustomerTag  string `json:"customer_tag"`
}

// withCustomers adds customer names to transactions with ONE extra query
// (WHERE id IN ...) instead of one query per row. When showCustomers is
// false (the admin lacks customers.view) the names stay empty.
func (service *Service) withCustomers(query *gorm.DB, transactions []models.Transaction, showCustomers bool) []TransactionRow {
	userIDs := make([]string, 0, len(transactions))
	for _, transaction := range transactions {
		if showCustomers {
			userIDs = append(userIDs, transaction.UserID)
		}
	}
	usersByID := map[string]models.User{}
	if len(userIDs) > 0 {
		var users []models.User
		query.Where("id IN ?", userIDs).Find(&users)
		for _, user := range users {
			usersByID[user.ID] = user
		}
	}
	rows := make([]TransactionRow, 0, len(transactions))
	for _, transaction := range transactions {
		row := TransactionRow{Transaction: transaction}
		if user, found := usersByID[transaction.UserID]; found {
			row.CustomerName, row.CustomerTag = user.FullName(), user.Tag
		}
		rows = append(rows, row)
	}
	return rows
}

// JournalLegView is a journal leg with its account, for display.
type JournalLegView struct {
	models.JournalLeg
	AccountNumber   string `json:"account_number"`
	AccountName     string `json:"account_name"`
	AccountCategory string `json:"account_category"`
	AmountDecimal   string `json:"amount_decimal"`
}

// JournalEntryView is a journal entry with its legs.
type JournalEntryView struct {
	Entry models.JournalEntry `json:"entry"`
	Legs  []JournalLegView    `json:"legs"`
}

// TransactionDetail is the admin transaction page.
type TransactionDetail struct {
	Transaction models.Transaction `json:"transaction"`
	Customer    *CustomerRow       `json:"customer"`
	Journal     []JournalEntryView `json:"journal"`
	SwapLegs    []models.SwapLeg   `json:"swap_legs"`
}

// GetTransaction returns a transaction with every journal entry and leg.
func (service *Service) GetTransaction(requestContext context.Context, admin *adminauth.Admin, transactionID string) (*TransactionDetail, error) {
	query := service.database.WithContext(requestContext)
	var transaction models.Transaction
	if err := query.Where("id = ? OR reference = ?", transactionID, transactionID).Take(&transaction).Error; err != nil {
		return nil, apperrors.NotFound("Transaction")
	}
	detail := &TransactionDetail{Transaction: transaction, Journal: []JournalEntryView{}, SwapLegs: []models.SwapLeg{}}
	var user models.User
	if query.Where("id = ?", transaction.UserID).Take(&user).Error == nil {
		row := customerRow(user, admin.Can("customers.pii"))
		detail.Customer = &row
	}
	var entries []models.JournalEntry
	query.Where("business_transaction_id = ?", transaction.ID).Order("posted_at").Find(&entries)
	for _, entry := range entries {
		view, err := service.journalEntryView(query, entry)
		if err != nil {
			return nil, err
		}
		detail.Journal = append(detail.Journal, *view)
	}
	query.Where("transaction_id = ?", transaction.ID).Order("step").Find(&detail.SwapLegs)
	return detail, nil
}

// GetJournalEntry returns one journal entry with legs.
func (service *Service) GetJournalEntry(requestContext context.Context, entryID string) (*JournalEntryView, error) {
	query := service.database.WithContext(requestContext)
	var entry models.JournalEntry
	if err := query.Where("id = ? OR reference = ?", entryID, entryID).Take(&entry).Error; err != nil {
		return nil, apperrors.NotFound("Journal entry")
	}
	return service.journalEntryView(query, entry)
}

// journalEntryView loads the legs of an entry with account names.
func (service *Service) journalEntryView(query *gorm.DB, entry models.JournalEntry) (*JournalEntryView, error) {
	decimals := shared.AssetDecimals(query)
	var legs []models.JournalLeg
	if err := query.Where("journal_entry_id = ?", entry.ID).Order("id").Find(&legs).Error; err != nil {
		return nil, err
	}
	view := &JournalEntryView{Entry: entry, Legs: make([]JournalLegView, 0, len(legs))}
	for _, leg := range legs {
		var account models.LedgerAccount
		query.Where("id = ?", leg.LedgerAccountID).Take(&account)
		amount, _ := money.ParseUnits(leg.Amount)
		view.Legs = append(view.Legs, JournalLegView{JournalLeg: leg, AccountNumber: account.AccountNumber, AccountName: account.Name,
			AccountCategory: account.Category, AmountDecimal: amount.Decimal(decimals[leg.AssetCode])})
	}
	return view, nil
}

// ─── Ledger ──────────────────────────────────────────────────────────

// LedgerAccountView is a ledger account with its balance.
type LedgerAccountView struct {
	models.LedgerAccount
	Balance string `json:"balance"`
}

// ListLedgerAccounts returns company accounts (or one user's) with balances.
func (service *Service) ListLedgerAccounts(requestContext context.Context, ownerFilter string, assetCode string) ([]LedgerAccountView, error) {
	query := service.database.WithContext(requestContext)
	switch {
	case ownerFilter == "" || ownerFilter == "company":
		query = query.Where("owner_key = ?", "company")
	default:
		query = query.Where("user_id = ?", ownerFilter)
	}
	if assetCode != "" {
		query = query.Where("asset_code = ?", assetCode)
	}
	var accounts []models.LedgerAccount
	if err := query.Order("asset_code, code").Find(&accounts).Error; err != nil {
		return nil, err
	}
	balances, err := service.ledger.BalancesForAccounts(requestContext, accounts)
	if err != nil {
		return nil, err
	}
	decimals := shared.AssetDecimals(service.database.WithContext(requestContext))
	views := make([]LedgerAccountView, 0, len(balances))
	for _, balance := range balances {
		views = append(views, LedgerAccountView{LedgerAccount: balance.Account, Balance: balance.Balance.Decimal(decimals[balance.Account.AssetCode])})
	}
	return views, nil
}

// AccountLegs lists the legs posted to one account (newest first).
func (service *Service) AccountLegs(requestContext context.Context, accountID uint64, page int) (Page[JournalLegView], error) {
	query := service.database.WithContext(requestContext)
	var account models.LedgerAccount
	if err := query.Where("id = ?", accountID).Take(&account).Error; err != nil {
		return Page[JournalLegView]{}, apperrors.NotFound("Ledger account")
	}
	decimals := shared.AssetDecimals(query)
	var total int64
	query.Model(&models.JournalLeg{}).Where("ledger_account_id = ?", accountID).Count(&total)
	var legs []models.JournalLeg
	if err := query.Where("ledger_account_id = ?", accountID).Order("id DESC").Offset(offset(page)).Limit(PageSize).Find(&legs).Error; err != nil {
		return Page[JournalLegView]{}, err
	}
	views := make([]JournalLegView, 0, len(legs))
	for _, leg := range legs {
		amount, _ := money.ParseUnits(leg.Amount)
		views = append(views, JournalLegView{JournalLeg: leg, AccountNumber: account.AccountNumber, AccountName: account.Name, AccountCategory: account.Category, AmountDecimal: amount.Decimal(decimals[leg.AssetCode])})
	}
	return newPage(views, total, page), nil
}

// SnapshotView is a daily snapshot with readable numbers.
type SnapshotView struct {
	models.LedgerDailySnapshot
	AccountNumber string `json:"account_number"`
	AccountName   string `json:"account_name"`
	AssetCode     string `json:"asset_code"`
	Closing       string `json:"closing"`
}

// ListSnapshots returns the daily snapshots for a day (default: latest day).
func (service *Service) ListSnapshots(requestContext context.Context, date string) ([]SnapshotView, string, error) {
	query := service.database.WithContext(requestContext)
	if date == "" {
		var latest sql.NullTime
		query.Raw("SELECT MAX(snapshot_date) FROM ledger_daily_snapshots").Scan(&latest)
		if !latest.Valid {
			return []SnapshotView{}, "", nil
		}
		date = latest.Time.Format(time.DateOnly)
	}
	decimals := shared.AssetDecimals(query)
	var snapshots []models.LedgerDailySnapshot
	if err := query.Where("snapshot_date = ?", date).Order("ledger_account_id").Limit(1000).Find(&snapshots).Error; err != nil {
		return nil, date, err
	}
	// Load all the accounts in one query instead of one per snapshot.
	accountIDs := make([]uint64, 0, len(snapshots))
	for _, snapshot := range snapshots {
		accountIDs = append(accountIDs, snapshot.LedgerAccountID)
	}
	accountByID := map[uint64]models.LedgerAccount{}
	if len(accountIDs) > 0 {
		var accounts []models.LedgerAccount
		if err := query.Where("id IN ?", accountIDs).Find(&accounts).Error; err != nil {
			return nil, date, err
		}
		for _, account := range accounts {
			accountByID[account.ID] = account
		}
	}
	views := make([]SnapshotView, 0, len(snapshots))
	for _, snapshot := range snapshots {
		account := accountByID[snapshot.LedgerAccountID]
		closing, _ := money.ParseUnits(snapshot.ClosingBalance)
		views = append(views, SnapshotView{LedgerDailySnapshot: snapshot, AccountNumber: account.AccountNumber, AccountName: account.Name, AssetCode: account.AssetCode, Closing: closing.Decimal(decimals[account.AssetCode])})
	}
	return views, date, nil
}

// Inventory returns the company vault of each asset with its balance (the admin "Inventory" page).
//
// Only the three account kinds it shows are loaded and summed (before, every
// company account was summed and most were thrown away).
func (service *Service) Inventory(requestContext context.Context) ([]LedgerAccountView, error) {
	query := service.database.WithContext(requestContext)
	var accounts []models.LedgerAccount
	err := query.Where("owner_key = ? AND code IN ?", "company",
		[]string{ledger.CodeCompanyVault, ledger.CodeCashpointEscrow, ledger.CodePayoutInTransit, ledger.CodePurchaseInTransit}).
		Order("asset_code, code").Find(&accounts).Error
	if err != nil {
		return nil, err
	}
	balances, err := service.ledger.BalancesForAccounts(requestContext, accounts)
	if err != nil {
		return nil, err
	}
	decimals := shared.AssetDecimals(query)
	vaults := make([]LedgerAccountView, 0, len(balances))
	for _, balance := range balances {
		vaults = append(vaults, LedgerAccountView{LedgerAccount: balance.Account, Balance: balance.Balance.Decimal(decimals[balance.Account.AssetCode])})
	}
	return vaults, nil
}

// TreasuryFundingView is a funding request with its amount in readable form.
type TreasuryFundingView struct {
	models.TreasuryFunding
	AmountDecimal string `json:"amount_decimal"`
}

// ListTreasuryFundings returns funding requests, newest first.
// status filters to pending, approved or rejected (empty = all).
func (service *Service) ListTreasuryFundings(requestContext context.Context, status string, page int) (Page[TreasuryFundingView], error) {
	query := service.database.WithContext(requestContext)
	decimals := shared.AssetDecimals(query)
	filtered := query.Model(&models.TreasuryFunding{})
	if status != "" {
		filtered = filtered.Where("status = ?", status)
	}
	var total int64
	filtered.Count(&total)
	var fundings []models.TreasuryFunding
	if err := filtered.Order("created_at DESC").Offset(offset(page)).Limit(PageSize).Find(&fundings).Error; err != nil {
		return Page[TreasuryFundingView]{}, err
	}
	views := make([]TreasuryFundingView, 0, len(fundings))
	for _, funding := range fundings {
		views = append(views, NewTreasuryFundingView(funding, decimals[funding.AssetCode]))
	}
	return newPage(views, total, page), nil
}

// NewTreasuryFundingView adds the readable amount to a funding row.
func NewTreasuryFundingView(funding models.TreasuryFunding, decimalPlaces int) TreasuryFundingView {
	amount, _ := money.ParseUnits(funding.Amount)
	return TreasuryFundingView{TreasuryFunding: funding, AmountDecimal: amount.Decimal(decimalPlaces)}
}

// CustomerAdjustmentView is a manual credit or debit request with its amount in readable
// form and who it is for.
type CustomerAdjustmentView struct {
	models.CustomerAdjustment
	AmountDecimal string `json:"amount_decimal"`
	CustomerName  string `json:"customer_name"`
	CustomerTag   string `json:"customer_tag"`
}

// ListCustomerAdjustments returns manual credit and debit requests, newest first.
// status filters to pending, approved or rejected, kind to credit or debit; customerID to one customer
// (empty = all).
func (service *Service) ListCustomerAdjustments(requestContext context.Context, status string, kind string, customerID string, page int) (Page[CustomerAdjustmentView], error) {
	query := service.database.WithContext(requestContext)
	decimals := shared.AssetDecimals(query)
	filtered := query.Model(&models.CustomerAdjustment{})
	if status != "" {
		filtered = filtered.Where("status = ?", status)
	}
	if kind != "" {
		filtered = filtered.Where("kind = ?", kind)
	}
	if customerID != "" {
		filtered = filtered.Where("user_id = ?", customerID)
	}
	var total int64
	filtered.Count(&total)
	var adjustments []models.CustomerAdjustment
	if err := filtered.Order("created_at DESC").Offset(offset(page)).Limit(PageSize).Find(&adjustments).Error; err != nil {
		return Page[CustomerAdjustmentView]{}, err
	}
	// one query for every customer on the page, not one per row
	userIDs := make([]string, 0, len(adjustments))
	for _, adjustment := range adjustments {
		userIDs = append(userIDs, adjustment.UserID)
	}
	var users []models.User
	if len(userIDs) > 0 {
		if err := query.Where("id IN ?", userIDs).Find(&users).Error; err != nil {
			return Page[CustomerAdjustmentView]{}, err
		}
	}
	usersByID := map[string]models.User{}
	for _, user := range users {
		usersByID[user.ID] = user
	}
	views := make([]CustomerAdjustmentView, 0, len(adjustments))
	for _, adjustment := range adjustments {
		views = append(views, NewCustomerAdjustmentView(adjustment, usersByID[adjustment.UserID], decimals[adjustment.AssetCode]))
	}
	return newPage(views, total, page), nil
}

// NewCustomerAdjustmentView adds the readable amount and the customer's name to an adjustment row.
func NewCustomerAdjustmentView(adjustment models.CustomerAdjustment, user models.User, decimalPlaces int) CustomerAdjustmentView {
	amount, _ := money.ParseUnits(adjustment.Amount)
	return CustomerAdjustmentView{
		CustomerAdjustment: adjustment, AmountDecimal: amount.Decimal(decimalPlaces),
		CustomerName: user.FirstName + " " + user.LastName, CustomerTag: user.Tag,
	}
}

// FreeFundsView is one currency's free funds, readable.
type FreeFundsView struct {
	AssetCode string `json:"asset_code"`
	Vault     string `json:"vault"`
	Owed      string `json:"owed"`
	Free      string `json:"free"` // may be negative when the vault is short
}

// FreeFunds returns, for every enabled asset, the vault, what we owe
// customers and the difference admins may credit. It reads every currency
// with a fixed number of queries (see ledger.FreeFundsForAssets), and gives
// the same numbers as ledger.FreeFundsOf would one asset at a time.
func (service *Service) FreeFunds(requestContext context.Context) ([]FreeFundsView, error) {
	query := service.database.WithContext(requestContext)
	var assets []models.Asset
	if err := query.Where("enabled = ?", true).Order("display_order").Find(&assets).Error; err != nil {
		return nil, err
	}
	assetCodes := make([]string, 0, len(assets))
	for _, asset := range assets {
		assetCodes = append(assetCodes, asset.Code)
	}
	freeByAsset, err := service.ledger.FreeFundsForAssets(query, assetCodes)
	if err != nil {
		return nil, err
	}
	views := make([]FreeFundsView, 0, len(assets))
	for _, asset := range assets {
		free := freeByAsset[asset.Code]
		views = append(views, FreeFundsView{
			AssetCode: asset.Code, Vault: free.Vault.Decimal(asset.DecimalPlaces),
			Owed: free.Owed.Decimal(asset.DecimalPlaces), Free: free.Free.Decimal(asset.DecimalPlaces),
		})
	}
	return views, nil
}

// InventoryEntries returns the journal legs on the company vault accounts
// (every movement in or out of the company's own money), newest first.
// assetCode narrows it to one currency.
func (service *Service) InventoryEntries(requestContext context.Context, assetCode string, page int) (Page[JournalLegView], error) {
	query := service.database.WithContext(requestContext)
	accountsQuery := query.Model(&models.LedgerAccount{}).Where("user_id IS NULL AND code = ?", ledger.CodeCompanyVault)
	if assetCode != "" {
		accountsQuery = accountsQuery.Where("asset_code = ?", assetCode)
	}
	var vaults []models.LedgerAccount
	if err := accountsQuery.Find(&vaults).Error; err != nil {
		return Page[JournalLegView]{}, err
	}
	if len(vaults) == 0 {
		return newPage([]JournalLegView{}, 0, page), nil
	}
	vaultsByID := map[uint64]models.LedgerAccount{}
	vaultIDs := make([]uint64, 0, len(vaults))
	for _, vault := range vaults {
		vaultsByID[vault.ID] = vault
		vaultIDs = append(vaultIDs, vault.ID)
	}
	decimals := shared.AssetDecimals(query)
	var total int64
	query.Model(&models.JournalLeg{}).Where("ledger_account_id IN ?", vaultIDs).Count(&total)
	var legs []models.JournalLeg
	if err := query.Where("ledger_account_id IN ?", vaultIDs).Order("id DESC").Offset(offset(page)).Limit(PageSize).Find(&legs).Error; err != nil {
		return Page[JournalLegView]{}, err
	}
	views := make([]JournalLegView, 0, len(legs))
	for _, leg := range legs {
		vault := vaultsByID[leg.LedgerAccountID]
		amount, _ := money.ParseUnits(leg.Amount)
		views = append(views, JournalLegView{JournalLeg: leg, AccountNumber: vault.AccountNumber, AccountName: vault.Name, AccountCategory: vault.Category, AmountDecimal: amount.Decimal(decimals[leg.AssetCode])})
	}
	return newPage(views, total, page), nil
}

// ─── Rates, margins, fees, providers ─────────────────────────────────

// RateHistory returns recent rates for one asset (for charts).
func (service *Service) RateHistory(requestContext context.Context, assetCode string, hours int) ([]models.ExchangeRate, error) {
	if hours <= 0 || hours > 24*30 {
		hours = 24
	}
	var history []models.ExchangeRate
	err := service.database.WithContext(requestContext).
		Where("asset_code = ? AND collected_at > ?", assetCode, time.Now().UTC().Add(-time.Duration(hours)*time.Hour)).
		Order("collected_at").Limit(5000).Find(&history).Error
	return history, err
}

// ListMargins returns every asset's buy/sell margins.
func (service *Service) ListMargins(requestContext context.Context) ([]models.RateMargin, error) {
	var margins []models.RateMargin
	err := service.database.WithContext(requestContext).Order("asset_code").Find(&margins).Error
	return margins, err
}

// SaveMargin changes an asset's margins (lock + version + audit).
func (service *Service) SaveMargin(requestContext context.Context, admin *adminauth.Admin, change models.RateMargin) (*models.RateMargin, error) {
	for _, text := range []string{change.BuyMarginPercent, change.SellMarginPercent} {
		value, err := money.ParseRate(text)
		if err != nil || value.Sign() < 0 || value.Cmp(bigTwentyFive) > 0 {
			return nil, apperrors.Validation(map[string][]string{"margin": {"Margins must be between 0 and 25 percent."}})
		}
	}
	var before, after models.RateMargin
	err := database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		if err := database.LockForUpdate(databaseTransaction, &before, "asset_code = ?", change.AssetCode); err != nil {
			return apperrors.NotFound("Margin for " + change.AssetCode)
		}
		if change.Version != 0 && change.Version != before.Version {
			return apperrors.ErrStaleData
		}
		if err := databaseTransaction.Model(&models.RateMargin{}).Where("asset_code = ?", change.AssetCode).Updates(map[string]any{
			"buy_margin_percent": change.BuyMarginPercent, "sell_margin_percent": change.SellMarginPercent,
			"version": before.Version + 1, "updated_by": admin.Name, "updated_at": time.Now().UTC(),
		}).Error; err != nil {
			return err
		}
		return databaseTransaction.Where("asset_code = ?", change.AssetCode).Take(&after).Error
	})
	if err == nil {
		service.rates.ConfigChanged(requestContext) // every program reloads within a second
	}
	if err == nil {
		service.adminAuth.Audit(requestContext, admin, "rates.margin", change.AssetCode, "Changed margins", before, after)
	}
	return &after, err
}

// ListFees returns every transaction fee rule.
func (service *Service) ListFees(requestContext context.Context) ([]models.TransactionFee, error) {
	var fees []models.TransactionFee
	err := service.database.WithContext(requestContext).Order("category, transaction_type").Find(&fees).Error
	return fees, err
}

// SaveFee changes one fee rule (lock + version + audit).
func (service *Service) SaveFee(requestContext context.Context, admin *adminauth.Admin, change models.TransactionFee) (*models.TransactionFee, error) {
	percent, err := money.ParseRate(change.FeePercent)
	if err != nil || percent.Sign() < 0 || percent.Cmp(bigTen) > 0 {
		return nil, apperrors.Validation(map[string][]string{"fee_percent": {"Fee % must be between 0 and 10."}})
	}
	if flat, err := money.ParseRate(change.FlatFee); err != nil || flat.Sign() < 0 {
		return nil, apperrors.Validation(map[string][]string{"flat_fee": {"Enter a valid flat fee."}})
	}
	var before, after models.TransactionFee
	err = database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		if err := database.LockForUpdate(databaseTransaction, &before, "transaction_type = ?", change.TransactionType); err != nil {
			return apperrors.NotFound("Fee rule")
		}
		if change.Version != 0 && change.Version != before.Version {
			return apperrors.ErrStaleData
		}
		if err := databaseTransaction.Model(&models.TransactionFee{}).Where("transaction_type = ?", change.TransactionType).Updates(map[string]any{
			"fee_percent": change.FeePercent, "flat_fee": change.FlatFee, "flat_fee_asset": change.FlatFeeAsset,
			"maximum_fee": change.MaximumFee, "active": change.Active,
			"version": before.Version + 1, "updated_by": admin.Name, "updated_at": time.Now().UTC(),
		}).Error; err != nil {
			return err
		}
		return databaseTransaction.Where("transaction_type = ?", change.TransactionType).Take(&after).Error
	})
	if err == nil {
		service.rates.ConfigChanged(requestContext) // every program reloads within a second
	}
	if err == nil {
		service.adminAuth.Audit(requestContext, admin, "fees.update", change.TransactionType, "Changed fee rule", before, after)
	}
	return &after, err
}

// ProviderView is a provider with the assets it handles.
type ProviderView struct {
	models.Provider
	Assets []string `json:"assets"`
}

// ListProviders returns every provider.
func (service *Service) ListProviders(requestContext context.Context) ([]ProviderView, error) {
	query := service.database.WithContext(requestContext)
	var providerRows []models.Provider
	if err := query.Order("category, name").Find(&providerRows).Error; err != nil {
		return nil, err
	}
	views := make([]ProviderView, 0, len(providerRows))
	for _, provider := range providerRows {
		var assets []string
		query.Model(&models.ProviderAsset{}).Where("provider_id = ?", provider.ID).Order("asset_code").Pluck("asset_code", &assets)
		if assets == nil {
			assets = []string{}
		}
		views = append(views, ProviderView{Provider: provider, Assets: assets})
	}
	return views, nil
}

// ProviderChanges are the switches an admin can flip.
type ProviderChanges struct {
	UsesNgnRail              *bool
	SupportsDirectConversion *bool
	Enabled                  *bool
	Version                  int
}

// UpdateProvider flips provider switches, including the NGN RAIL.
// Turning uses_ngn_rail ON makes every conversion through this provider go
// FROM -> NGN -> TO (see rates.decideRoute).
func (service *Service) UpdateProvider(requestContext context.Context, admin *adminauth.Admin, providerID string, changes ProviderChanges) (*models.Provider, error) {
	var before, after models.Provider
	err := database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		if err := database.LockForUpdate(databaseTransaction, &before, "id = ?", providerID); err != nil {
			return apperrors.NotFound("Provider")
		}
		if changes.Version != 0 && changes.Version != before.Version {
			return apperrors.ErrStaleData
		}
		updates := map[string]any{"version": before.Version + 1, "updated_at": time.Now().UTC()}
		if changes.UsesNgnRail != nil {
			updates["uses_ngn_rail"] = *changes.UsesNgnRail
		}
		if changes.SupportsDirectConversion != nil {
			updates["supports_direct_conversion"] = *changes.SupportsDirectConversion
		}
		if changes.Enabled != nil {
			updates["enabled"] = *changes.Enabled
		}
		if err := databaseTransaction.Model(&models.Provider{}).Where("id = ?", providerID).Updates(updates).Error; err != nil {
			return err
		}
		return databaseTransaction.Where("id = ?", providerID).Take(&after).Error
	})
	if err == nil {
		service.rates.ConfigChanged(requestContext) // every program reloads within a second
	}
	if err == nil {
		service.adminAuth.Audit(requestContext, admin, "providers.update", before.Slug, "Changed provider settings", before, after)
	}
	return &after, err
}
