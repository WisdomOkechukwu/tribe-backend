package integration

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/pkg/ids"
	"github.com/tribe-africa/tribe-backend/pkg/money"
	"github.com/tribe-africa/tribe-backend/tests/testenv"
)

// TestSnapshotPlusLaterLegsEqualsBalance writes a daily snapshot in the
// middle of activity. A balance is "latest snapshot + legs after it", so
// balances must read the same before and after the snapshot, and keep
// adding up as more money moves. ledger.Verify must find no problems.
func TestSnapshotPlusLaterLegsEqualsBalance(test *testing.T) {
	environment := testenv.Start(test)
	user := environment.RegisterUser(test, "snapuser")
	environment.RegisterUser(test, "snapfriend")
	user.TopUp(test, "USD", "50")
	send := func(amount string) {
		response := user.Call(test, http.MethodPost, "/api/mobile/v1/sends", map[string]any{
			"recipient_kind": "tribe", "handle": "@snapfriend", "asset": "USD", "amount": amount, "pin": testenv.DemoPin,
		}, testenv.NewKey())
		if !response.Success {
			test.Fatalf("send %s failed: %s %s", amount, response.Code, response.Message)
		}
	}
	send("5")
	before := user.Balance(test, "USD")

	today := time.Now().In(environment.App.Settings.App.TimeZone).Format(time.DateOnly)
	written, err := environment.App.Ledger.WriteDailySnapshots(context.Background(), today)
	if err != nil || written == 0 {
		test.Fatalf("snapshot wrote %d rows: %v", written, err)
	}
	if after := user.Balance(test, "USD"); after != before {
		test.Fatalf("balance changed from %s to %s just by taking a snapshot", before, after)
	}

	// Running it again is safe (UNIQUE account + date).
	if again, err := environment.App.Ledger.WriteDailySnapshots(context.Background(), today); err != nil || again != 0 {
		test.Fatalf("second snapshot run wrote %d rows: %v", again, err)
	}

	user.TopUp(test, "USD", "10")
	send("2.50")
	if balance := user.Balance(test, "USD"); balance != "52.50" {
		test.Errorf("balance = %s, want 52.50 (50 - 5 + 10 - 2.50)", balance)
	}
	assertBooksBalance(test, environment)
}

// TestSnapshotNeverSkipsALegStampedAfterMidnight recreates the midnight race.
// Post stamps created_at BEFORE it waits for the account locks, so a leg with
// a HIGHER id can carry an EARLIER time than the leg before it. The snapshot
// must cut by leg id, otherwise it records the later leg as "last" and every
// balance after that silently skips the earlier one.
//
// The legs are inserted directly (not through Post) so the test can choose
// their timestamps: entry 1 gets the lower ids but is stamped just AFTER
// midnight, entry 2 gets the higher ids but is stamped just BEFORE it.
func TestSnapshotNeverSkipsALegStampedAfterMidnight(test *testing.T) {
	environment := testenv.Start(test)
	user := environment.RegisterUser(test, "midnightuser")
	friend := environment.RegisterUser(test, "midnightfriend")
	user.TopUp(test, "USD", "50")

	walletOf := func(client *testenv.Client) models.LedgerAccount {
		var account models.LedgerAccount
		err := environment.App.Database.
			Joins("JOIN users ON users.id = ledger_accounts.user_id").
			Where("users.email = ? AND ledger_accounts.code = ? AND ledger_accounts.asset_code = ?", client.Email, "user_primary", "USD").
			Take(&account).Error
		if err != nil {
			test.Fatalf("find USD wallet of %s: %v", client.Email, err)
		}
		return account
	}
	userWallet := walletOf(user)
	friendWallet := walletOf(friend)

	timeZone := environment.App.Settings.App.TimeZone
	today := time.Now().In(timeZone)
	midnight := time.Date(today.Year(), today.Month(), today.Day()+1, 0, 0, 0, 0, timeZone).UTC()

	// moveOneDollar writes one balanced entry: 1.00 USD from the user's
	// wallet to the friend's, with both legs stamped at the given time.
	moveOneDollar := func(reference string, stampedAt time.Time) {
		entryID := ids.New()
		err := environment.App.Database.Transaction(func(databaseTransaction *gorm.DB) error {
			entry := models.JournalEntry{
				ID: entryID, Reference: reference, TransactionType: "test", Status: "posted",
				CreatedBy: "test", AccountingDate: stampedAt, PostedAt: stampedAt,
			}
			if err := databaseTransaction.Create(&entry).Error; err != nil {
				return err
			}
			legs := []models.JournalLeg{
				{JournalEntryID: entryID, LedgerAccountID: userWallet.ID, AssetCode: "USD", Side: "debit", Amount: "100", CreatedAt: stampedAt},
				{JournalEntryID: entryID, LedgerAccountID: friendWallet.ID, AssetCode: "USD", Side: "credit", Amount: "100", CreatedAt: stampedAt},
			}
			return databaseTransaction.Create(&legs).Error
		})
		if err != nil {
			test.Fatalf("insert entry %s: %v", reference, err)
		}
	}
	moveOneDollar("TEST-AFTER-MIDNIGHT", midnight.Add(time.Second))   // lower ids, later time
	moveOneDollar("TEST-BEFORE-MIDNIGHT", midnight.Add(-time.Second)) // higher ids, earlier time

	written, err := environment.App.Ledger.WriteDailySnapshots(context.Background(), today.Format(time.DateOnly))
	if err != nil || written == 0 {
		test.Fatalf("snapshot wrote %d rows: %v", written, err)
	}

	// 50 - 1 - 1. A snapshot cut by time instead of by id would read 49.00.
	if balance := user.Balance(test, "USD"); balance != "48.00" {
		test.Errorf("user balance = %s, want 48.00 (a leg was skipped by the snapshot)", balance)
	}
	if balance := friend.Balance(test, "USD"); balance != "2.00" {
		test.Errorf("friend balance = %s, want 2.00 (a leg was skipped by the snapshot)", balance)
	}
	assertBooksBalance(test, environment)
}

