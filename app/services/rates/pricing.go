package rates

import (
	"context"
	"fmt"
	"math/big"
	"net/http"
	"time"

	"gorm.io/gorm"

	"github.com/tribe-africa/tribe-backend/app/apperrors"
	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/pkg/money"
)

// Price is the full price of a conversion, before it is saved as a quote.
type Price struct {
	TransactionType string
	FromAsset       models.Asset
	ToAsset         models.Asset
	TotalPaid       money.Amount // what leaves the user's wallet (fee included)
	Fee             money.Amount // in the from-asset
	Converted       money.Amount // TotalPaid - Fee: the part that is converted
	Received        money.Amount // what arrives, in the to-asset
	Rate            *big.Rat     // effective rate after margins: to-units per from-unit
	UsesNgnRail     bool
	NairaIn         *money.Amount // NGN rail: Naira we get for the from-asset (market value)
	NairaOut        *money.Amount // NGN rail: Naira we pay for the to-asset (market value)
	Route           []RouteLeg
	UsdValue        *big.Rat             // USD value of the converted amount, for reports
	RatesUsed       map[string][2]string // asset -> [usd, ngn] used, saved on the journal legs
}

// marketData is everything pricing needs, loaded once per request.
type marketData struct {
	assets    map[string]models.Asset
	providers map[string]models.Provider
	usd       map[string]*big.Rat
	ngn       map[string]*big.Rat
	margins   map[string]models.RateMargin
	collected time.Time
}

// loadMarket reads assets, providers, latest rates and margins.
//
// Assets, providers and margins come from the shared configuration copy
// (cache.ConfigCache: reloaded within a second of an admin edit). The LATEST
// RATES are always read from MySQL: they change every few seconds and a
// quote must refuse rates that are too old (see PriceConversion).
func (service *Service) loadMarket(query *gorm.DB) (*marketData, error) {
	snapshot, err := service.config.Snapshot(query.Statement.Context)
	if err != nil {
		return nil, err
	}
	// The maps are only read, never changed, so sharing them is safe.
	market := &marketData{assets: snapshot.Assets, providers: snapshot.Providers, usd: map[string]*big.Rat{}, ngn: map[string]*big.Rat{}, margins: snapshot.Margins}

	var latest []models.ExchangeRateLatest
	if err := query.Find(&latest).Error; err != nil {
		return nil, err
	}
	for _, row := range latest {
		usd, err := money.ParseRate(row.UsdRate)
		if err != nil {
			return nil, err
		}
		ngn, err := money.ParseRate(row.NgnRate)
		if err != nil {
			return nil, err
		}
		market.usd[row.AssetCode], market.ngn[row.AssetCode] = usd, ngn
		if market.collected.IsZero() || row.CollectedAt.Before(market.collected) {
			market.collected = row.CollectedAt
		}
	}
	return market, nil
}

// PriceInput asks "what does converting this cost?".
type PriceInput struct {
	TransactionType string
	FromAsset       string
	ToAsset         string
	// Give EXACTLY ONE of these (as typed by the user, e.g. "25.50"):
	PayAmount     string // "I want to pay this much (fee included)"
	ReceiveAmount string // "I want to receive this much"
	UsdAmount     string // "I want to pay this many US dollars' worth (fee included)"
}

// usdDecimalPlaces is how many decimal places a typed dollar amount may have (cents).
const usdDecimalPlaces = 2

// payAmountForUsd turns "I want to swap 25 dollars' worth" into an amount of
// the asset being paid, so the phone never has to do this conversion itself.
//
// It uses the plain market price (usdPerUnit = dollars for one unit of the
// asset), with no margin: the margin is applied afterwards, in the conversion
// rate, exactly as it is for a typed pay amount. The result rounds DOWN, so
// the user never pays more than the dollars they typed.
func payAmountForUsd(usdAmountText string, usdPerUnit *big.Rat, decimalPlaces int) (money.Amount, error) {
	usdAmount, err := money.ParseDecimal(usdAmountText, usdDecimalPlaces)
	if err != nil {
		return money.Zero(), err
	}
	if usdPerUnit == nil || usdPerUnit.Sign() <= 0 {
		return money.Zero(), money.ErrInvalidAmount
	}
	payAmount := new(big.Rat).Quo(usdAmount.ToRational(usdDecimalPlaces), usdPerUnit)
	return money.FromRational(payAmount, decimalPlaces, money.RoundDown), nil
}

