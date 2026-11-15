package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"sync"
	"time"
)

// coinbaseSpotURL is Coinbase's public spot-price endpoint, filled in with one
// asset code (for example BTC). It needs no API key and returns the price of
// one whole unit in US dollars: GET /v2/prices/BTC-USD/spot.
const coinbaseSpotURL = "https://api.coinbase.com/v2/prices/%s-USD/spot"

// fiatRatesURL is the free, key-less fiat rates feed (open.er-api.com). One
// request returns every currency priced as "how many units equal one US dollar".
const fiatRatesURL = "https://open.er-api.com/v6/latest/USD"

// fiatRefreshEvery is how often we ask the fiat feed for new rates. The feed
// itself only changes once a day, and its free tier refuses callers who ask
// too often, so asking every 10 seconds (the collector's pace) would only get
// us blocked. Between refreshes we reuse the rates we already have.
const fiatRefreshEvery = time.Hour

// fiatReuseLimit is the oldest fiat rates we will keep using when a refresh
// fails. The feed updates daily, so rates up to a day old (plus a margin) are
// still the latest the feed would give us.
const fiatReuseLimit = 26 * time.Hour

// cryptoReuseLimit is the oldest crypto price we will keep using when a
// Coinbase call fails. Crypto moves quickly, so this is short: after two
// minutes the price is too old to quote on and we report an error instead.
const cryptoReuseLimit = 2 * time.Minute

// cryptoAssetsFromCoinbase are the crypto assets we price from Coinbase when
// nobody has told us an asset's price source (see SetPriceSources). The
// assets table normally says so for every asset; this list is the starting
// point before the first collection, and what the tests use.
// Stablecoins are handled separately (pegged to one dollar), so they are not here.
var cryptoAssetsFromCoinbase = map[string]bool{"BTC": true, "ETH": true, "SOL": true}

// stablecoinAssets are always worth exactly one US dollar, so we never call out
// for them. Like the list above, it only applies to an asset whose price
// source has not been set.
var stablecoinAssets = map[string]bool{"USD": true, "USDT": true, "USDC": true}

// cachedPrice is a price we fetched successfully, and when we fetched it.
type cachedPrice struct {
	usdPrice  *big.Rat
	fetchedAt time.Time
}

// LiveRates is a REAL rate provider (not a simulator). It reads crypto prices
// from Coinbase and fiat prices from open.er-api.com, both free and without an
// API key.
//
// Every outgoing request runs in its own goroutine, so one collection fires all
// of its HTTP calls at the same time and finishes as fast as the slowest call,
// not the sum of them.
//
// It remembers the last good answer from each feed. When a call fails, a
// recent answer is reused (see fiatReuseLimit and cryptoReuseLimit), so one
// failed call never replaces real prices with simulated ones.
type LiveRates struct {
	httpClient *http.Client
	// fallback is used ONLY outside production, and only when no recent live
	// price exists at all (for example an offline laptop). It is nil in
	// production, where simulated data is never allowed.
	fallback RateProvider

	// The feed addresses. They are fields (not only constants) so a test can
	// point them at a fake server.
	coinbaseAddressPattern string
	fiatAddress            string

	// mutex protects the cache below. The scheduler calls FetchPrices from one
	// goroutine, but the API server or a test could call it from several, and
	// Go maps must never be read and written by two goroutines at once.
	mutex          sync.Mutex
	fiatRates      map[string]*big.Rat    // units per US dollar, from the last good fiat call
	fiatFetchedAt  time.Time              // when fiatRates arrived (zero = never)
	fiatCheckedAt  time.Time              // when we last ASKED the fiat feed, even if it failed
	cryptoPrices   map[string]cachedPrice // asset code -> last good Coinbase price
	priceSources   map[string]string      // asset code -> fiat_feed | coinbase | usd_peg, from the assets table
	timeNow        func() time.Time       // the clock; tests replace it to move time forward
	usingSimulator bool                   // true while we are on the fallback, so we log only the change
}

// NewLiveRates creates the live rate provider. Pass a non-nil fallback only
// outside production.
func NewLiveRates(fallback RateProvider) *LiveRates {
	return &LiveRates{
		httpClient:             &http.Client{Timeout: 8 * time.Second},
		fallback:               fallback,
		coinbaseAddressPattern: coinbaseSpotURL,
		fiatAddress:            fiatRatesURL,
		cryptoPrices:           map[string]cachedPrice{},
		timeNow:                time.Now,
	}
}

// SetPriceSources tells the provider where each asset's price comes from, as
// the assets table says (assets.price_source). The rate collector calls it
// before every collection, so a currency an admin has just added is priced
// from the right feed without any change to this file.
func (live *LiveRates) SetPriceSources(priceSourceByAsset map[string]string) {
	live.mutex.Lock()
	defer live.mutex.Unlock()
	live.priceSources = priceSourceByAsset
}

