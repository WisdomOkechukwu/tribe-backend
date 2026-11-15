// Package rates collects live exchange rates, prices conversions (with the
// NGN rail) and creates quotes.
//
// The live-rate flow, start to finish:
//
//  1. The COLLECTOR (a goroutine in cmd/scheduler) wakes up every
//     RATE_REFRESH_SECONDS, asks the provider for every asset's price in USD
//     and NGN, and saves them (exchange_rates = history, exchange_rate_latest
//     = newest).
//  2. It PUBLISHES one event with ALL rates to the Redis channel "rates.updated".
//  3. Every API server has a RateHub goroutine listening on that channel
//     (app/events/ratehub.go). It forwards the event to every phone that has
//     GET /api/mobile/v1/rates/stream open (the swap/send screens).
//  4. The app stores the rates and re-draws the screen.
package rates

import (
	"context"
	"encoding/json"
	"log/slog"
	"math/big"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/tribe-africa/tribe-backend/app/cache"
	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/app/services/providers"
	"github.com/tribe-africa/tribe-backend/config"
	"github.com/tribe-africa/tribe-backend/pkg/money"
)

// Redis names used for rates.
const (
	ChannelRatesUpdated = "rates.updated" // pub/sub channel every API server listens to
	keyLatestRates      = "rates:latest"  // the newest event, for new connections
	keyRatesVersion     = "rates:version" // a counter that goes up on every change
)

// rateDecimalPlaces is how many decimals we store rates with.
const rateDecimalPlaces = 18

// AssetRates is the rate of one asset inside an event.
type AssetRates struct {
	Usd                string `json:"usd"`                  // USD per 1 unit
	Ngn                string `json:"ngn"`                  // NGN per 1 unit
	ChangeTodayPercent string `json:"change_today_percent"` // movement since the start of the Lagos day
}

// RatesEvent is what we publish and stream to phones: ALL rates at once.
type RatesEvent struct {
	Version     uint64                `json:"version"`
	CollectedAt time.Time             `json:"collected_at"`
	IsStale     bool                  `json:"is_stale"`
	Rates       map[string]AssetRates `json:"rates"`
}

// Service is the rates engine.
type Service struct {
	database  *gorm.DB
	redis     *redis.Client
	providers *providers.Registry
	config    *cache.ConfigCache // shared copy of assets, providers, margins and fees
	settings  config.Config
}

// NewService creates the rates service.
func NewService(connection *gorm.DB, redisClient *redis.Client, registry *providers.Registry, configCache *cache.ConfigCache, settings config.Config) *Service {
	return &Service{database: connection, redis: redisClient, providers: registry, config: configCache, settings: settings}
}

// ConfigChanged tells every program to reload assets, providers, margins
// and fees (see cache.ConfigCache). Call it after an edit has committed.
func (service *Service) ConfigChanged(requestContext context.Context) {
	service.config.ConfigChanged(requestContext)
}

// CheckPriceSource says whether the rate provider can price an asset from a
// price source (fiat_feed, coinbase or usd_peg). It is asked before an admin
// adds a currency or switches one on. A provider that cannot answer (the
// simulator used by the tests) lets everything through.
func (service *Service) CheckPriceSource(requestContext context.Context, assetCode string, priceSource string) error {
	rateProvider, err := service.providers.Rates()
	if err != nil {
		return err
	}
	if priceChecker, ok := rateProvider.(providers.PriceChecker); ok {
		return priceChecker.CheckPrice(requestContext, assetCode, priceSource)
	}
	return nil
}