// PriceConversion works out a full price. It never saves anything.
//
// Maths (all exact fractions, rounded only at the end):
//
//	sellPrice(from) = usd(from) × (1 − sell margin of from)   // we pay a bit less for what we buy
//	buyPrice(to)    = usd(to)   × (1 + buy margin of to)      // we charge a bit more for what we sell
//	rate            = sellPrice(from) / buyPrice(to)
//
// On the NGN rail the same rate is reached in two steps through Naira, and
// we also record the Naira received and paid (the difference is our margin).
func (service *Service) PriceConversion(requestContext context.Context, input PriceInput) (*Price, error) {
	query := service.database.WithContext(requestContext)
	market, err := service.loadMarket(query)
	if err != nil {
		return nil, err
	}
	from, fromFound := market.assets[input.FromAsset]
	to, toFound := market.assets[input.ToAsset]
	if !fromFound || !toFound || !from.Enabled || !to.Enabled {
		return nil, apperrors.New(http.StatusUnprocessableEntity, "UNKNOWN_ASSET", "That currency is not supported.")
	}
	if from.Code == to.Code {
		return nil, apperrors.New(http.StatusUnprocessableEntity, "SAME_ASSET", "Pick two different currencies.")
	}
	if market.usd[from.Code] == nil || market.usd[to.Code] == nil {
		return nil, apperrors.New(http.StatusServiceUnavailable, "RATES_UNAVAILABLE", "Rates are loading. Try again in a moment.")
	}
	if time.Since(market.collected) > service.settings.Rates.StaleAfter {
		return nil, apperrors.New(http.StatusServiceUnavailable, "RATES_STALE", "Rates are out of date. Try again in a moment.")
	}

	usesNgnRail, fromProvider, toProvider, err := decideRoute(market, from, to)
	if err != nil {
		return nil, err
	}

	sellPrice := applyMargin(market.usd[from.Code], market.margins[from.Code].SellMarginPercent, -1)
	buyPrice := applyMargin(market.usd[to.Code], market.margins[to.Code].BuyMarginPercent, +1)
	rate := new(big.Rat).Quo(sellPrice, buyPrice) // to-units per from-unit

	feeRule, err := service.feeRule(query, input.TransactionType)
	if err != nil {
		return nil, err
	}

	price := &Price{TransactionType: input.TransactionType, FromAsset: from, ToAsset: to, Rate: rate, UsesNgnRail: usesNgnRail}

	// The phone sends the amount in the unit the user typed it in, and only
	// that one: two amounts could disagree, and we would have to guess.
	amountsGiven := 0
	for _, amountText := range []string{input.PayAmount, input.ReceiveAmount, input.UsdAmount} {
		if amountText != "" {
			amountsGiven++
		}
	}
	if amountsGiven > 1 {
		return nil, apperrors.Validation(map[string][]string{"amount": {"Send only one of amount, receive_amount or usd_amount."}})
	}

	switch {
	case input.PayAmount != "" || input.UsdAmount != "":
		// A dollar amount is first turned into the pay amount, then priced
		// exactly like a typed pay amount.
		var totalPaid money.Amount
		var err error
		if input.PayAmount != "" {
			totalPaid, err = money.ParseDecimal(input.PayAmount, from.DecimalPlaces)
		} else {
			totalPaid, err = payAmountForUsd(input.UsdAmount, market.usd[from.Code], from.DecimalPlaces)
		}
		if err != nil || !totalPaid.IsPositive() {
			return nil, apperrors.Validation(map[string][]string{"amount": {"Enter a valid amount."}})
		}
		fee := calculateFee(feeRule, totalPaid, from, market)
		if fee.Compare(totalPaid) >= 0 {
			return nil, apperrors.Validation(map[string][]string{"amount": {"The amount is too small to cover the fee."}})
		}
		price.TotalPaid, price.Fee, price.Converted = totalPaid, fee, totalPaid.Subtract(fee)
		received := new(big.Rat).Mul(price.Converted.ToRational(from.DecimalPlaces), rate)
		price.Received = money.FromRational(received, to.DecimalPlaces, money.RoundDown) // never pay out a fraction we don't have

	case input.ReceiveAmount != "":
		received, err := money.ParseDecimal(input.ReceiveAmount, to.DecimalPlaces)
		if err != nil || !received.IsPositive() {
			return nil, apperrors.Validation(map[string][]string{"amount": {"Enter a valid amount."}})
		}
		converted := new(big.Rat).Quo(received.ToRational(to.DecimalPlaces), rate)
		price.Received = received
		price.Converted = money.FromRational(converted, from.DecimalPlaces, money.RoundUp) // round up what the user pays
		price.Fee = calculateFee(feeRule, price.Converted, from, market)
		price.TotalPaid = price.Converted.Add(price.Fee)

	default:
		return nil, apperrors.Validation(map[string][]string{"amount": {"Enter an amount."}})
	}
	if !price.Received.IsPositive() {
		return nil, apperrors.Validation(map[string][]string{"amount": {"The amount is too small."}})
	}

	price.UsdValue = new(big.Rat).Mul(price.Converted.ToRational(from.DecimalPlaces), market.usd[from.Code])
	price.RatesUsed = map[string][2]string{}
	for _, assetCode := range []string{from.Code, to.Code, "NGN"} {
		if market.usd[assetCode] != nil {
			price.RatesUsed[assetCode] = [2]string{money.FormatRate(market.usd[assetCode], rateDecimalPlaces), money.FormatRate(market.ngn[assetCode], rateDecimalPlaces)}
		}
	}
	price.Route = buildRoute(market, price, fromProvider, toProvider)
	return price, nil
}