// TestTreasuryFundingNeedsASecondAdmin checks the four-eyes rule end to end.
func TestTreasuryFundingNeedsASecondAdmin(test *testing.T) {
	environment := testenv.Start(test)
	finance := environment.SignInAdmin(test, "finance@tribe.africa")
	superAdmin := environment.SignInAdmin(test, "super@tribe.africa")

	requested := finance.Call(test, http.MethodPost, "/admin-api/treasury-fundings", map[string]string{
		"asset": "USD", "amount": "1000", "external_reference": "BANK-TEST-1", "note": "Test",
	}, testenv.NewKey())
	var funding struct {
		ID             string `json:"id"`
		Status         string `json:"status"`
		JournalEntryID string `json:"journal_entry_id"`
	}
	requested.Decode(test, &funding)
	if funding.Status != "pending" || funding.JournalEntryID != "" {
		test.Fatalf("a new request should be pending and unposted, got %+v", funding)
	}

	ownApproval := finance.Call(test, http.MethodPost, "/admin-api/treasury-fundings/"+funding.ID+"/approve", nil, testenv.NewKey())
	if ownApproval.Code != "FOUR_EYES_REQUIRED" {
		test.Fatalf("approving your own request gave %d %s, want FOUR_EYES_REQUIRED", ownApproval.Status, ownApproval.Code)
	}

	approved := superAdmin.Call(test, http.MethodPost, "/admin-api/treasury-fundings/"+funding.ID+"/approve", nil, testenv.NewKey())
	approved.Decode(test, &funding)
	if funding.Status != "approved" || funding.JournalEntryID == "" {
		test.Fatalf("approval did not post: %+v", funding)
	}

	twice := superAdmin.Call(test, http.MethodPost, "/admin-api/treasury-fundings/"+funding.ID+"/reject", map[string]string{"reason": "changed my mind"}, testenv.NewKey())
	if twice.Code != "ALREADY_DECIDED" {
		test.Errorf("deciding twice gave %d %s, want ALREADY_DECIDED", twice.Status, twice.Code)
	}
	assertBooksBalance(test, environment)
}