// priceSourceOf returns where an asset's price comes from: what the assets
// table said, or, for an asset nobody told us about, the built-in lists.
// The caller must hold live.mutex.
func (live *LiveRates) priceSourceOf(assetCode string) string {
	if priceSource := live.priceSources[assetCode]; priceSource != "" {
		return priceSource
	}
	switch {
	case stablecoinAssets[assetCode]:
		return PriceSourceUsdPeg
	case cryptoAssetsFromCoinbase[assetCode]:
		return PriceSourceCoinbase
	}
	return PriceSourceFiatFeed
}

// CheckPrice says whether an asset can be priced from a price source, by
// asking the feed right now. An admin adding a currency gets a clear "we
// cannot price this" instead of a currency that never shows a rate.
func (live *LiveRates) CheckPrice(requestContext context.Context, assetCode string, priceSource string) error {
	switch priceSource {
	case PriceSourceUsdPeg:
		return nil // always one dollar: nothing to look up
	case PriceSourceCoinbase:
		if _, err := live.fetchCoinbaseSpot(requestContext, assetCode); err != nil {
			return fmt.Errorf("Coinbase has no %s-USD price", assetCode)
		}
		return nil
	case PriceSourceFiatFeed:
		// Use the rates we already hold when we have them (the feed's free
		// tier refuses callers who ask too often); ask only when we have none.
		live.mutex.Lock()
		fiatRates := live.fiatRates
		live.mutex.Unlock()
		if fiatRates == nil {
			freshFiatRates, err := live.fetchFiatRates(requestContext)
			if err != nil {
				return fmt.Errorf("the fiat rates feed could not be reached, so %s could not be checked", assetCode)
			}
			fiatRates = freshFiatRates
		}
		if unitsPerDollar, found := fiatRates[assetCode]; !found || unitsPerDollar.Sign() <= 0 {
			return fmt.Errorf("the fiat rates feed has no rate for %s", assetCode)
		}
		return nil
	}
	return fmt.Errorf("unknown price source %q", priceSource)
}

// FetchPrices returns the live USD and NGN price of one whole unit of every
// requested asset. The NGN price of every asset is its USD price multiplied by
// the live Naira-per-dollar rate, which the fiat feed provides.
//
// Steps:
//  1. Ask the fiat feed only if our copy is more than fiatRefreshEvery old.
//     Ask Coinbase for every crypto asset. All calls run at the same time.
//  2. A failed call reuses that feed's last good answer if it is recent enough.
//  3. If something is still missing, use the simulator (developer machines
//     only) or return an error (production).
func (live *LiveRates) FetchPrices(requestContext context.Context, assetCodes []string) (map[string]AssetPrice, error) {
	live.mutex.Lock()
	defer live.mutex.Unlock()
	now := live.timeNow()

	// Which crypto assets we actually need to ask Coinbase about this time.
	requestedCrypto := make([]string, 0, len(assetCodes))
	for _, assetCode := range assetCodes {
		if live.priceSourceOf(assetCode) == PriceSourceCoinbase {
			requestedCrypto = append(requestedCrypto, assetCode)
		}
	}

	var waitGroup sync.WaitGroup

	// Fiat: one request for every currency, including the NGN rail, but only
	// once per fiatRefreshEvery. fiatCheckedAt moves forward even when the call
	// fails, so a broken feed is retried hourly instead of every 10 seconds.
	var freshFiatRates map[string]*big.Rat
	var fiatError error
	if now.Sub(live.fiatCheckedAt) >= fiatRefreshEvery {
		live.fiatCheckedAt = now
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			freshFiatRates, fiatError = live.fetchFiatRates(requestContext)
		}()
	}

	// Crypto: one request per asset, all launched before we wait on any of them.
	// Each goroutine writes only its own slot, so no lock is needed; waitGroup.Wait
	// below makes those writes visible to this goroutine.
	freshCryptoPrices := make([]*big.Rat, len(requestedCrypto))
	cryptoErrors := make([]error, len(requestedCrypto))
	for index, assetCode := range requestedCrypto {
		waitGroup.Add(1)
		go func(slot int, code string) {
			defer waitGroup.Done()
			freshCryptoPrices[slot], cryptoErrors[slot] = live.fetchCoinbaseSpot(requestContext, code)
		}(index, assetCode)
	}

	waitGroup.Wait()

	// Save every good answer, and note every failure.
	if fiatError != nil {
		slog.Warn("live rates: the fiat feed failed; the last good fiat rates are reused if recent enough", "error", fiatError, "last_good", live.fiatFetchedAt)
	} else if freshFiatRates != nil {
		live.fiatRates, live.fiatFetchedAt = freshFiatRates, now
	}
	for index, assetCode := range requestedCrypto {
		if cryptoErrors[index] != nil {
			slog.Warn("live rates: a Coinbase call failed; the last good price is reused if it is recent enough", "asset", assetCode, "error", cryptoErrors[index])
			continue
		}
		live.cryptoPrices[assetCode] = cachedPrice{usdPrice: freshCryptoPrices[index], fetchedAt: now}
	}

	prices, missing := live.pricesFromCache(assetCodes, now)
	if missing != nil {
		if live.fallback == nil {
			return nil, missing
		}
		if !live.usingSimulator {
			slog.Warn("live rates: no recent live price; using the SIMULATOR until the feeds answer again", "reason", missing)
			live.usingSimulator = true
		}
		return live.fallback.FetchPrices(requestContext, assetCodes)
	}
	if live.usingSimulator {
		slog.Info("live rates: the feeds answered again; back on live prices")
		live.usingSimulator = false
	}
	return prices, nil
}

