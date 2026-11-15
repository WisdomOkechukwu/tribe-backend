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

	marketReceived := money.FromUnits(7_900) // the market value; the user gets 7_800, so 100 is our margin
	marketLoss := money.FromUnits(7_750)     // the market value is BELOW what the user gets: a loss
	conversionAccounts := ConversionAccounts{Source: 1, SourceVault: 2, SourceFunds: 3, NairaVault: 10, NairaFunds: 11, TargetVault: 20, TargetFunds: 22, Target: 21}
	conversionAssets := accountsFor(map[uint64]string{1: "USD", 2: "USD", 3: "USD", 10: "NGN", 11: "NGN", 20: "GBP", 21: "GBP", 22: "GBP"})

	cases := []struct {
		name     string
		legs     []Leg
		accounts map[uint64]models.LedgerAccount
	}{
		{"deposit into a provider vault", DepositLegs(1, 2, amount), usdOnly},
		{"treasury funding of a provider vault", TreasuryFundingLegs(1, 2, amount), usdOnly},
		{"customer credit from Tribe Funds", CustomerCreditLegs(1, 2, amount), usdOnly},
		{"customer debit back into Tribe Funds", CustomerDebitLegs(1, 2, amount), usdOnly},
		{"internal transfer with fee", InternalTransferLegs(1, 2, 3, amount, fee), usdOnly},
		{"internal transfer without fee", InternalTransferLegs(1, 2, 3, amount, money.Zero()), usdOnly},
		{"payout with fee", PayoutLegs(1, 2, 3, amount, fee), usdOnly},
		{"payout refused", PayoutFailedLegs(1, 2, amount), usdOnly},
		{"vault to vault transfer", VaultTransferLegs(1, 2, amount, "Card money"), usdOnly},
		{"standalone fee", FeeLegs(1, 2, fee, "Card fee"), usdOnly},
		{"move", MoveLegs(1, 2, amount, "Card top-up"), usdOnly},
		{"direct conversion without a market value", ConversionLegs(conversionAccounts, ConversionAmounts{SourceAmount: amount, Fee: fee, TargetAmount: money.FromUnits(7_800)}), conversionAssets},
		{"direct conversion with its margin", ConversionLegs(conversionAccounts, ConversionAmounts{SourceAmount: amount, Fee: fee, TargetAmount: money.FromUnits(7_800), MarketReceived: &marketReceived}), conversionAssets},
		{"direct conversion at a loss", ConversionLegs(conversionAccounts, ConversionAmounts{SourceAmount: amount, TargetAmount: money.FromUnits(7_800), MarketReceived: &marketLoss}), conversionAssets},
		{"NGN rail with margin", ConversionLegs(conversionAccounts, ConversionAmounts{SourceAmount: amount, Fee: fee, TargetAmount: money.FromUnits(7_800), NairaIn: &nairaIn, NairaOut: &nairaOut}), conversionAssets},
		{"NGN rail at a loss", ConversionLegs(conversionAccounts, ConversionAmounts{SourceAmount: amount, TargetAmount: money.FromUnits(7_800), NairaIn: &nairaIn, NairaOut: &nairaLoss}), conversionAssets},
		{"NGN rail then a second margin (gift card sold for another currency)", ConversionLegs(conversionAccounts, ConversionAmounts{SourceAmount: amount, TargetAmount: money.FromUnits(7_800), NairaIn: &nairaIn, NairaOut: &nairaOut, MarketReceived: &marketReceived}), conversionAssets},
		{"purchase refused: reversal of a same-asset purchase", ReversalLegs(PayoutLegs(1, 2, 3, amount, fee)), usdOnly},
		{"purchase refused: reversal of an NGN rail purchase", ReversalLegs(ConversionLegs(conversionAccounts, ConversionAmounts{SourceAmount: amount, Fee: fee, TargetAmount: money.FromUnits(7_800), NairaIn: &nairaIn, NairaOut: &nairaOut})), conversionAssets},
		{"referral reward from Tribe Funds", ReferralRewardLegs(10, 11, money.FromUnits(4_620)), conversionAssets},
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
	original := PayoutLegs(1, 2, 3, money.FromUnits(10_000), money.FromUnits(150))
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

// TestMarginsLandInTribeFunds checks that a conversion credits the fee and
// the margin to Tribe Funds of the currency each was earned in, tags those
// legs with where the money came from, and moves the provider vaults by
// what the providers really sold and bought.
func TestMarginsLandInTribeFunds(test *testing.T) {
	accounts := ConversionAccounts{Source: 1, SourceVault: 2, SourceFunds: 3, NairaVault: 10, NairaFunds: 11, TargetVault: 20, TargetFunds: 22, Target: 21}
	marketReceived := money.FromUnits(7_900)
	legs := ConversionLegs(accounts, ConversionAmounts{SourceAmount: money.FromUnits(10_000), Fee: money.FromUnits(150), TargetAmount: money.FromUnits(7_800), MarketReceived: &marketReceived})

	seen := map[uint64]string{}
	for _, leg := range legs {
		seen[leg.AccountID] = leg.Side + " " + leg.Amount.UnitsString() + " " + leg.Source
		if leg.Source != "" && leg.AccountID != 3 && leg.AccountID != 22 {
			test.Errorf("leg on account %d is tagged %q, only Tribe Funds legs carry a source", leg.AccountID, leg.Source)
		}
	}
	want := map[uint64]string{
		2:  "credit 10000 ",     // the source provider sold the converted amount
		3:  "credit 150 fee",    // the fee, in the from-asset
		20: "debit 7900 ",       // the target provider bought the market value
		22: "credit 100 margin", // 7900 at market - 7800 to the user
	}
	for accountID, expected := range want {
		if seen[accountID] != expected {
			test.Errorf("account %d: got %q, want %q", accountID, seen[accountID], expected)
		}
	}
}

// TestTribeFundsAlwaysEqualsVaultsMinusOwed is the rule the whole ledger
// rests on: in every currency, Tribe Funds = what the providers hold - what
// we owe customers. It replays every kind of posting on a small chart and
// checks the rule after each one.
func TestTribeFundsAlwaysEqualsVaultsMinusOwed(test *testing.T) {
	const (
		homeUsd, issuerUsd, billsNgn, homeNgn, homeGbp = 1, 2, 3, 4, 5 // provider vaults (assets)
		adaUsd, adaNgn, adaGbp, adaCard, adaCashpoint  = 11, 12, 13, 14, 15
		bolaUsd, bolaNgn, bolaReferral                 = 21, 22, 23
		fundsUsd, fundsNgn, fundsGbp                   = 31, 32, 33 // Tribe Funds (equity)
	)
	type kind int
	const (
		vault kind = iota
		owed
		funds
	)
	accounts := map[uint64]struct {
		asset string
		kind  kind
	}{
		homeUsd: {"USD", vault}, issuerUsd: {"USD", vault}, billsNgn: {"NGN", vault}, homeNgn: {"NGN", vault}, homeGbp: {"GBP", vault},
		adaUsd: {"USD", owed}, adaNgn: {"NGN", owed}, adaGbp: {"GBP", owed}, adaCard: {"USD", owed}, adaCashpoint: {"NGN", owed},
		bolaUsd: {"USD", owed}, bolaNgn: {"NGN", owed}, bolaReferral: {"NGN", owed},
		fundsUsd: {"USD", funds}, fundsNgn: {"NGN", funds}, fundsGbp: {"GBP", funds},
	}
	balances := map[uint64]int64{}
	apply := func(name string, legs []Leg) {
		for _, leg := range legs {
			account := accounts[leg.AccountID]
			units, _ := new(big.Int).SetString(leg.Amount.UnitsString(), 10)
			change := units.Int64()
			increasesWithDebit := account.kind == vault
			if (leg.Side == Debit) != increasesWithDebit {
				change = -change
			}
			balances[leg.AccountID] += change
		}
		totals := map[string][3]int64{}
		for accountID, account := range accounts {
			total := totals[account.asset]
			total[account.kind] += balances[accountID]
			totals[account.asset] = total
		}
		for asset, total := range totals {
			if total[vault]-total[owed] != total[funds] {
				test.Fatalf("after %s: %s vaults %d - owed %d = %d, but Tribe Funds is %d", name, asset, total[vault], total[owed], total[vault]-total[owed], total[funds])
			}
		}
	}
	units := money.FromUnits
	nairaIn, nairaOut, market := units(15_400_000), units(15_300_000), units(7_900)
	swap := ConversionAccounts{Source: adaUsd, SourceVault: homeUsd, SourceFunds: fundsUsd, NairaVault: homeNgn, NairaFunds: fundsNgn, TargetVault: homeGbp, TargetFunds: fundsGbp, Target: adaGbp}

	apply("deposit", DepositLegs(homeUsd, adaUsd, units(50_000)))
	apply("treasury funding", TreasuryFundingLegs(billsNgn, fundsNgn, units(100_000_000)))
	apply("Tribe send with fee", InternalTransferLegs(adaUsd, bolaUsd, fundsUsd, units(2_000), units(10)))
	apply("NGN rail swap", ConversionLegs(swap, ConversionAmounts{SourceAmount: units(10_000), Fee: units(50), TargetAmount: units(7_800), NairaIn: &nairaIn, NairaOut: &nairaOut}))
	apply("direct swap with margin", ConversionLegs(swap, ConversionAmounts{SourceAmount: units(10_000), TargetAmount: units(7_800), MarketReceived: &market}))
	apply("card funding", append(MoveLegs(adaUsd, adaCard, units(5_000), "Card top-up"), VaultTransferLegs(homeUsd, issuerUsd, units(5_000), "Card money")...))
	apply("Naira deposit", DepositLegs(homeNgn, bolaNgn, units(600_000)))
	apply("bill paid from the bills vault", MoveLegs(bolaNgn, billsNgn, units(500_000), "Bill"))
	apply("payout", PayoutLegs(bolaUsd, homeUsd, fundsUsd, units(1_000), units(25)))
	apply("payout refused", PayoutFailedLegs(homeUsd, bolaUsd, units(1_000)))
	apply("cashpoint created", MoveLegs(adaNgn, adaCashpoint, units(1_000), "Cashpoint"))
	apply("manual credit", CustomerCreditLegs(fundsNgn, adaNgn, units(10_000)))
	apply("manual debit", CustomerDebitLegs(adaNgn, fundsNgn, units(5_000)))
	apply("referral reward", ReferralRewardLegs(fundsNgn, bolaReferral, units(2_000)))
	apply("card closed", append(MoveLegs(adaCard, adaUsd, units(5_000), "Card closed"), VaultTransferLegs(issuerUsd, homeUsd, units(5_000), "Card money")...))
}
