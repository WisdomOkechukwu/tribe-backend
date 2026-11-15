package backoffice

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/tribe-africa/tribe-backend/app/apperrors"
	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/app/services/adminauth"
	"github.com/tribe-africa/tribe-backend/app/services/ledger"
	"github.com/tribe-africa/tribe-backend/app/services/providers"
	"github.com/tribe-africa/tribe-backend/database"
)

// ─── Adding and switching currencies ─────────────────────────────────
//
// An admin can add a currency or coin from the admin panel instead of a
// developer adding it to the seeder. A currency needs four things to work,
// and CreateCurrency makes all of them:
//
//  1. its row in `assets` (code, name, decimals, the provider that converts
//     it, and where its live price comes from);
//  2. a `rate_margins` row (the buy/sell margin, starting at zero);
//  3. a `provider_assets` row (which provider handles it);
//  4. its ledger accounts: Tribe Funds and the provider's vault for it.
//     Without these no money in that currency could ever be posted.
//
// A new currency starts SWITCHED OFF. Nothing can use it until an admin
// switches it on (UpdateCurrency), which first makes sure the accounts exist
// and that a live price can be fetched.

// ErrCurrencyExists means a currency with that code is already there.
var ErrCurrencyExists = apperrors.New(http.StatusConflict, "CURRENCY_EXISTS", "A currency with that code already exists.")

// currencyCode is what a currency code may look like: 2 to 10 capital
// letters or digits (NGN, USDT, BTC).
var currencyCode = regexp.MustCompile(`^[A-Z0-9]{2,10}$`)

// maximumDecimalPlaces is the most decimal places a new currency may have.
// Eight is what Bitcoin uses, and the most this system has been run with.
const maximumDecimalPlaces = 8

// alwaysOnCurrencies cannot be switched off: Naira is the rail conversions
// and bills go through, and the US dollar is what cards and gift cards are
// priced in. Switching either off would stop far more than that currency.
var alwaysOnCurrencies = map[string]string{
	"NGN": "Naira cannot be switched off: bills, cashpoints and conversions run on it.",
	"USD": "The US dollar cannot be switched off: cards and gift cards are priced in it.",
}

// CurrencyInput is what an admin fills in to add a currency.
type CurrencyInput struct {
	Code                 string
	Name                 string
	Kind                 string // fiat | crypto
	Symbol               string // "" = use the code
	DecimalPlaces        int
	ConversionProviderID string
	PriceSource          string // fiat_feed | coinbase | usd_peg
}

