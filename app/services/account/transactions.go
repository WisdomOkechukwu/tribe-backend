package account

import (
	"context"
	"time"

	"github.com/tribe-africa/tribe-backend/app/apperrors"
	"github.com/tribe-africa/tribe-backend/app/models"
)

// TransactionFilters are the filters on the activity list.
type TransactionFilters struct {
	Category string // currency | virtual_card | cashpoint | utility | giftcard | referral
	Type     string
	Asset    string
	CardID   string
	Cursor   string // created_at of the last item seen (RFC3339), for "load more"
	Limit    int
}

// Transactions lists the user's transactions, newest first.
//
// It uses CURSOR pagination: the app sends the created_at of the last item
// it has, and we return the items older than that. Unlike page numbers,
// this never skips or repeats items when new transactions arrive.
func (service *Service) Transactions(requestContext context.Context, userID string, filters TransactionFilters) ([]models.Transaction, string, error) {
	if filters.Limit <= 0 || filters.Limit > 100 {
		filters.Limit = 30
	}
	query := service.database.WithContext(requestContext).Where("user_id = ?", userID)
	if filters.Category != "" {
		query = query.Where("category = ?", filters.Category)
	}
	if filters.Type != "" {
		query = query.Where("type = ?", filters.Type)
	}
	if filters.Asset != "" {
		query = query.Where("(from_asset = ? OR to_asset = ?)", filters.Asset, filters.Asset)
	}
	if filters.CardID != "" {
		query = query.Where("JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.card_id')) = ?", filters.CardID)
	}
	if filters.Cursor != "" {
		if cursorTime, err := time.Parse(time.RFC3339Nano, filters.Cursor); err == nil {
			query = query.Where("created_at < ?", cursorTime)
		}
	}
	var transactions []models.Transaction
	if err := query.Order("created_at DESC").Limit(filters.Limit + 1).Find(&transactions).Error; err != nil {
		return nil, "", err
	}
	nextCursor := ""
	if len(transactions) > filters.Limit {
		transactions = transactions[:filters.Limit]
		nextCursor = transactions[len(transactions)-1].CreatedAt.Format(time.RFC3339Nano)
	}
	return transactions, nextCursor, nil
}

// Transaction returns one of the user's transactions with its route legs.
func (service *Service) Transaction(requestContext context.Context, userID string, transactionID string) (*models.Transaction, []models.SwapLeg, error) {
	var transaction models.Transaction
	err := service.database.WithContext(requestContext).Where("(id = ? OR reference = ?) AND user_id = ?", transactionID, transactionID, userID).Take(&transaction).Error
	if err != nil {
		return nil, nil, apperrors.NotFound("Transaction")
	}
	var legs []models.SwapLeg
	service.database.WithContext(requestContext).Where("transaction_id = ?", transaction.ID).Order("step").Find(&legs)
	return &transaction, legs, nil
}