// Collect fetches new prices once, stores them and publishes the event.
func (service *Service) Collect(requestContext context.Context) (*RatesEvent, error) {
	var assets []models.Asset
	if err := service.database.WithContext(requestContext).Where("enabled = ?", true).Order("display_order").Find(&assets).Error; err != nil {
		return nil, err
	}
	assetCodes := make([]string, 0, len(assets))
	providerByAsset := map[string]*string{}
	priceSourceByAsset := map[string]string{}
	for _, asset := range assets {
		assetCodes = append(assetCodes, asset.Code)
		providerByAsset[asset.Code] = asset.ConversionProviderID
		priceSourceByAsset[asset.Code] = asset.PriceSource
	}

	rateProvider, err := service.providers.Rates()
	if err != nil {
		return nil, err
	}
	// Tell the provider where each asset's price comes from, as the assets
	// table says. This is how a currency added from the admin panel gets a
	// rate without its code being written into the provider.
	if priceSourceAware, ok := rateProvider.(providers.PriceSourceAware); ok {
		priceSourceAware.SetPriceSources(priceSourceByAsset)
	}
	prices, err := rateProvider.FetchPrices(requestContext, assetCodes)
	if err != nil {
		return nil, err
	}

	version, err := service.redis.Incr(requestContext, keyRatesVersion).Uint64()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	todayInLagos := now.In(service.settings.App.TimeZone).Format(time.DateOnly)

	// Load the previous "latest" rows to know each asset's opening rate today.
	var previousRows []models.ExchangeRateLatest
	service.database.WithContext(requestContext).Find(&previousRows)
	previousByAsset := map[string]models.ExchangeRateLatest{}
	for _, row := range previousRows {
		previousByAsset[row.AssetCode] = row
	}

	event := &RatesEvent{Version: version, CollectedAt: now, Rates: map[string]AssetRates{}}
	history := make([]models.ExchangeRate, 0, len(prices))
	latest := make([]models.ExchangeRateLatest, 0, len(prices))
	for _, assetCode := range assetCodes {
		price, found := prices[assetCode]
		if !found {
			continue
		}
		usdText := money.FormatRate(price.Usd, rateDecimalPlaces)
		ngnText := money.FormatRate(price.Ngn, rateDecimalPlaces)

		// A new Lagos day starts a new "opening" rate for the "% today" figure.
		openingText := usdText
		if previous, found := previousByAsset[assetCode]; found && previous.CollectedAt.In(service.settings.App.TimeZone).Format(time.DateOnly) == todayInLagos {
			openingText = previous.OpeningUsdRate
		}

		history = append(history, models.ExchangeRate{AssetCode: assetCode, UsdRate: usdText, NgnRate: ngnText, ProviderID: providerByAsset[assetCode], CollectedAt: now})
		latest = append(latest, models.ExchangeRateLatest{AssetCode: assetCode, UsdRate: usdText, NgnRate: ngnText, OpeningUsdRate: openingText, ProviderID: providerByAsset[assetCode], RatesVersion: version, CollectedAt: now})
		event.Rates[assetCode] = AssetRates{Usd: trimRate(price.Usd, 8), Ngn: trimRate(price.Ngn, 8), ChangeTodayPercent: percentChange(openingText, price.Usd)}
	}

	// Save history and upsert "latest" (INSERT ... ON DUPLICATE KEY UPDATE).
	err = service.database.WithContext(requestContext).Transaction(func(databaseTransaction *gorm.DB) error {
		if err := databaseTransaction.Create(&history).Error; err != nil {
			return err
		}
		return databaseTransaction.Clauses(clause.OnConflict{UpdateAll: true}).Create(&latest).Error
	})
	if err != nil {
		return nil, err
	}

	// Tell every API server (and through them, every open phone).
	encoded, _ := json.Marshal(event)
	service.redis.Set(requestContext, keyLatestRates, encoded, 0)
	if err := service.redis.Publish(requestContext, ChannelRatesUpdated, encoded).Err(); err != nil {
		slog.Warn("could not publish rates", "error", err)
	}
	return event, nil
}

// RunCollector calls Collect every RefreshEvery until the context is cancelled.
// It is started as a goroutine by the scheduler.
func (service *Service) RunCollector(requestContext context.Context) {
	ticker := time.NewTicker(service.settings.Rates.RefreshEvery)
	defer ticker.Stop()
	for {
		if _, err := service.Collect(requestContext); err != nil {
			slog.Error("rate collection failed", "error", err)
		}
		select {
		case <-requestContext.Done():
			return
		case <-ticker.C:
		}
	}
}

// LatestEvent returns the newest rates (from Redis, or rebuilt from MySQL).
func (service *Service) LatestEvent(requestContext context.Context) (*RatesEvent, error) {
	var event RatesEvent
	if cached, err := service.redis.Get(requestContext, keyLatestRates).Bytes(); err == nil && json.Unmarshal(cached, &event) == nil {
		event.IsStale = time.Since(event.CollectedAt) > service.settings.Rates.StaleAfter
		return &event, nil
	}
	var rows []models.ExchangeRateLatest
	if err := service.database.WithContext(requestContext).Find(&rows).Error; err != nil {
		return nil, err
	}
	event = RatesEvent{Rates: map[string]AssetRates{}}
	for _, row := range rows {
		usd, _ := money.ParseRate(row.UsdRate)
		ngn, _ := money.ParseRate(row.NgnRate)
		if usd == nil || ngn == nil {
			continue
		}
		event.Rates[row.AssetCode] = AssetRates{Usd: trimRate(usd, 8), Ngn: trimRate(ngn, 8), ChangeTodayPercent: percentChange(row.OpeningUsdRate, usd)}
		if row.RatesVersion > event.Version {
			event.Version = row.RatesVersion
			event.CollectedAt = row.CollectedAt
		}
	}
	event.IsStale = time.Since(event.CollectedAt) > service.settings.Rates.StaleAfter
	return &event, nil
}

// percentChange returns (now - opening) / opening * 100 as text with 2 decimals.
func percentChange(openingText string, current *big.Rat) string {
	opening, err := money.ParseRate(openingText)
	if err != nil || opening.Sign() == 0 {
		return "0.00"
	}
	change := new(big.Rat).Sub(current, opening)
	change.Quo(change, opening)
	change.Mul(change, big.NewRat(100, 1))
	return change.FloatString(2)
}

// trimRate formats a rate with a fixed number of decimals for display.
func trimRate(rate *big.Rat, decimalPlaces int) string {
	return rate.FloatString(decimalPlaces)
}
