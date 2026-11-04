package ledger

import (
	"math/big"
	"testing"

	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/pkg/money"
)

// accountsFor gives every account id an asset, so checkBalancedPerAsset can
// check each currency on its own.
func accountsFor(assetByAccount map[uint64]string) map[uint64]models.LedgerAccount {
	accounts := map[uint64]models.LedgerAccount{}
	for accountID, assetCode := range assetByAccount {
		accounts[accountID] = models.LedgerAccount{ID: accountID, AssetCode: assetCode}
	}
	return accounts
}

// TestEveryRuleBalances proves that every posting rule writes legs whose
// debits equal credits for each asset, with and without fees.
func TestEveryRuleBalances(test *testing.T) {
	usdOnly := accountsFor(map[uint64]string{1: "USD", 2: "USD", 3: "USD", 4: "USD"})
	amount := money.FromUnits(10_000) // 100.00
	fee := money.FromUnits(150)       // 1.50
	nairaIn := money.FromUnits(15_400_000)
	nairaOut := money.FromUnits(15_300_000)
	nairaLoss := money.FromUnits(15_500_000)

	conversionAccounts := ConversionAccounts{Source: 1, SourceClearing: 2, SourceFeeRevenue: 3, NairaClearing: 10, NairaMargin: 11, TargetClearing: 20, Target: 21}
	conversionAssets := accountsFor(map[uint64]string{1: "USD", 2: "USD", 3: "USD", 10: "NGN", 11: "NGN", 20: "GBP", 21: "GBP"})

	cases := []struct {
		name     string
		legs     []Leg
		accounts map[uint64]models.LedgerAccount
	}{
		{"deposit", DepositLegs(1, 2, amount), usdOnly},
		{"treasury funding", TreasuryFundingLegs(1, 2, amount), usdOnly},
		{"customer credit", CustomerCreditLegs(1, 2, amount), usdOnly},
		{"customer debit", CustomerDebitLegs(1, 2, amount), usdOnly},
		{"internal transfer with fee", InternalTransferLegs(1, 2, 3, amount, fee), usdOnly},
		{"internal transfer without fee", InternalTransferLegs(1, 2, 3, amount, money.Zero()), usdOnly},
		{"payout start with fee", PayoutStartLegs(1, 2, 3, amount, fee), usdOnly},
		{"payout succeeded", PayoutSucceededLegs(1, 2, amount), usdOnly},
		{"payout failed", PayoutFailedLegs(1, 2, amount), usdOnly},
		{"standalone fee", FeeLegs(1, 2, fee, "Card fee"), usdOnly},
		{"move", MoveLegs(1, 2, amount, "Card top-up"), usdOnly},
		{"direct conversion", ConversionLegs(conversionAccounts, ConversionAmounts{SourceAmount: amount, Fee: fee, TargetAmount: money.FromUnits(7_800)}), conversionAssets},
		{"NGN rail with margin", ConversionLegs(conversionAccounts, ConversionAmounts{SourceAmount: amount, Fee: fee, TargetAmount: money.FromUnits(7_800), NairaIn: &nairaIn, NairaOut: &nairaOut}), conversionAssets},
		{"NGN rail at a loss", ConversionLegs(conversionAccounts, ConversionAmounts{SourceAmount: amount, TargetAmount: money.FromUnits(7_800), NairaIn: &nairaIn, NairaOut: &nairaLoss}), conversionAssets},
		{"purchase succeeded", PurchaseSucceededLegs(1, 2, amount), usdOnly},
		{"purchase refused: reversal of a same-asset purchase", ReversalLegs(PayoutStartLegs(1, 2, 3, amount, fee)), usdOnly},
		{"purchase refused: reversal of an NGN rail purchase", ReversalLegs(ConversionLegs(conversionAccounts, ConversionAmounts{SourceAmount: amount, Fee: fee, TargetAmount: money.FromUnits(7_800), NairaIn: &nairaIn, NairaOut: &nairaOut})), conversionAssets},
	}
	for _, testCase := range cases {
		test.Run(testCase.name, func(test *testing.T) {
			if err := validateLegs(testCase.legs); err != nil {
				test.Fatalf("legs are not valid: %v", err)
			}
			if err := checkBalancedPerAsset(testCase.legs, testCase.accounts); err != nil {
				test.Fatalf("does not balance: %v", err)
			}
		})
	}
}

