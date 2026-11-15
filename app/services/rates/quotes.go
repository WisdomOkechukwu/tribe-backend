package rates

import (
	"context"
	"encoding/json"
	"time"

	"gorm.io/gorm"

	"github.com/tribe-africa/tribe-backend/app/apperrors"
	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/app/services/shared"
	"github.com/tribe-africa/tribe-backend/pkg/ids"
	"github.com/tribe-africa/tribe-backend/pkg/money"
)

// CreateQuote prices a conversion and SAVES it, so the user can confirm at
// exactly this price within QUOTE_LIFETIME_SECONDS (30 s by default).
//
// A quote for a service an admin switched off is refused (503
// SERVICE_DISABLED): there is no point showing a price nobody can use.
func (service *Service) CreateQuote(requestContext context.Context, userID string, input PriceInput) (*models.Quote, *Price, error) {
	if err := service.requireQuotedService(requestContext, input); err != nil {
		return nil, nil, err
	}
	price, err := service.PriceConversion(requestContext, input)
	if err != nil {
		return nil, nil, err
	}
	routeJSON, err := json.Marshal(price.Route)
	if err != nil {
		return nil, nil, err
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	quote := models.Quote{
		ID:              ids.New(),
		UserID:          userID,
		TransactionType: input.TransactionType,
		FromAsset:       price.FromAsset.Code,
		ToAsset:         price.ToAsset.Code,
		FromAmount:      price.TotalPaid.UnitsString(),
		FeeAmount:       price.Fee.UnitsString(),
		ToAmount:        price.Received.UnitsString(),
		Rate:            price.Rate.FloatString(rateDecimalPlaces),
		UsesNgnRail:     price.UsesNgnRail,
		Route:           routeJSON,
		ExpiresAt:       now.Add(service.settings.Rates.QuoteLifetime),
		CreatedAt:       now,
	}
	if price.NairaIn != nil {
		nairaIn, nairaOut := price.NairaIn.UnitsString(), price.NairaOut.UnitsString()
		quote.NairaInAmount, quote.NairaOutAmount = &nairaIn, &nairaOut
	}
	if price.MarketReceived != nil {
		marketToAmount := price.MarketReceived.UnitsString()
		quote.MarketToAmount = &marketToAmount
	}
	if err := service.database.WithContext(requestContext).Create(&quote).Error; err != nil {
		return nil, nil, err
	}
	return &quote, price, nil
}

// requireQuotedService checks the service switch a quote is for.
//
//   - A swap quote needs the swap service.
//   - A send quote does not say WHO the money is for yet (a Tribe tag, a
//     bank account, a crypto wallet), so it is refused only when every kind
//     of send that could use it is off: the send to a Tribe tag, plus the
//     outside send that pays out in the quote's to-asset (crypto send for a
//     coin, Nigerian bank for Naira, global send for other currencies).
//     The send itself is checked again, exactly, in transfers.Send.
//
// Other transaction types (bills, cards) never come through CreateQuote.
func (service *Service) requireQuotedService(requestContext context.Context, input PriceInput) error {
	query := service.database.WithContext(requestContext)
	switch input.TransactionType {
	case "swap":
		return shared.RequireService(query, shared.ServiceSwap)
	case "send":
		snapshot, err := service.config.Snapshot(requestContext)
		if err != nil {
			return err
		}
		recipientKind := "bank"
		if snapshot.Assets[input.ToAsset].Kind == "crypto" {
			recipientKind = "wallet"
		}
		return shared.RequireAnyService(query, shared.SendServiceFor(recipientKind, input.ToAsset), shared.ServiceSendTribe)
	}
	return nil
}

// ConsumeQuote uses a quote ONCE, inside the caller's money transaction.
//
// The trick is a single UPDATE with every rule in its WHERE clause:
//
//	UPDATE quotes SET used_at = now
//	 WHERE id = ? AND user_id = ? AND used_at IS NULL AND expires_at > now
//
// If two requests try to use the same quote at the same moment, MySQL lets
// only ONE of them change the row (RowsAffected = 1). The other sees 0 and
// is refused. No double spending, no extra locks needed.
func (service *Service) ConsumeQuote(databaseTransaction *gorm.DB, quoteID string, userID string, transactionType string) (*models.Quote, error) {
	now := time.Now().UTC()
	result := databaseTransaction.Model(&models.Quote{}).
		Where("id = ? AND user_id = ? AND transaction_type = ? AND used_at IS NULL AND expires_at > ?", quoteID, userID, transactionType, now).
		Update("used_at", now)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected != 1 {
		return nil, apperrors.ErrQuoteExpired
	}
	var quote models.Quote
	if err := databaseTransaction.Where("id = ?", quoteID).Take(&quote).Error; err != nil {
		return nil, err
	}
	return &quote, nil
}

// MarginAmountsOf reads the amounts a quote saved for booking our margin
// (see ledger.ConversionAmounts): the Naira in and out on the NGN rail, or
// the market value of what is received on any other route. Each is nil
// when the quote has none.
func MarginAmountsOf(quote *models.Quote) (nairaIn *money.Amount, nairaOut *money.Amount, marketReceived *money.Amount) {
	parse := func(text *string) *money.Amount {
		if text == nil {
			return nil
		}
		amount, err := money.ParseUnits(*text)
		if err != nil {
			return nil
		}
		return &amount
	}
	nairaIn, nairaOut = parse(quote.NairaInAmount), parse(quote.NairaOutAmount)
	if nairaIn == nil || nairaOut == nil {
		nairaIn, nairaOut = nil, nil
	}
	return nairaIn, nairaOut, parse(quote.MarketToAmount)
}

// RouteOf decodes the route saved on a quote.
func RouteOf(quote *models.Quote) []RouteLeg {
	var route []RouteLeg
	json.Unmarshal(quote.Route, &route)
	return route
}