// pricesFromCache builds the prices from the remembered feed answers. It
// returns an error naming the first thing that is missing or too old to use.
// The caller must hold live.mutex.
func (live *LiveRates) pricesFromCache(assetCodes []string, now time.Time) (map[string]AssetPrice, error) {
	if live.fiatRates == nil || now.Sub(live.fiatFetchedAt) > fiatReuseLimit {
		return nil, fmt.Errorf("live rates: no fiat rates newer than %s", fiatReuseLimit)
	}
	nairaPerDollar, found := live.fiatRates["NGN"]
	if !found || nairaPerDollar.Sign() <= 0 {
		return nil, fmt.Errorf("live rates: the fiat feed did not include an NGN rate")
	}

	// Work out the USD price of one unit of each asset.
	usdPerUnit := map[string]*big.Rat{}
	for _, assetCode := range assetCodes {
		switch {
		case assetCode == "NGN":
			// handled as the rail below
		case live.priceSourceOf(assetCode) == PriceSourceUsdPeg:
			usdPerUnit[assetCode] = big.NewRat(1, 1)
		case live.priceSourceOf(assetCode) == PriceSourceCoinbase:
			cached, found := live.cryptoPrices[assetCode]
			if !found || now.Sub(cached.fetchedAt) > cryptoReuseLimit {
				return nil, fmt.Errorf("live rates: no %s price newer than %s", assetCode, cryptoReuseLimit)
			}
			usdPerUnit[assetCode] = cached.usdPrice
		default:
			if unitsPerDollar, found := live.fiatRates[assetCode]; found && unitsPerDollar.Sign() > 0 {
				usdPerUnit[assetCode] = new(big.Rat).Inv(unitsPerDollar)
			}
		}
	}

	// Turn USD prices into the AssetPrice (USD and NGN per unit) the collector saves.
	prices := make(map[string]AssetPrice, len(assetCodes))
	for _, assetCode := range assetCodes {
		if assetCode == "NGN" {
			// One Naira is 1/nairaPerDollar dollars, and one Naira by definition.
			prices["NGN"] = AssetPrice{Usd: new(big.Rat).Inv(nairaPerDollar), Ngn: big.NewRat(1, 1)}
			continue
		}
		usd := usdPerUnit[assetCode]
		if usd == nil {
			continue // an asset neither feed covers; skip it like the simulator does
		}
		prices[assetCode] = AssetPrice{Usd: usd, Ngn: new(big.Rat).Mul(usd, nairaPerDollar)}
	}
	return prices, nil
}

// fetchCoinbaseSpot reads one asset's USD spot price from Coinbase.
func (live *LiveRates) fetchCoinbaseSpot(requestContext context.Context, assetCode string) (*big.Rat, error) {
	var payload struct {
		Data struct {
			Amount string `json:"amount"`
		} `json:"data"`
	}
	if err := live.getJSON(requestContext, fmt.Sprintf(live.coinbaseAddressPattern, assetCode), &payload); err != nil {
		return nil, err
	}
	price, ok := new(big.Rat).SetString(payload.Data.Amount)
	if !ok || price.Sign() <= 0 {
		return nil, fmt.Errorf("live rates: Coinbase returned an unreadable %s price %q", assetCode, payload.Data.Amount)
	}
	return price, nil
}

// fetchFiatRates reads every fiat currency's "units per US dollar" rate from
// open.er-api.com in a single request.
func (live *LiveRates) fetchFiatRates(requestContext context.Context) (map[string]*big.Rat, error) {
	var payload struct {
		Result string                 `json:"result"`
		Rates  map[string]json.Number `json:"rates"`
	}
	if err := live.getJSON(requestContext, live.fiatAddress, &payload); err != nil {
		return nil, err
	}
	if payload.Result != "success" {
		return nil, fmt.Errorf("live rates: the fiat feed returned result %q", payload.Result)
	}
	rates := make(map[string]*big.Rat, len(payload.Rates))
	for code, number := range payload.Rates {
		if value, ok := new(big.Rat).SetString(number.String()); ok {
			rates[code] = value
		}
	}
	return rates, nil
}

// getJSON performs a GET request that is cancelled with the context and decodes
// the JSON body into target.
func (live *LiveRates) getJSON(requestContext context.Context, address string, target any) error {
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, address, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	response, err := live.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("live rates: GET %s returned HTTP %d", address, response.StatusCode)
	}
	return json.NewDecoder(response.Body).Decode(target)
}