// applyMargin returns price × (1 + direction × percent / 100).
// direction -1 lowers the price (sell margin), +1 raises it (buy margin).
func applyMargin(price *big.Rat, percentText string, direction int64) *big.Rat {
	percent, err := money.ParseRate(percentText)
	if err != nil || percentText == "" {
		percent = new(big.Rat)
	}
	factor := new(big.Rat).Quo(percent, big.NewRat(100, 1))
	factor.Mul(factor, big.NewRat(direction, 1))
	factor.Add(factor, big.NewRat(1, 1))
	return new(big.Rat).Mul(price, factor)
}

// feeRule returns the active fee settings for a transaction type (no rule =
// no fee), from the shared configuration copy. An admin's fee change is
// used everywhere within about a second of being saved.
func (service *Service) feeRule(query *gorm.DB, transactionType string) (*models.TransactionFee, error) {
	snapshot, err := service.config.Snapshot(query.Statement.Context)
	if err != nil {
		return nil, err
	}
	rule, found := snapshot.Fees[transactionType]
	if !found {
		return nil, nil
	}
	return &rule, nil // a copy: the shared one is never changed
}

// calculateFee works out the fee on an amount: percent + flat fee, capped.
// The flat fee and cap are set in flat_fee_asset (usually USD) and converted
// to the asset being paid. Fees round UP (we never undercharge a fraction).
func calculateFee(rule *models.TransactionFee, amount money.Amount, asset models.Asset, market *marketData) money.Amount {
	if rule == nil {
		return money.Zero()
	}
	percent, _ := money.ParseRate(rule.FeePercent)
	if percent == nil {
		percent = new(big.Rat)
	}
	fee := new(big.Rat).Mul(amount.ToRational(asset.DecimalPlaces), new(big.Rat).Quo(percent, big.NewRat(100, 1)))

	convertFromFeeAsset := func(valueText string) *big.Rat {
		value, err := money.ParseRate(valueText)
		if err != nil || value.Sign() == 0 {
			return new(big.Rat)
		}
		if rule.FlatFeeAsset == asset.Code {
			return value
		}
		feeAssetUsd, assetUsd := market.usd[rule.FlatFeeAsset], market.usd[asset.Code]
		if feeAssetUsd == nil || assetUsd == nil || assetUsd.Sign() == 0 {
			return new(big.Rat)
		}
		return new(big.Rat).Quo(new(big.Rat).Mul(value, feeAssetUsd), assetUsd)
	}
	fee.Add(fee, convertFromFeeAsset(rule.FlatFee))
	if rule.MaximumFee != nil {
		maximum := convertFromFeeAsset(*rule.MaximumFee)
		if maximum.Sign() > 0 && fee.Cmp(maximum) > 0 {
			fee = maximum
		}
	}
	return money.FromRational(fee, asset.DecimalPlaces, money.RoundUp)
}

// FeeFor works out a fee for an action that is not a conversion (for example
// an internal transfer or a card creation fee).
//
// query is the connection to read with. Inside database.WithTransaction,
// pass the databaseTransaction: asking the pool for a SECOND connection
// while holding one can make every request wait forever when the pool is
// busy (each holds a connection and waits for another).
func (service *Service) FeeFor(query *gorm.DB, transactionType string, assetCode string, amount money.Amount) (money.Amount, error) {
	market, err := service.loadMarket(query)
	if err != nil {
		return money.Zero(), err
	}
	asset, found := market.assets[assetCode]
	if !found {
		return money.Zero(), fmt.Errorf("unknown asset %s", assetCode)
	}
	rule, err := service.feeRule(query, transactionType)
	if err != nil {
		return money.Zero(), err
	}
	return calculateFee(rule, amount, asset, market), nil
}

// UsdValueOf returns the USD value of an amount, using the latest rate.
// query follows the same rule as FeeFor (pass the databaseTransaction inside one).
func (service *Service) UsdValueOf(query *gorm.DB, assetCode string, amount money.Amount, decimalPlaces int) string {
	var latest models.ExchangeRateLatest
	if err := query.Where("asset_code = ?", assetCode).Take(&latest).Error; err != nil {
		return "0"
	}
	usd, err := money.ParseRate(latest.UsdRate)
	if err != nil {
		return "0"
	}
	return new(big.Rat).Mul(amount.ToRational(decimalPlaces), usd).FloatString(2)
}

// RatesFor returns [usd, ngn] for assets, to store on journal legs.
// query follows the same rule as FeeFor (pass the databaseTransaction inside one).
func (service *Service) RatesFor(query *gorm.DB, assetCodes ...string) map[string][2]string {
	var rows []models.ExchangeRateLatest
	query.Where("asset_code IN ?", assetCodes).Find(&rows)
	result := map[string][2]string{}
	for _, row := range rows {
		result[row.AssetCode] = [2]string{row.UsdRate, row.NgnRate}
	}
	return result
}
