package ledger

import (
	"gorm.io/gorm"

	"github.com/tribe-africa/tribe-backend/pkg/money"
)

// ConversionPosting describes a conversion to post. Swaps, card funding,
// bills and gift cards paid with another currency all use it.
type ConversionPosting struct {
	Reference             string
	TransactionType       string
	Description           string
	CreatedBy             string
	BusinessTransactionID *string
	SourceAccountID       uint64 // where the money comes from
	TargetAccountID       uint64 // where the converted money goes
	FromAsset             string
	ToAsset               string
	Converted             money.Amount // from-asset amount converted (fee NOT included)
	Fee                   money.Amount // from-asset fee
	Received              money.Amount // to-asset amount that arrives
	NairaIn               *money.Amount
	NairaOut              *money.Amount
	Rates                 map[string][2]string // asset -> [usd, ngn] at this moment
	Metadata              map[string]any
}

// PostConversion finds the company accounts a conversion needs and posts it.
// If both sides are the same asset, it simply moves the money (plus fee).
func (service *Service) PostConversion(databaseTransaction *gorm.DB, posting ConversionPosting) (*PostResult, error) {
	entry := Entry{
		Reference: posting.Reference, TransactionType: posting.TransactionType, Description: posting.Description,
		CreatedBy: posting.CreatedBy, BusinessTransactionID: posting.BusinessTransactionID, Metadata: posting.Metadata,
		Rates: map[string]AssetRate{},
	}
	for assetCode, pair := range posting.Rates {
		entry.Rates[assetCode] = AssetRate{Usd: pair[0], Ngn: pair[1]}
	}

	sourceFeeRevenue, err := service.CompanyAccount(databaseTransaction, CodeFeeRevenue, posting.FromAsset)
	if err != nil {
		return nil, err
	}

	// Same asset: no conversion, just move it.
	if posting.FromAsset == posting.ToAsset {
		entry.Legs = MoveLegs(posting.SourceAccountID, posting.TargetAccountID, posting.Converted, posting.Description)
		if posting.Fee.IsPositive() {
			entry.Legs = append(entry.Legs, FeeLegs(posting.SourceAccountID, sourceFeeRevenue.ID, posting.Fee, "Fee")...)
		}
		return service.Post(databaseTransaction, entry)
	}

	sourceClearing, err := service.CompanyAccount(databaseTransaction, CodeSwapClearing, posting.FromAsset)
	if err != nil {
		return nil, err
	}
	targetClearing, err := service.CompanyAccount(databaseTransaction, CodeSwapClearing, posting.ToAsset)
	if err != nil {
		return nil, err
	}
	accounts := ConversionAccounts{
		Source: posting.SourceAccountID, SourceClearing: sourceClearing.ID, SourceFeeRevenue: sourceFeeRevenue.ID,
		TargetClearing: targetClearing.ID, Target: posting.TargetAccountID,
	}
	if posting.NairaIn != nil && posting.NairaOut != nil {
		nairaClearing, err := service.CompanyAccount(databaseTransaction, CodeSwapClearing, "NGN")
		if err != nil {
			return nil, err
		}
		nairaMargin, err := service.CompanyAccount(databaseTransaction, CodeFxMarginRevenue, "NGN")
		if err != nil {
			return nil, err
		}
		accounts.NairaClearing, accounts.NairaMargin = nairaClearing.ID, nairaMargin.ID
	}
	entry.Legs = ConversionLegs(accounts, ConversionAmounts{
		SourceAmount: posting.Converted, Fee: posting.Fee, TargetAmount: posting.Received,
		NairaIn: posting.NairaIn, NairaOut: posting.NairaOut,
	})
	return service.Post(databaseTransaction, entry)
}