// TestUsdToGbpUsesTheNgnRail: the seeded global provider has the rail ON,
// so a USD -> GBP quote must go USD -> NGN -> GBP (two hops). That routing
// is ours to know: the quote we STORE records it, and the quote the app
// RECEIVES shows one step, USD -> GBP, with no sign of Naira.
func TestUsdToGbpUsesTheNgnRail(test *testing.T) {
	environment := testenv.Start(test)
	user := environment.RegisterUser(test, "railuser")
	type quoteAnswer struct {
		ID    string `json:"id"`
		Route []struct {
			FromAsset string `json:"from_asset"`
			ToAsset   string `json:"to_asset"`
		} `json:"route"`
	}
	// storedHops reads the quote row as the server saved it: was the rail used, and how many hops.
	storedHops := func(quoteID string) (bool, int) {
		var stored models.Quote
		if err := environment.App.Database.Where("id = ?", quoteID).Take(&stored).Error; err != nil {
			test.Fatalf("read the stored quote: %v", err)
		}
		var hops []map[string]any
		if err := json.Unmarshal(stored.Route, &hops); err != nil {
			test.Fatalf("read the stored route: %v", err)
		}
		return stored.UsesNgnRail, len(hops)
	}

	quote := user.Call(test, http.MethodPost, "/api/mobile/v1/quotes", map[string]string{
		"transaction_type": "swap", "from_asset": "USD", "to_asset": "GBP", "amount": "20",
	}, "")
	var railQuote quoteAnswer
	quote.Decode(test, &railQuote)
	if usedRail, hops := storedHops(railQuote.ID); !usedRail || hops != 2 {
		test.Fatalf("stored USD -> GBP quote: rail=%v hops=%d, want the rail with 2 hops", usedRail, hops)
	}
	if len(railQuote.Route) != 1 || railQuote.Route[0].FromAsset != "USD" || railQuote.Route[0].ToAsset != "GBP" {
		test.Errorf("the app's USD -> GBP route = %+v, want one step USD -> GBP", railQuote.Route)
	}
	if body := string(quote.Data); strings.Contains(body, "ngn_rail") || strings.Contains(body, "NGN") {
		test.Errorf("the app's quote reveals the Naira rail: %s", body)
	}

	// USDT -> USDC share the crypto provider with the rail OFF: one direct hop.
	direct := user.Call(test, http.MethodPost, "/api/mobile/v1/quotes", map[string]string{
		"transaction_type": "swap", "from_asset": "USDT", "to_asset": "USDC", "amount": "20",
	}, "")
	var directQuote quoteAnswer
	direct.Decode(test, &directQuote)
	if usedRail, hops := storedHops(directQuote.ID); usedRail || hops != 1 {
		test.Fatalf("stored USDT -> USDC quote: rail=%v hops=%d, want one direct hop", usedRail, hops)
	}
	if len(directQuote.Route) != 1 {
		test.Errorf("the app's USDT -> USDC route = %+v, want one step", directQuote.Route)
	}
}

