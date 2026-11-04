package rates

import (
	"context"
	"encoding/json"
	"time"

	"gorm.io/gorm"

	"github.com/tribe-africa/tribe-backend/app/apperrors"
	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/pkg/ids"
)

// CreateQuote prices a conversion and SAVES it, so the user can confirm at
// exactly this price within QUOTE_LIFETIME_SECONDS (30 s by default).
func (service *Service) CreateQuote(requestContext context.Context, userID string, input PriceInput) (*models.Quote, *Price, error) {
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
	if err := service.database.WithContext(requestContext).Create(&quote).Error; err != nil {
		return nil, nil, err
	}
	return &quote, price, nil
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

// RouteOf decodes the route saved on a quote.
func RouteOf(quote *models.Quote) []RouteLeg {
	var route []RouteLeg
	json.Unmarshal(quote.Route, &route)
	return route
}
