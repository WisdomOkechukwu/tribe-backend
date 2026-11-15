package products

import (
	"fmt"

	"gorm.io/gorm"

	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/app/services/ledger"
	"github.com/tribe-africa/tribe-backend/pkg/money"
)

// productVault returns the vault of the provider that delivers a product, in
// one currency: the biller's provider pays bills out of its Naira vault, the
// gift card provider buys cards out of its dollar vault, the card issuer
// holds the dollars behind every card.
//
// providerID is the product's own provider (billers.provider_id,
// gift_card_brands.provider_id). When it is not set, the first enabled
// provider of that category is used.
func (service *Service) productVault(query *gorm.DB, providerID *string, category string, assetCode string) (*models.LedgerAccount, error) {
	if providerID != nil && *providerID != "" {
		return service.ledger.ProviderVault(query, *providerID, assetCode)
	}
	var provider models.Provider
	if err := query.Where("category = ? AND enabled = ?", category, true).Order("name").Take(&provider).Error; err != nil {
		return nil, fmt.Errorf("no enabled %s provider holds %s: %w", category, assetCode, err)
	}
	return service.ledger.ProviderVault(query, provider.ID, assetCode)
}

// cardMoneyLegs moves the dollars behind a card between the home provider
// of the card's currency and the card issuer, so the issuer's vault always
// equals the balances on its cards:
//
//	toIssuer = true  (funding a card):   Dr Card Issuer vault / Cr home vault
//	toIssuer = false (withdraw, delete): Dr home vault / Cr Card Issuer vault
func (service *Service) cardMoneyLegs(query *gorm.DB, assetCode string, amount money.Amount, toIssuer bool) ([]ledger.Leg, error) {
	if !amount.IsPositive() {
		return nil, nil
	}
	homeVault, err := service.ledger.HomeVault(query, assetCode)
	if err != nil {
		return nil, err
	}
	issuerVault, err := service.productVault(query, nil, models.ProviderCategoryVirtualCard, assetCode)
	if err != nil {
		return nil, err
	}
	if homeVault.ID == issuerVault.ID {
		return nil, nil
	}
	if toIssuer {
		return ledger.VaultTransferLegs(homeVault.ID, issuerVault.ID, amount, "Card money sent to the card issuer"), nil
	}
	return ledger.VaultTransferLegs(issuerVault.ID, homeVault.ID, amount, "Card money returned by the card issuer"), nil
}