// CreateCurrency adds a currency, switched off, with everything it needs
// (see the list at the top of this file). It returns 409 CURRENCY_EXISTS
// when the code is taken.
func (service *Service) CreateCurrency(requestContext context.Context, admin *adminauth.Admin, input CurrencyInput) (*models.Asset, error) {
	input.Code = strings.ToUpper(strings.TrimSpace(input.Code))
	input.Name = strings.TrimSpace(input.Name)
	input.Symbol = strings.TrimSpace(input.Symbol)
	if input.Symbol == "" {
		input.Symbol = input.Code
	}
	problems := map[string][]string{}
	if !currencyCode.MatchString(input.Code) {
		problems["code"] = []string{"Use 2 to 10 capital letters or digits, for example ZAR or DOGE."}
	}
	if input.Name == "" || len(input.Name) > 64 {
		problems["name"] = []string{"Enter a name of up to 64 characters."}
	}
	if len(input.Symbol) > 8 {
		problems["symbol"] = []string{"Keep the symbol to 8 characters or fewer."}
	}
	if input.Kind != "fiat" && input.Kind != "crypto" {
		problems["kind"] = []string{"Choose fiat or crypto."}
	}
	if input.DecimalPlaces < 0 || input.DecimalPlaces > maximumDecimalPlaces {
		problems["decimal_places"] = []string{"Decimal places must be between 0 and 8."}
	}
	switch {
	case !providers.IsPriceSource(input.PriceSource):
		problems["price_source"] = []string{"Choose where the price comes from: fiat_feed, coinbase or usd_peg."}
	case input.Kind == "fiat" && input.PriceSource == providers.PriceSourceCoinbase:
		problems["price_source"] = []string{"A fiat currency is priced from the fiat feed (or pegged to the dollar)."}
	case input.Kind == "crypto" && input.PriceSource == providers.PriceSourceFiatFeed:
		problems["price_source"] = []string{"A coin is priced from Coinbase (or pegged to the dollar)."}
	}
	if len(problems) > 0 {
		return nil, apperrors.Validation(problems)
	}
	var provider models.Provider
	if err := service.database.WithContext(requestContext).Where("id = ?", input.ConversionProviderID).Take(&provider).Error; err != nil {
		return nil, apperrors.Validation(map[string][]string{"conversion_provider_id": {"Choose the provider that converts this currency."}})
	}
	// Refuse a currency we could never show a rate for, before anything is saved.
	if err := service.rates.CheckPriceSource(requestContext, input.Code, input.PriceSource); err != nil {
		return nil, apperrors.Validation(map[string][]string{"price_source": {"We cannot price " + input.Code + " from that source: " + err.Error() + "."}})
	}

	now := time.Now().UTC()
	asset := models.Asset{
		Code: input.Code, Name: input.Name, Kind: input.Kind, Symbol: input.Symbol, DecimalPlaces: input.DecimalPlaces,
		ConversionProviderID: &provider.ID, PriceSource: input.PriceSource,
		Enabled: false, Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	err := database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		var existing models.Asset
		lookupError := databaseTransaction.Where("code = ?", asset.Code).Take(&existing).Error
		if lookupError == nil {
			return ErrCurrencyExists
		}
		if !database.IsNotFound(lookupError) {
			return lookupError
		}
		// The new currency goes last in every list.
		var lastOrder int
		if err := databaseTransaction.Model(&models.Asset{}).Select("COALESCE(MAX(display_order), -1)").Scan(&lastOrder).Error; err != nil {
			return err
		}
		asset.DisplayOrder = lastOrder + 1
		err := databaseTransaction.Create(&asset).Error
		// Two admins adding the same code at the same moment: the primary
		// key stops the second insert.
		if database.IsDuplicateEntry(err) {
			return ErrCurrencyExists
		}
		if err != nil {
			return err
		}
		// Margins start at zero: an admin sets them on Rates & pricing.
		margin := models.RateMargin{AssetCode: asset.Code, BuyMarginPercent: "0", SellMarginPercent: "0", Version: 1, UpdatedBy: admin.Email, UpdatedAt: now}
		if err := databaseTransaction.Create(&margin).Error; err != nil && !database.IsDuplicateEntry(err) {
			return err
		}
		providerAsset := models.ProviderAsset{ProviderID: provider.ID, AssetCode: asset.Code}
		if err := databaseTransaction.Create(&providerAsset).Error; err != nil && !database.IsDuplicateEntry(err) {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// The ledger accounts are made after the row is saved, and safely more
	// than once: if this step fails, switching the currency on makes them
	// again (UpdateCurrency), and until then the currency stays off.
	if err := service.ensureCompanyAccounts(requestContext, asset.Code); err != nil {
		return nil, err
	}
	service.rates.ConfigChanged(requestContext) // every program reloads within a second
	service.adminAuth.Audit(requestContext, admin, "currencies.create", asset.Code, "Added the currency "+asset.Name, nil, asset)
	return &asset, nil
}

// ensureCompanyAccounts makes the ledger accounts for a currency: its Tribe
// Funds (ledger.CompanyAccountTemplates) and a vault at every provider that
// handles it (provider_assets). Each one is created only if it is missing,
// so it is safe to run again.
func (service *Service) ensureCompanyAccounts(requestContext context.Context, assetCode string) error {
	for _, template := range ledger.CompanyAccountTemplates {
		if _, err := service.ledger.EnsureCompanyAccount(requestContext, template, assetCode); err != nil {
			return err
		}
	}
	query := service.database.WithContext(requestContext)
	var providerAssets []models.ProviderAsset
	if err := query.Where("asset_code = ?", assetCode).Find(&providerAssets).Error; err != nil {
		return err
	}
	for _, providerAsset := range providerAssets {
		var provider models.Provider
		if err := query.Where("id = ?", providerAsset.ProviderID).Take(&provider).Error; err != nil {
			return err
		}
		if _, err := service.ledger.EnsureProviderVault(requestContext, provider, assetCode); err != nil {
			return err
		}
	}
	return nil
}

// CurrencyChanges is what an admin can change on a currency. A nil field is
// left as it is. The code, kind and decimal places can never change: money
// already recorded in that currency depends on them.
type CurrencyChanges struct {
	Name         *string
	Symbol       *string
	DisplayOrder *int
	Enabled      *bool
	Version      int // the version the admin last saw
}

// UpdateCurrency renames a currency, moves it in the list or switches it on
// or off (lock + version + audit).
//
// Switching a currency ON first makes sure its ledger accounts exist and
// that a live price can be fetched, so the app never shows a currency that
// cannot be priced or posted. Switching it OFF hides it from the app and
// refuses new quotes and wallets in it; balances customers already hold stay
// in the ledger untouched and come back when it is switched on again.
func (service *Service) UpdateCurrency(requestContext context.Context, admin *adminauth.Admin, code string, changes CurrencyChanges) (*models.Asset, error) {
	if changes.Name == nil && changes.Symbol == nil && changes.DisplayOrder == nil && changes.Enabled == nil {
		return nil, apperrors.Validation(map[string][]string{"enabled": {"Send at least one thing to change."}})
	}
	var current models.Asset
	if err := service.database.WithContext(requestContext).Where("code = ?", code).Take(&current).Error; err != nil {
		return nil, apperrors.NotFound("Currency")
	}
	if changes.Enabled != nil && !*changes.Enabled {
		if reason, alwaysOn := alwaysOnCurrencies[code]; alwaysOn {
			return nil, apperrors.New(http.StatusUnprocessableEntity, "CURRENCY_ALWAYS_ON", reason)
		}
	}
	if changes.Enabled != nil && *changes.Enabled && !current.Enabled {
		// Done BEFORE the lock below: creating ledger rows and calling a price
		// feed do not belong inside a transaction that holds a row lock.
		if err := service.ensureCompanyAccounts(requestContext, code); err != nil {
			return nil, err
		}
		if err := service.rates.CheckPriceSource(requestContext, code, current.PriceSource); err != nil {
			return nil, apperrors.New(http.StatusUnprocessableEntity, "CURRENCY_NOT_PRICED", "We cannot price "+code+" yet: "+err.Error()+".")
		}
	}

	var before, after models.Asset
	err := database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		// Lock the row so two admins changing the same currency at once are
		// handled one after the other; the second one then fails the version check.
		if err := database.LockForUpdate(databaseTransaction, &before, "code = ?", code); err != nil {
			return apperrors.NotFound("Currency")
		}
		if changes.Version != before.Version {
			return apperrors.ErrStaleData
		}
		updates := map[string]any{"version": before.Version + 1, "updated_at": time.Now().UTC()}
		if changes.Name != nil {
			name := strings.TrimSpace(*changes.Name)
			if name == "" || len(name) > 64 {
				return apperrors.Validation(map[string][]string{"name": {"Enter a name of up to 64 characters."}})
			}
			updates["name"] = name
		}
		if changes.Symbol != nil {
			symbol := strings.TrimSpace(*changes.Symbol)
			if symbol == "" || len(symbol) > 8 {
				return apperrors.Validation(map[string][]string{"symbol": {"Enter a symbol of up to 8 characters."}})
			}
			updates["symbol"] = symbol
		}
		if changes.DisplayOrder != nil {
			updates["display_order"] = *changes.DisplayOrder
		}
		if changes.Enabled != nil {
			updates["enabled"] = *changes.Enabled
		}
		if err := databaseTransaction.Model(&models.Asset{}).Where("code = ?", code).Updates(updates).Error; err != nil {
			return err
		}
		return databaseTransaction.Where("code = ?", code).Take(&after).Error
	})
	if err != nil {
		return nil, err
	}
	service.rates.ConfigChanged(requestContext) // every program reloads within a second
	summary := "Changed the currency " + before.Name
	if before.Enabled != after.Enabled {
		summary = "Switched " + before.Name + " on"
		if !after.Enabled {
			summary = "Switched " + before.Name + " off"
		}
	}
	service.adminAuth.Audit(requestContext, admin, "currencies.update", code, summary, before, after)
	return &after, nil
}