// TestQuoteFromDollarAmount: a quote can be asked for in US dollars' worth
// (usd_amount), the way the app's swap screen lets a user type it. The server
// turns the dollars into the pay amount itself (market rate, rounded DOWN so
// the user never pays more than they typed) and then prices it exactly like a
// typed pay amount. Two amounts in one request are refused, because they
// could disagree.
func TestQuoteFromDollarAmount(test *testing.T) {
	environment := testenv.Start(test)
	user := environment.RegisterUser(test, "dollarquote")
	type quoteAmounts struct {
		FromAmount string `json:"from_amount"`
		Fee        string `json:"fee"`
		ToAmount   string `json:"to_amount"`
	}
	askForQuote := func(fromAsset string, toAsset string, amountField string, amount string) quoteAmounts {
		response := user.Call(test, http.MethodPost, "/api/mobile/v1/quotes", map[string]string{
			"transaction_type": "swap", "from_asset": fromAsset, "to_asset": toAsset, amountField: amount,
		}, "")
		if !response.Success {
			test.Fatalf("quote %s -> %s with %s=%s: %d %s", fromAsset, toAsset, amountField, amount, response.Status, response.Message)
		}
		var amounts quoteAmounts
		response.Decode(test, &amounts)
		return amounts
	}

	// Paying in dollars: 20 dollars' worth IS 20 USD, so both ways of asking agree.
	typedInDollars := askForQuote("USD", "GBP", "usd_amount", "20")
	typedAsPayAmount := askForQuote("USD", "GBP", "amount", "20")
	if typedInDollars != typedAsPayAmount {
		test.Errorf("USD -> GBP: usd_amount gave %+v, amount gave %+v, want the same", typedInDollars, typedAsPayAmount)
	}

	// Paying in pounds: the pay amount is 20 dollars / (dollars per pound), rounded down to pence.
	var poundRate models.ExchangeRateLatest
	if err := environment.App.Database.Where("asset_code = ?", "GBP").Take(&poundRate).Error; err != nil {
		test.Fatalf("read the GBP rate: %v", err)
	}
	dollarsPerPound, err := money.ParseRate(poundRate.UsdRate)
	if err != nil {
		test.Fatalf("parse the GBP rate: %v", err)
	}
	twentyDollars, _ := money.ParseDecimal("20", 2)
	wantPounds := money.FromRational(new(big.Rat).Quo(twentyDollars.ToRational(2), dollarsPerPound), 2, money.RoundDown).Decimal(2)
	poundsFromDollars := askForQuote("GBP", "USD", "usd_amount", "20")
	if poundsFromDollars.FromAmount != wantPounds {
		test.Errorf("GBP -> USD for 20 dollars' worth pays %s, want %s", poundsFromDollars.FromAmount, wantPounds)
	}
	if poundsTyped := askForQuote("GBP", "USD", "amount", wantPounds); poundsTyped != poundsFromDollars {
		test.Errorf("GBP -> USD: usd_amount gave %+v, the same pounds typed gave %+v, want the same", poundsFromDollars, poundsTyped)
	}

	twoAmounts := user.Call(test, http.MethodPost, "/api/mobile/v1/quotes", map[string]string{
		"transaction_type": "swap", "from_asset": "USD", "to_asset": "GBP", "amount": "20", "usd_amount": "20",
	}, "")
	if twoAmounts.Status != http.StatusUnprocessableEntity {
		test.Errorf("two amounts in one quote gave %d, want 422", twoAmounts.Status)
	}
}

