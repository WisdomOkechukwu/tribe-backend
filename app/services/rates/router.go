package rates

// The ROUTER decides HOW one asset becomes another:
//
//	direct     USDT -> USDC                   (one leg, same provider, rail off)
//	NGN rail   USD -> NGN -> GBP              (two legs through Naira)
//	           BTC -> NGN -> USD              (different providers always use the rail)
//
// Pricing (pricing.go) asks the router for the route, then works out the
// amounts. Each leg is later saved in swap_legs with its rate and provider.

import (
	"math/big"
	"net/http"

	"github.com/tribe-africa/tribe-backend/app/apperrors"
	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/pkg/money"
)

// RouteLeg is one hop of a conversion route.
type RouteLeg struct {
	Step         int    `json:"step"`
	FromAsset    string `json:"from_asset"`
	ToAsset      string `json:"to_asset"`
	FromAmount   string `json:"from_amount"` // smallest units
	ToAmount     string `json:"to_amount"`   // smallest units
	Rate         string `json:"rate"`        // to-units per from-unit (whole units)
	ProviderID   string `json:"provider_id"`
	ProviderName string `json:"provider_name"`
	ViaNgnRail   bool   `json:"via_ngn_rail"`
}

// decideRoute is the NGN RAIL RULE:
//
//   - If either side IS Naira, there is nothing to route through: one leg.
//   - If the two assets use DIFFERENT providers (e.g. BTC on the crypto
//     provider, GBP on the global provider) we MUST go through Naira:
//     BTC -> NGN, then NGN -> GBP.
//   - If they share a provider but that provider has uses_ngn_rail ON (or
//     cannot convert directly), we also go through Naira:
//     USD -> NGN, then NGN -> GBP.
//   - Otherwise (same provider, rail OFF) we convert directly: USDT -> USDC.
func decideRoute(market *marketData, from models.Asset, to models.Asset) (usesNgnRail bool, fromProvider models.Provider, toProvider models.Provider, err error) {
	if from.ConversionProviderID == nil || to.ConversionProviderID == nil {
		return false, fromProvider, toProvider, apperrors.New(http.StatusUnprocessableEntity, "ROUTE_UNAVAILABLE", "This conversion is not available right now.")
	}
	fromProvider = market.providers[*from.ConversionProviderID]
	toProvider = market.providers[*to.ConversionProviderID]
	if !fromProvider.Enabled || !toProvider.Enabled {
		return false, fromProvider, toProvider, apperrors.New(http.StatusUnprocessableEntity, "ROUTE_UNAVAILABLE", "This conversion is paused right now.")
	}
	if from.Code == "NGN" || to.Code == "NGN" {
		return false, fromProvider, toProvider, nil
	}
	if fromProvider.ID != toProvider.ID {
		return true, fromProvider, toProvider, nil
	}
	if fromProvider.UsesNgnRail || !fromProvider.SupportsDirectConversion {
		return true, fromProvider, toProvider, nil
	}
	return false, fromProvider, toProvider, nil
}

// buildRoute fills in the legs (and the Naira amounts on the NGN rail).
func buildRoute(market *marketData, price *Price, fromProvider models.Provider, toProvider models.Provider) []RouteLeg {
	from, to := price.FromAsset, price.ToAsset
	if !price.UsesNgnRail {
		return []RouteLeg{{
			Step: 1, FromAsset: from.Code, ToAsset: to.Code,
			FromAmount: price.Converted.UnitsString(), ToAmount: price.Received.UnitsString(),
			Rate: price.Rate.FloatString(rateDecimalPlaces), ProviderID: fromProvider.ID, ProviderName: fromProvider.Name,
		}}
	}

	naira := market.assets["NGN"]
	// Leg 1: from -> NGN (we sell the user's asset for Naira).
	legOneRate := applyMargin(market.ngn[from.Code], market.margins[from.Code].SellMarginPercent, -1)
	nairaMiddle := money.FromRational(new(big.Rat).Mul(price.Converted.ToRational(from.DecimalPlaces), legOneRate), naira.DecimalPlaces, money.RoundDown)
	// Leg 2: NGN -> to (we buy the asset the user wants with Naira).
	legTwoRate := new(big.Rat).Inv(applyMargin(market.ngn[to.Code], market.margins[to.Code].BuyMarginPercent, +1))

	// Naira at MARKET value on each side: the gap is our FX margin.
	nairaIn := money.FromRational(new(big.Rat).Mul(price.Converted.ToRational(from.DecimalPlaces), market.ngn[from.Code]), naira.DecimalPlaces, money.RoundHalfUp)
	nairaOut := money.FromRational(new(big.Rat).Mul(price.Received.ToRational(to.DecimalPlaces), market.ngn[to.Code]), naira.DecimalPlaces, money.RoundHalfUp)
	price.NairaIn, price.NairaOut = &nairaIn, &nairaOut

	return []RouteLeg{
		{Step: 1, FromAsset: from.Code, ToAsset: "NGN", FromAmount: price.Converted.UnitsString(), ToAmount: nairaMiddle.UnitsString(),
			Rate: legOneRate.FloatString(rateDecimalPlaces), ProviderID: fromProvider.ID, ProviderName: fromProvider.Name, ViaNgnRail: true},
		{Step: 2, FromAsset: "NGN", ToAsset: to.Code, FromAmount: nairaMiddle.UnitsString(), ToAmount: price.Received.UnitsString(),
			Rate: legTwoRate.FloatString(rateDecimalPlaces), ProviderID: toProvider.ID, ProviderName: toProvider.Name, ViaNgnRail: true},
	}
}
