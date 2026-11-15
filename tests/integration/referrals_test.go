package integration

import (
	"context"
	"net/http"
	"testing"

	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/app/services/ledger"
	"github.com/tribe-africa/tribe-backend/pkg/money"
	"github.com/tribe-africa/tribe-backend/tests/testenv"
)

// swapFor quotes and swaps for a customer, and returns the transaction id.
func swapFor(test *testing.T, customer *testenv.Client, fromAsset string, toAsset string, amount string) string {
	test.Helper()
	var quote struct {
		ID string `json:"id"`
	}
	customer.Call(test, http.MethodPost, "/api/mobile/v1/quotes", map[string]string{
		"transaction_type": "swap", "from_asset": fromAsset, "to_asset": toAsset, "amount": amount,
	}, "").Decode(test, &quote)
	response := customer.Call(test, http.MethodPost, "/api/mobile/v1/swaps", map[string]string{"quote_id": quote.ID, "pin": testenv.DemoPin}, testenv.NewKey())
	if !response.Success {
		test.Fatalf("swap %s -> %s failed: %s %s", fromAsset, toAsset, response.Code, response.Message)
	}
	var transaction struct {
		ID string `json:"id"`
	}
	response.Decode(test, &transaction)
	return transaction.ID
}

// earningOf returns the referral earning of one transaction (nil if none).
func earningOf(test *testing.T, environment *testenv.Environment, transactionID string) *models.ReferralEarning {
	test.Helper()
	var earnings []models.ReferralEarning
	environment.App.Database.Where("transaction_id = ?", transactionID).Find(&earnings)
	if len(earnings) == 0 {
		return nil
	}
	return &earnings[0]
}

// tribeFundsNaira is Tribe's own Naira right now (rewards are paid from it).
func tribeFundsNaira(test *testing.T, environment *testenv.Environment) money.Amount {
	test.Helper()
	funds, err := environment.App.Ledger.TribeFunds(environment.App.Database, ledger.ReferralAsset)
	if err != nil {
		test.Fatalf("Tribe Funds NGN: %v", err)
	}
	balance, err := environment.App.Ledger.BalanceOf(context.Background(), funds.ID)
	if err != nil {
		test.Fatalf("Tribe Funds NGN balance: %v", err)
	}
	return balance
}

// referralBalance is the inviter's referral balance as the app shows it.
func referralBalance(test *testing.T, customer *testenv.Client) string {
	test.Helper()
	var summary struct {
		Balance      string `json:"balance"`
		SharePercent string `json:"share_percent"`
	}
	customer.Call(test, http.MethodGet, "/api/mobile/v1/referrals", nil, "").Decode(test, &summary)
	return summary.Balance
}