// TestCustomerAdjustmentsUseFreeFundsAndASecondAdmin checks manual credits
// and debits end to end: a credit can never be bigger than the vault's FREE
// funds (vault minus what customers are owed), a debit never bigger than the
// wallet, only a super admin approves (never their own), and the books still balance.
func TestCustomerAdjustmentsUseFreeFundsAndASecondAdmin(test *testing.T) {
	environment := testenv.Start(test)
	customer := environment.RegisterUser(test, "adjustee")
	customer.TopUp(test, "USD", "40") // customer money in the vault: must NOT count as free
	finance := environment.SignInAdmin(test, "finance@tribe.africa")
	superAdmin := environment.SignInAdmin(test, "super@tribe.africa")

	var user models.User
	if err := environment.App.Database.Where("email = ?", customer.Email).Take(&user).Error; err != nil {
		test.Fatalf("find customer: %v", err)
	}
	freeUsd := func() string {
		var funds []struct {
			AssetCode string `json:"asset_code"`
			Free      string `json:"free"`
		}
		finance.Call(test, http.MethodGet, "/admin-api/free-funds", nil, "").Decode(test, &funds)
		for _, fund := range funds {
			if fund.AssetCode == "USD" {
				return fund.Free
			}
		}
		return "missing"
	}
	type adjustment struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	request := func(kind string, amount string) *testenv.Response {
		return finance.Call(test, http.MethodPost, "/admin-api/customers/"+user.ID+"/adjustments", map[string]string{
			"kind": kind, "asset": "USD", "amount": amount, "reason": "Integration test",
		}, testenv.NewKey())
	}
	approve := func(kind string, amount string) {
		var pending adjustment
		request(kind, amount).Decode(test, &pending)
		if pending.Status != "pending" {
			test.Fatalf("a new %s should be pending, got %+v", kind, pending)
		}
		// finance may request but is not a super admin, so may not approve
		if notSuper := finance.Call(test, http.MethodPost, "/admin-api/customer-adjustments/"+pending.ID+"/approve", nil, testenv.NewKey()); notSuper.Status != http.StatusForbidden {
			test.Fatalf("a non-super admin approving a %s gave %d %s, want 403", kind, notSuper.Status, notSuper.Code)
		}
		var approved adjustment
		superAdmin.Call(test, http.MethodPost, "/admin-api/customer-adjustments/"+pending.ID+"/approve", nil, testenv.NewKey()).Decode(test, &approved)
		if approved.Status != "approved" {
			test.Fatalf("%s approval did not post: %+v", kind, approved)
		}
	}

	startFree := freeUsd()
	funding := finance.Call(test, http.MethodPost, "/admin-api/treasury-fundings", map[string]string{
		"asset": "USD", "amount": "1000", "external_reference": "BANK-ADJ-1", "note": "Test",
	}, testenv.NewKey())
	var fundingRow adjustment
	funding.Decode(test, &fundingRow)
	superAdmin.Call(test, http.MethodPost, "/admin-api/treasury-fundings/"+fundingRow.ID+"/approve", nil, testenv.NewKey())
	if startFree != "0.00" || freeUsd() != "1000.00" {
		test.Fatalf("free USD went %s -> %s, want 0.00 -> 1000.00 (the customer's 40 is not free)", startFree, freeUsd())
	}

	if tooBig := request("credit", "1000.01"); tooBig.Code != "INSUFFICIENT_FREE_FUNDS" {
		test.Fatalf("crediting more than the free funds gave %d %s, want INSUFFICIENT_FREE_FUNDS", tooBig.Status, tooBig.Code)
	}
	approve("credit", "300")
	if balance := customer.Balance(test, "USD"); balance != "340.00" {
		test.Fatalf("balance after credit = %s, want 340.00", balance)
	}
	if free := freeUsd(); free != "700.00" {
		test.Fatalf("free USD after a 300 credit = %s, want 700.00", free)
	}

	if tooBig := request("debit", "340.01"); tooBig.Code != "INSUFFICIENT_FUNDS" {
		test.Fatalf("debiting more than the wallet gave %d %s, want INSUFFICIENT_FUNDS", tooBig.Status, tooBig.Code)
	}
	approve("debit", "100")
	if balance := customer.Balance(test, "USD"); balance != "240.00" {
		test.Fatalf("balance after debit = %s, want 240.00", balance)
	}
	if free := freeUsd(); free != "800.00" {
		test.Fatalf("free USD after a 100 debit = %s, want 800.00", free)
	}
	// even a super admin cannot approve their own request
	var own adjustment
	superAdmin.Call(test, http.MethodPost, "/admin-api/customers/"+user.ID+"/adjustments", map[string]string{
		"kind": "credit", "asset": "USD", "amount": "1", "reason": "Own request",
	}, testenv.NewKey()).Decode(test, &own)
	if ownApproval := superAdmin.Call(test, http.MethodPost, "/admin-api/customer-adjustments/"+own.ID+"/approve", nil, testenv.NewKey()); ownApproval.Code != "FOUR_EYES_REQUIRED" {
		test.Fatalf("approving your own adjustment gave %d %s, want FOUR_EYES_REQUIRED", ownApproval.Status, ownApproval.Code)
	}
	assertBooksBalance(test, environment)
}

