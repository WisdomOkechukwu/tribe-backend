package ledger

import (
	"gorm.io/gorm"

	"github.com/tribe-africa/tribe-backend/pkg/money"
)

// ConversionPosting describes a conversion to post. Swaps, card funding,
// bills, gift cards and payouts paid with another currency all use it.
type ConversionPosting struct {
	Reference             string
	TransactionType       string
	Description           string
	CreatedBy             string
	BusinessTransactionID *string
	SourceAccountID       uint64 // where the money comes from
	TargetAccountID       uint64 // where the converted money goes (a wallet, a card, or a provider vault that pays someone)
	FromAsset             string
	ToAsset               string
	Converted             money.Amount // from-asset amount converted (fee NOT included)
	Fee                   money.Amount // from-asset fee
	Received              money.Amount // to-asset amount that arrives
	NairaIn               *money.Amount
	NairaOut              *money.Amount
	MarketReceived        *money.Amount        // direct routes: to-asset amount at market (see ConversionAmounts)
	Rates                 map[string][2]string // asset -> [usd, ngn] at this moment
	Metadata              map[string]any
	ReferralShare         *ReferralShare // set when the customer was invited (see Entry.ReferralShare)
	// ExtraLegs are added to the same entry, for example the transfer of the
	// dollars that back a card from the home provider to the card issuer.
	ExtraLegs []Leg
}

// PostConversion finds the accounts a conversion needs and posts it. The
// from-asset leaves its HOME provider vault and the to-asset arrives in its
// home vault (HomeVault); fees and margins go to Tribe Funds. If both sides
// are the same asset, it simply moves the money (plus fee).
func (service *Service) PostConversion(databaseTransaction *gorm.DB, posting ConversionPosting) (*PostResult, error) {
	entry := Entry{
		Reference: posting.Reference, TransactionType: posting.TransactionType, Description: posting.Description,
		CreatedBy: posting.CreatedBy, BusinessTransactionID: posting.BusinessTransactionID, Metadata: posting.Metadata,
		Rates: RatesFromPairs(posting.Rates), ReferralShare: posting.ReferralShare,
	}

	sourceFunds, err := service.TribeFunds(databaseTransaction, posting.FromAsset)
	if err != nil {
		return nil, err
	}

	// Same asset: no conversion, just move it.
	if posting.FromAsset == posting.ToAsset {
		entry.Legs = MoveLegs(posting.SourceAccountID, posting.TargetAccountID, posting.Converted, posting.Description)
		if posting.Fee.IsPositive() {
			entry.Legs = append(entry.Legs, FeeLegs(posting.SourceAccountID, sourceFunds.ID, posting.Fee, "Fee")...)
		}
		entry.Legs = append(entry.Legs, posting.ExtraLegs...)
		return service.Post(databaseTransaction, entry)
	}

	sourceVault, err := service.HomeVault(databaseTransaction, posting.FromAsset)
	if err != nil {
		return nil, err
	}
	targetVault, err := service.HomeVault(databaseTransaction, posting.ToAsset)
	if err != nil {
		return nil, err
	}
	targetFunds, err := service.TribeFunds(databaseTransaction, posting.ToAsset)
	if err != nil {
		return nil, err
	}
	accounts := ConversionAccounts{
		Source: posting.SourceAccountID, SourceVault: sourceVault.ID, SourceFunds: sourceFunds.ID,
		TargetVault: targetVault.ID, TargetFunds: targetFunds.ID, Target: posting.TargetAccountID,
	}
	if posting.NairaIn != nil && posting.NairaOut != nil {
		nairaVault, err := service.HomeVault(databaseTransaction, "NGN")
		if err != nil {
			return nil, err
		}
		nairaFunds, err := service.TribeFunds(databaseTransaction, "NGN")
		if err != nil {
			return nil, err
		}
		accounts.NairaVault, accounts.NairaFunds = nairaVault.ID, nairaFunds.ID
	}
	entry.Legs = ConversionLegs(accounts, ConversionAmounts{
		SourceAmount: posting.Converted, Fee: posting.Fee, TargetAmount: posting.Received,
		NairaIn: posting.NairaIn, NairaOut: posting.NairaOut, MarketReceived: posting.MarketReceived,
	})
	entry.Legs = append(entry.Legs, posting.ExtraLegs...)
	return service.Post(databaseTransaction, entry)
}