// TestInvitedCustomersShareEarningsWithTheirInviter follows the referral
// rule end to end:
//   - an invited customer's swap pays the inviter the referral share (2%) of
//     what Tribe earned on it, straight away, in Naira, out of Tribe Funds;
//   - a bill a provider must confirm only records the share, and pays it on
//     confirmation; a refused bill cancels it;
//   - a customer nobody invited earns nobody anything;
//   - a share of zero pays nothing, and the share can never pass 100%.
func TestInvitedCustomersShareEarningsWithTheirInviter(test *testing.T) {
	environment := testenv.Start(test)
	inviter := environment.RegisterUser(test, "inviter")
	friend := environment.RegisterInvitedUser(test, "friend", inviter)
	stranger := environment.RegisterUser(test, "stranger")
	friend.TopUp(test, "NGN", "300000")
	friend.TopUp(test, "USD", "100")
	stranger.TopUp(test, "NGN", "100000")
	finance := environment.SignInAdmin(test, "finance@tribe.africa")

	// 1. A swap: ₦200,000 to USD. The 0.5% fee is ₦1,000.00, so the Naira
	// part of the share is 2% of it: ₦20.00. The margin (about 0.5%, some
	// 65 cents) is earned in USD; its share (1 cent) is valued in Naira on
	// top. Shares round DOWN in each currency: on a small swap the USD part
	// would be under a cent and simply not paid.
	nairaBefore := tribeFundsNaira(test, environment)
	swapID := swapFor(test, friend, "NGN", "USD", "200000")
	swapEarning := earningOf(test, environment, swapID)
	if swapEarning == nil || swapEarning.Status != models.ReferralPaid {
		test.Fatalf("swap earning = %+v, want one paid earning", swapEarning)
	}
	swapShare, _ := money.ParseUnits(swapEarning.AmountNgn)
	if swapShare.Compare(money.FromUnits(2000)) <= 0 {
		test.Errorf("swap share = %s kobo, want more than the 2000 kobo fee share (the USD margin share is added)", swapEarning.AmountNgn)
	}
	var rewardLeg models.JournalLeg
	err := environment.App.Database.Where("journal_entry_id = ? AND earning_source = ?", *swapEarning.AccrualEntryID, ledger.SourceReferralReward).Take(&rewardLeg).Error
	if err != nil || rewardLeg.Amount != swapEarning.AmountNgn || rewardLeg.Side != "debit" {
		test.Fatalf("reward leg = %+v (%v), want a Tribe Funds NGN debit of %s in the swap's own entry", rewardLeg, err, swapEarning.AmountNgn)
	}
	// Tribe Funds NGN: + the ₦1,000.00 fee - the reward.
	if gained := tribeFundsNaira(test, environment).Subtract(nairaBefore); gained.Compare(money.FromUnits(100_000).Subtract(swapShare)) != 0 {
		test.Errorf("Tribe Funds NGN moved by %s kobo, want 100000 - %s", gained.UnitsString(), swapShare.UnitsString())
	}
	if balance := referralBalance(test, inviter); balance != swapShare.Decimal(2) {
		test.Errorf("inviter's referral balance = %s, want %s", balance, swapShare.Decimal(2))
	}

	// 2. A bill paid with USD, confirmed by the biller: recorded, then paid
	// in its own entry once the biller confirmed.
	confirmed := friend.Call(test, http.MethodPost, "/api/mobile/v1/bills", map[string]any{
		"biller_id": "phed", "target": "45678901", "amount_ngn": "5000", "pay_with": "USD", "pin": testenv.DemoPin,
	}, testenv.NewKey())
	var bill struct {
		ID string `json:"id"`
	}
	confirmed.Decode(test, &bill)
	billEarning := earningOf(test, environment, bill.ID)
	if billEarning == nil || billEarning.Status != models.ReferralPaid || billEarning.PayoutEntryID == nil || *billEarning.PayoutEntryID == *billEarning.AccrualEntryID {
		test.Fatalf("confirmed bill earning = %+v, want paid in a later entry than it accrued in", billEarning)
	}
	billShare, _ := money.ParseUnits(billEarning.AmountNgn)
	if balance := referralBalance(test, inviter); balance != swapShare.Add(billShare).Decimal(2) {
		test.Errorf("inviter's referral balance = %s, want %s (swap + bill)", balance, swapShare.Add(billShare).Decimal(2))
	}

	// 3. A bill the biller refuses: the whole entry is reversed, share included.
	refused := friend.Call(test, http.MethodPost, "/api/mobile/v1/bills", map[string]any{
		"biller_id": "phed", "target": "45670000", "amount_ngn": "5000", "pay_with": "USD", "pin": testenv.DemoPin,
	}, testenv.NewKey())
	if refused.Code != "PROVIDER_REFUSED" {
		test.Fatalf("refused bill gave %s, want PROVIDER_REFUSED", refused.Code)
	}
	var refusedBill models.Transaction
	environment.App.Database.Where("user_id = (SELECT id FROM users WHERE email = ?) AND status = ?", friend.Email, models.TransactionStatusFailed).Take(&refusedBill)
	if refusedEarning := earningOf(test, environment, refusedBill.ID); refusedEarning == nil || refusedEarning.Status != models.ReferralCancelled {
		test.Errorf("refused bill earning = %+v, want cancelled", refusedEarning)
	}
	if balance := referralBalance(test, inviter); balance != swapShare.Add(billShare).Decimal(2) {
		test.Errorf("a refused bill changed the inviter's balance to %s", balance)
	}

	// 4. Nobody invited the stranger: their swap earns nobody anything.
	if strangerEarning := earningOf(test, environment, swapFor(test, stranger, "NGN", "USD", "10000")); strangerEarning != nil {
		test.Errorf("a customer nobody invited produced a referral earning: %+v", strangerEarning)
	}

	// 5. The share is an admin setting: at 0% nothing is paid, and it can never pass 100%.
	var settings struct {
		SharePercent string `json:"share_percent"`
		Version      int    `json:"version"`
	}
	finance.Call(test, http.MethodGet, "/admin-api/referral-settings", nil, "").Decode(test, &settings)
	if tooMuch := finance.Call(test, http.MethodPut, "/admin-api/referral-settings", map[string]any{"share_percent": "100.5", "version": settings.Version}, testenv.NewKey()); tooMuch.Status != http.StatusUnprocessableEntity {
		test.Errorf("a 100.5%% share gave %d, want 422", tooMuch.Status)
	}
	if saved := finance.Call(test, http.MethodPut, "/admin-api/referral-settings", map[string]any{"share_percent": "0", "version": settings.Version}, testenv.NewKey()); !saved.Success {
		test.Fatalf("setting the share to 0: %s %s", saved.Code, saved.Message)
	}
	if zeroEarning := earningOf(test, environment, swapFor(test, friend, "NGN", "USD", "10000")); zeroEarning != nil {
		test.Errorf("a 0%% share still produced an earning: %+v", zeroEarning)
	}

	var summary struct {
		Earnings []map[string]any `json:"earnings"`
		Invited  int64            `json:"invited"`
	}
	inviter.Call(test, http.MethodGet, "/api/mobile/v1/referrals", nil, "").Decode(test, &summary)
	if len(summary.Earnings) != 2 || summary.Invited != 1 {
		test.Errorf("referral screen shows %d earnings and %d invited, want 2 and 1", len(summary.Earnings), summary.Invited)
	}
	assertBooksBalance(test, environment)
}
