package ledger

import (
	"fmt"
	"math/big"
	"sort"

	"gorm.io/gorm"

	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/pkg/money"
)

// referralShareLegs works out the inviter's share of what an entry earns and
// returns it in Naira, plus (with PayNow) the legs that pay it from Tribe
// Funds NGN into the inviter's referral balance.
//
// "What the entry earns" is read from the entry's own legs: in each currency,
// the fee and margin credits to Tribe Funds minus its fee and margin debits
// (treasury top-ups and manual adjustments are not earnings). So every kind
// of earning (a fee, a margin on any route, a gift card spread) is shared the
// same way, and a new kind cannot be forgotten. A currency where the entry
// LOST money (a negative margin) gives no share.
//
// Example, 2% share: a USDT -> USDC swap earns a 0.50 USDT fee and a
// 0.20 USDC margin. The share is 0.01 USDT and 0.004 USDC, valued in Naira
// at the market rate; the inviter is paid that many Naira out of Tribe Funds
// NGN. Tribe's USDT and USDC earnings are not converted: they stay where they are.
//
// Rounding: every share rounds DOWN (it is money paid to a user).
// The reads here do not lock anything; Post locks every account afterwards.
func (service *Service) referralShareLegs(query *gorm.DB, entry Entry) ([]Leg, money.Amount, error) {
	total := money.Zero()
	share := entry.ReferralShare
	if share.Percent == nil || share.Percent.Sign() <= 0 {
		return nil, total, nil
	}

	accountIDs := uniqueAccountIDs(entry.Legs)
	var accounts []models.LedgerAccount
	if err := query.Where("id IN ?", accountIDs).Find(&accounts).Error; err != nil {
		return nil, total, err
	}
	tribeFunds := map[uint64]models.LedgerAccount{}
	for _, account := range accounts {
		if account.OwnerKey == ownerCompany && account.Code == CodeTribeFunds {
			tribeFunds[account.ID] = account
		}
	}

	// Net earnings per currency (credits count +, debits count -).
	earnedByAsset := map[string]money.Amount{}
	for _, leg := range entry.Legs {
		account, isTribeFunds := tribeFunds[leg.AccountID]
		if !isTribeFunds || (leg.Source != SourceFee && leg.Source != SourceMargin) {
			continue
		}
		earned, found := earnedByAsset[account.AssetCode]
		if !found {
			earned = money.Zero()
		}
		if leg.Side == Credit {
			earned = earned.Add(leg.Amount)
		} else {
			earned = earned.Subtract(leg.Amount)
		}
		earnedByAsset[account.AssetCode] = earned
	}
	if len(earnedByAsset) == 0 {
		return nil, total, nil
	}

	assetCodes := make([]string, 0, len(earnedByAsset))
	for assetCode := range earnedByAsset {
		assetCodes = append(assetCodes, assetCode)
	}
	sort.Strings(assetCodes)
	decimals, err := assetDecimalPlaces(query, append(assetCodes, ReferralAsset))
	if err != nil {
		return nil, total, err
	}
	fraction := new(big.Rat).Quo(share.Percent, big.NewRat(100, 1))

	for _, assetCode := range assetCodes {
		earned := earnedByAsset[assetCode]
		if !earned.IsPositive() {
			continue
		}
		// share = earned x percent, in smallest units, rounded down.
		shareUnits := money.FromRational(new(big.Rat).Mul(earned.ToRational(0), fraction), 0, money.RoundDown)
		if !shareUnits.IsPositive() {
			continue
		}
		shareInNaira := shareUnits
		if assetCode != ReferralAsset {
			rate, found := entry.Rates[assetCode]
			if !found || rate.Ngn == "" {
				return nil, total, fmt.Errorf("posting %s: the referral share needs the NGN rate of %s", entry.Reference, assetCode)
			}
			nairaPerUnit, err := money.ParseRate(rate.Ngn)
			if err != nil {
				return nil, total, err
			}
			nairaValue := new(big.Rat).Mul(shareUnits.ToRational(decimals[assetCode]), nairaPerUnit)
			shareInNaira = money.FromRational(nairaValue, decimals[ReferralAsset], money.RoundDown)
		}
		total = total.Add(shareInNaira)
	}

	if !share.PayNow || !total.IsPositive() {
		return nil, total, nil
	}
	if share.InviterAccountID == 0 {
		return nil, total, fmt.Errorf("posting %s: a referral share paid now needs the inviter's account", entry.Reference)
	}
	nairaFunds, err := service.TribeFunds(query, ReferralAsset)
	if err != nil {
		return nil, total, err
	}
	return ReferralRewardLegs(nairaFunds.ID, share.InviterAccountID, total), total, nil
}

// assetDecimalPlaces reads how many decimal places each asset has.
func assetDecimalPlaces(query *gorm.DB, assetCodes []string) (map[string]int, error) {
	var assets []models.Asset
	if err := query.Select("code", "decimal_places").Where("code IN ?", assetCodes).Find(&assets).Error; err != nil {
		return nil, err
	}
	decimals := make(map[string]int, len(assets))
	for _, asset := range assets {
		decimals[asset.Code] = asset.DecimalPlaces
	}
	for _, assetCode := range assetCodes {
		if _, found := decimals[assetCode]; !found {
			return nil, fmt.Errorf("unknown asset %s", assetCode)
		}
	}
	return decimals, nil
}

// RatesFromPairs turns the [usd, ngn] pairs that rates.RatesFor returns into
// the rates an Entry stores on its legs.
func RatesFromPairs(pairs map[string][2]string) map[string]AssetRate {
	result := make(map[string]AssetRate, len(pairs))
	for assetCode, pair := range pairs {
		result[assetCode] = AssetRate{Usd: pair[0], Ngn: pair[1]}
	}
	return result
}