// TestBatchedBalancesMatchOneByOne checks that the batched balance query and
// the single-statement free-funds sum give EXACTLY the same numbers as the
// one-account-at-a-time method, both before and after a daily snapshot.
func TestBatchedBalancesMatchOneByOne(test *testing.T) {
	environment := testenv.Start(test)
	alice := environment.RegisterUser(test, "batchalice")
	bob := environment.RegisterUser(test, "batchbob")
	alice.TopUp(test, "USD", "70")
	bob.TopUp(test, "NGN", "25000")
	send := func(from *testenv.Client, handle string, asset string, amount string) {
		response := from.Call(test, http.MethodPost, "/api/mobile/v1/sends", map[string]any{
			"recipient_kind": "tribe", "handle": handle, "asset": asset, "amount": amount, "pin": testenv.DemoPin,
		}, testenv.NewKey())
		if !response.Success {
			test.Fatalf("send failed: %s %s", response.Code, response.Message)
		}
	}
	ledgerService := environment.App.Ledger
	compare := func(stage string) {
		var accounts []models.LedgerAccount
		environment.App.Database.Order("id").Find(&accounts)
		batched, err := ledgerService.BalancesForAccounts(context.Background(), accounts)
		if err != nil {
			test.Fatalf("%s: batched balances: %v", stage, err)
		}
		for _, row := range batched {
			single, err := ledgerService.BalanceOf(context.Background(), row.Account.ID)
			if err != nil {
				test.Fatalf("%s: balance of %d: %v", stage, row.Account.ID, err)
			}
			if single.UnitsString() != row.Balance.UnitsString() {
				test.Errorf("%s: account %d (%s %s): batched %s, one by one %s", stage, row.Account.ID, row.Account.Code, row.Account.AssetCode, row.Balance.UnitsString(), single.UnitsString())
			}
		}
		// The free-funds page computes every currency at once; it must give
		// exactly what FreeFundsOf gives one asset at a time.
		var enabledAssets []models.Asset
		environment.App.Database.Where("enabled = ?", true).Find(&enabledAssets)
		enabledCodes := make([]string, 0, len(enabledAssets))
		for _, asset := range enabledAssets {
			enabledCodes = append(enabledCodes, asset.Code)
		}
		freeByAsset, err := ledgerService.FreeFundsForAssets(environment.App.Database, enabledCodes)
		if err != nil {
			test.Fatalf("%s: batched free funds: %v", stage, err)
		}
		if len(freeByAsset) != len(enabledCodes) {
			test.Fatalf("%s: batched free funds has %d assets, want %d", stage, len(freeByAsset), len(enabledCodes))
		}
		for _, assetCode := range enabledCodes {
			single, err := ledgerService.FreeFundsOf(environment.App.Database, assetCode)
			if err != nil {
				test.Fatalf("%s: free funds %s: %v", stage, assetCode, err)
			}
			batchedFree := freeByAsset[assetCode]
			if batchedFree.AssetCode != assetCode || batchedFree.Vault.UnitsString() != single.Vault.UnitsString() ||
				batchedFree.Owed.UnitsString() != single.Owed.UnitsString() || batchedFree.Free.UnitsString() != single.Free.UnitsString() {
				test.Errorf("%s: free funds %s: batched vault %s owed %s free %s, one asset vault %s owed %s free %s", stage, assetCode,
					batchedFree.Vault.UnitsString(), batchedFree.Owed.UnitsString(), batchedFree.Free.UnitsString(),
					single.Vault.UnitsString(), single.Owed.UnitsString(), single.Free.UnitsString())
			}
		}

		for _, assetCode := range []string{"USD", "NGN"} {
			free, err := ledgerService.FreeFundsOf(environment.App.Database, assetCode)
			if err != nil {
				test.Fatalf("%s: free funds %s: %v", stage, assetCode, err)
			}
			owed := money.Zero()
			for _, row := range batched {
				account := row.Account
				owedKind := (account.UserID != nil && (account.Code == "user_primary" || account.Code == "user_referral" || account.Code == "user_card")) ||
					(account.OwnerKey == "company" && (account.Code == "cashpoint_escrow" || account.Code == "payout_in_transit" || account.Code == "purchase_in_transit"))
				if account.AssetCode == assetCode && owedKind {
					owed = owed.Add(row.Balance)
				}
			}
			if free.Owed.UnitsString() != owed.UnitsString() {
				test.Errorf("%s: owed %s: single statement %s, account by account %s", stage, assetCode, free.Owed.UnitsString(), owed.UnitsString())
			}
		}
	}

	send(alice, "@batchbob", "USD", "12.34")
	compare("before any snapshot")

	today := time.Now().In(environment.App.Settings.App.TimeZone).Format(time.DateOnly)
	if _, err := ledgerService.WriteDailySnapshots(context.Background(), today); err != nil {
		test.Fatalf("snapshot: %v", err)
	}
	compare("right after the snapshot")

	send(bob, "@batchalice", "NGN", "1500")
	send(bob, "@batchalice", "USD", "2.34")
	compare("snapshot plus later legs")
}