// TestReversalLegsMirrorTheOriginal checks that a reversal flips every side
// and keeps every account and amount, so original + reversal nets to zero
// on every account.
func TestReversalLegsMirrorTheOriginal(test *testing.T) {
	original := PayoutStartLegs(1, 2, 3, money.FromUnits(10_000), money.FromUnits(150))
	reversed := ReversalLegs(original)
	if len(reversed) != len(original) {
		test.Fatalf("reversal has %d legs, original %d", len(reversed), len(original))
	}
	net := map[uint64]int64{}
	for _, leg := range append(append([]Leg{}, original...), reversed...) {
		units, _ := money.ParseUnits(leg.Amount.UnitsString())
		value, _ := new(big.Int).SetString(units.UnitsString(), 10)
		if leg.Side == Debit {
			net[leg.AccountID] += value.Int64()
		} else {
			net[leg.AccountID] -= value.Int64()
		}
	}
	for accountID, total := range net {
		if total != 0 {
			test.Errorf("account %d nets to %d after original + reversal, want 0", accountID, total)
		}
	}
}

// TestUnbalancedEntryIsRefused makes sure the balance check really catches mistakes.
func TestUnbalancedEntryIsRefused(test *testing.T) {
	legs := []Leg{
		{AccountID: 1, Side: Debit, Amount: money.FromUnits(100)},
		{AccountID: 2, Side: Credit, Amount: money.FromUnits(99)},
	}
	if err := checkBalancedPerAsset(legs, accountsFor(map[uint64]string{1: "USD", 2: "USD"})); err == nil {
		test.Fatal("an entry with 100 debit and 99 credit was accepted")
	}
	// Balanced in total but across two currencies: still wrong.
	crossCurrency := []Leg{
		{AccountID: 1, Side: Debit, Amount: money.FromUnits(100)},
		{AccountID: 2, Side: Credit, Amount: money.FromUnits(100)},
	}
	if err := checkBalancedPerAsset(crossCurrency, accountsFor(map[uint64]string{1: "USD", 2: "GBP"})); err == nil {
		test.Fatal("USD debit balanced by a GBP credit was accepted")
	}
}

// TestInvalidLegsAreRefused covers the basic shape checks.
func TestInvalidLegsAreRefused(test *testing.T) {
	cases := map[string][]Leg{
		"one leg":     {{AccountID: 1, Side: Debit, Amount: money.FromUnits(1)}},
		"zero amount": {{AccountID: 1, Side: Debit, Amount: money.Zero()}, {AccountID: 2, Side: Credit, Amount: money.Zero()}},
		"bad side":    {{AccountID: 1, Side: "sideways", Amount: money.FromUnits(1)}, {AccountID: 2, Side: Credit, Amount: money.FromUnits(1)}},
		"no account":  {{AccountID: 0, Side: Debit, Amount: money.FromUnits(1)}, {AccountID: 2, Side: Credit, Amount: money.FromUnits(1)}},
	}
	for name, legs := range cases {
		if err := validateLegs(legs); err == nil {
			test.Errorf("%s: was accepted", name)
		}
	}
}

// TestNormalSideFollowsTheNexumTable checks every category against the table in accounts.go.
func TestNormalSideFollowsTheNexumTable(test *testing.T) {
	expected := map[string]string{
		CategoryAsset: Debit, CategoryExpense: Debit, CategoryContraAsset: Debit, CategorySuspenseAsset: Debit,
		CategoryLiability: Credit, CategoryRevenue: Credit, CategoryEquity: Credit,
		CategoryContraLiability: Credit, CategorySuspenseLiability: Credit, CategorySummary: Credit,
	}
	for category, side := range expected {
		if got := NormalSideForCategory(category); got != side {
			test.Errorf("%s increases with %s, want %s", category, got, side)
		}
	}
}

// TestLocksAreTakenInAscendingOrder: Post locks accounts in this order, which
// is what stops two opposite transfers from deadlocking.
func TestLocksAreTakenInAscendingOrder(test *testing.T) {
	legs := []Leg{{AccountID: 9}, {AccountID: 3}, {AccountID: 9}, {AccountID: 5}}
	got := uniqueAccountIDs(legs)
	want := []uint64{3, 5, 9}
	if len(got) != len(want) {
		test.Fatalf("got %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			test.Fatalf("got %v, want %v", got, want)
		}
	}
}
