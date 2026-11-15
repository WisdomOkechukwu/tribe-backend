// Package providers describes the outside companies Tribe works with, as Go
// INTERFACES, plus SIMULATOR versions that return placeholder answers.
//
// An interface is a promise: "anything that has these methods can be used
// here". Our services only talk to the interface, so when a real provider
// is chosen we write one new adapter and nothing else changes.
//
// Provider categories (from the providers table):
//
//	global       - fiat money abroad (USD, GBP, EUR, ...)
//	crypto       - BTC, ETH, USDT, ...
//	utilities    - airtime, data, TV and electricity bills
//	gift_card    - buying and selling gift cards
//	virtual_card - issuing dollar cards
//
// Simulators are clearly marked (is_simulator = 1) and are REFUSED in production.
package providers

import (
	"context"
	"errors"
	"math/big"
)

// ErrSimulatorInProduction is returned when production would use a fake provider.
var ErrSimulatorInProduction = errors.New("simulated providers are not allowed in production; configure a real adapter")

// AssetPrice is the live market price of one whole unit of an asset.
type AssetPrice struct {
	Usd *big.Rat // US dollars per unit
	Ngn *big.Rat // Naira per unit
}

// RateProvider gives live market prices.
type RateProvider interface {
	FetchPrices(requestContext context.Context, assetCodes []string) (map[string]AssetPrice, error)
}

// Where an asset's live price comes from (assets.price_source). An admin
// picks one when adding a currency, so no list of codes lives in the code.
const (
	PriceSourceFiatFeed = "fiat_feed" // the daily fiat feed, looked up by currency code
	PriceSourceCoinbase = "coinbase"  // Coinbase's spot price for CODE-USD
	PriceSourceUsdPeg   = "usd_peg"   // always exactly one US dollar (stablecoins, and USD itself)
)

// IsPriceSource is true for one of the three price sources above.
func IsPriceSource(priceSource string) bool {
	return priceSource == PriceSourceFiatFeed || priceSource == PriceSourceCoinbase || priceSource == PriceSourceUsdPeg
}

// PriceSourceAware is implemented by a rate provider that needs to be told
// where each asset's price comes from (the live provider does; the simulator
// has its own price table). The rate collector calls it before every
// collection with what the assets table says.
type PriceSourceAware interface {
	SetPriceSources(priceSourceByAsset map[string]string)
}

// PriceChecker is implemented by a rate provider that can say, BEFORE a
// currency is added or switched on, whether it is able to price it. It
// returns nil when it can, and an error a person can read when it cannot.
type PriceChecker interface {
	CheckPrice(requestContext context.Context, assetCode string, priceSource string) error
}

// PayoutResult is what a provider says about a payout.
type PayoutResult struct {
	Status            string // succeeded | failed | pending
	ProviderReference string
	FailureReason     string
}

// PayoutProvider sends money out of Tribe (bank transfer, crypto withdrawal).
type PayoutProvider interface {
	SendPayout(requestContext context.Context, reference string, assetCode string, amount string, destination map[string]string) (PayoutResult, error)
}

// RejectedError means the provider gave a FINAL "no" (an unknown meter, a
// brand out of stock): the purchase did not happen and never will, so the
// customer is refunded. Any OTHER error (a timeout, a 500, a dropped
// connection) means "we do not know yet": the purchase may have gone
// through, so it stays pending and is asked about again later, never
// refunded on a guess.
type RejectedError struct {
	Reason string
}

// Error makes RejectedError an error.
func (rejected *RejectedError) Error() string { return "rejected by provider: " + rejected.Reason }

// IsRejected reports whether err is a final "no" from a provider.
func IsRejected(err error) (*RejectedError, bool) {
	var rejected *RejectedError
	if errors.As(err, &rejected) {
		return rejected, true
	}
	return nil, false
}

// BillResult is what a utilities provider returns.
type BillResult struct {
	ProviderReference string
	Token             string // prepaid electricity only
	Units             string
}

// BillProvider pays bills.
//
// PayBill MUST treat reference as an idempotency key: calling it again with
// the same reference returns the result of the first call and never pays
// twice. Tribe calls it again after a timeout to find out what happened.
// A final refusal is returned as *RejectedError.
type BillProvider interface {
	VerifyCustomer(requestContext context.Context, billerID string, target string) (customerName string, err error)
	PayBill(requestContext context.Context, reference string, billerID string, target string, amountNgn string) (BillResult, error)
}

// GiftCardProvider buys and redeems gift cards.
//
// BuyCard MUST treat reference as an idempotency key (the same reference
// returns the same card, never a second one). A final refusal is returned
// as *RejectedError.
type GiftCardProvider interface {
	BuyCard(requestContext context.Context, reference string, brandID string, faceValueUsd string) (code string, err error)
	CheckSoldCard(requestContext context.Context, reference string, brandID string, code string) (valid bool, err error)
}

// IssuedCard is a new virtual card from the issuer.
type IssuedCard struct {
	ProviderCardReference string
	LastFour              string
	ExpiryMonth           int
	ExpiryYear            int
}

// CardIssuer creates and controls virtual cards.
type CardIssuer interface {
	IssueCard(requestContext context.Context, reference string, holderName string, network string) (IssuedCard, error)
	RevealCard(requestContext context.Context, providerCardReference string) (number string, cvv string, err error)
}

// Registry hands out the adapter for each category.
type Registry struct {
	isProduction bool
	simulator    *Simulator
	liveRates    *LiveRates
}

// NewRegistry creates the registry. In production it refuses simulators, except
// for rates, which come from real public price feeds in every environment.
func NewRegistry(isProduction bool, nairaPerDollarSeed string) *Registry {
	simulator := NewSimulator(nairaPerDollarSeed)
	// Outside production the simulator is the fallback when the live feeds are
	// unreachable (offline laptop); production has no fallback.
	var fallback RateProvider
	if !isProduction {
		fallback = simulator
	}
	return &Registry{isProduction: isProduction, simulator: simulator, liveRates: NewLiveRates(fallback)}
}

// Rates returns the live rate provider. Unlike the other providers these are
// real market prices from free public APIs, so they are used in production too.
func (registry *Registry) Rates() (RateProvider, error) {
	return registry.liveRates, nil
}

// Payouts returns the payout provider for a category (global or crypto).
func (registry *Registry) Payouts(category string) (PayoutProvider, error) {
	if registry.isProduction {
		return nil, ErrSimulatorInProduction
	}
	return registry.simulator, nil
}

// Bills returns the utilities provider.
func (registry *Registry) Bills() (BillProvider, error) {
	if registry.isProduction {
		return nil, ErrSimulatorInProduction
	}
	return registry.simulator, nil
}

// GiftCards returns the gift card provider.
func (registry *Registry) GiftCards() (GiftCardProvider, error) {
	if registry.isProduction {
		return nil, ErrSimulatorInProduction
	}
	return registry.simulator, nil
}

// Cards returns the virtual card issuer.
func (registry *Registry) Cards() (CardIssuer, error) {
	if registry.isProduction {
		return nil, ErrSimulatorInProduction
	}
	return registry.simulator, nil
}
