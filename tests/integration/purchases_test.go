package integration

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/tests/testenv"
)

// payBill pays a ₦5,000 electricity bill from the user's NGN wallet.
func payBill(test *testing.T, user *testenv.Client, target string) *testenv.Response {
	return user.Call(test, http.MethodPost, "/api/mobile/v1/bills", map[string]any{
		"biller_id": "phed", "target": target, "amount_ngn": "5000", "pay_with": "NGN", "pin": testenv.DemoPin,
	}, testenv.NewKey())
}

// purchaseEntries counts the journal entries of a purchase whose reference
// ends with suffix (":POST", ":SETTLE", ":POST:REVERSAL").
func purchaseEntries(test *testing.T, environment *testenv.Environment, transactionID string, suffix string) int64 {
	var count int64
	environment.App.Database.Model(&models.JournalEntry{}).
		Where("business_transaction_id = ? AND reference LIKE ?", transactionID, "%"+suffix).Count(&count)
	return count
}

// TestBillIsPaidOutsideTheTransactionAndSettled pays a bill with a provider
// that answers straight away: the customer still gets the token in the same
// response, the money went POST -> SETTLE through Purchases In Transit, and
// nothing is left in transit.
func TestBillIsPaidOutsideTheTransactionAndSettled(test *testing.T) {
	environment := testenv.Start(test)
	user := environment.RegisterUser(test, "billpayer")
	user.TopUp(test, "NGN", "20000")

	response := payBill(test, user, "45678901")
	if !response.Success {
		test.Fatalf("bill failed: %s %s", response.Code, response.Message)
	}
	var transaction struct {
		ID     string         `json:"id"`
		Status string         `json:"status"`
		Meta   map[string]any `json:"meta"`
	}
	response.Decode(test, &transaction)
	if transaction.Status != "completed" || transaction.Meta["token"] == nil {
		test.Fatalf("bill = %+v, want completed with a token", transaction)
	}
	if purchaseEntries(test, environment, transaction.ID, ":POST") != 1 || purchaseEntries(test, environment, transaction.ID, ":SETTLE") != 1 {
		test.Errorf("want exactly one POST and one SETTLE entry")
	}
	var tokens int64
	environment.App.Database.Model(&models.VaultToken{}).Where("meter = ?", "45678901").Count(&tokens)
	if tokens != 1 {
		test.Errorf("%d vault tokens saved, want 1", tokens)
	}
	assertNothingInTransit(test, environment, "NGN")
	assertBooksBalance(test, environment)
}

// TestRefusedBillIsRefundedExactly makes the biller refuse: the request
// answers PROVIDER_REFUSED, the original posting is reversed, and the
// wallet is back to the exact amount it had (fee included).
func TestRefusedBillIsRefundedExactly(test *testing.T) {
	environment := testenv.Start(test)
	user := environment.RegisterUser(test, "billrefused")
	user.TopUp(test, "NGN", "20000")

	response := payBill(test, user, "45670000") // the simulator refuses targets ending in 0000
	if response.Success || response.Code != "PROVIDER_REFUSED" {
		test.Fatalf("got %v %s, want PROVIDER_REFUSED", response.Success, response.Code)
	}
	if balance := user.Balance(test, "NGN"); balance != "20000.00" {
		test.Errorf("balance = %s, want 20000.00 (full refund, fee included)", balance)
	}
	var transaction models.Transaction
	environment.App.Database.Where("user_id = (SELECT id FROM users WHERE email = ?) AND type = 'bill'", user.Email).Take(&transaction)
	if transaction.Status != models.TransactionStatusFailed {
		test.Errorf("transaction status = %s, want failed", transaction.Status)
	}
	if purchaseEntries(test, environment, transaction.ID, ":POST:REVERSAL") != 1 || purchaseEntries(test, environment, transaction.ID, ":SETTLE") != 0 {
		test.Errorf("want one reversal and no settlement")
	}
	assertNothingInTransit(test, environment, "NGN")
	assertBooksBalance(test, environment)
}

// TestSlowBillStaysPendingThenSettlesExactlyOnce makes the biller time out
// on the first try. The customer is charged once and the bill stays
// pending (a timeout is never treated as a refusal). Then ten workers
// finish it AT THE SAME TIME: exactly one settlement may be posted.
func TestSlowBillStaysPendingThenSettlesExactlyOnce(test *testing.T) {
	environment := testenv.Start(test)
	user := environment.RegisterUser(test, "billslow")
	user.TopUp(test, "NGN", "20000")

	response := payBill(test, user, "45675555") // the simulator times out once for targets ending in 5555
	if !response.Success {
		test.Fatalf("bill failed: %s %s", response.Code, response.Message)
	}
	var transaction struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	response.Decode(test, &transaction)
	if transaction.Status != "pending" {
		test.Fatalf("status = %s, want pending after a timeout", transaction.Status)
	}
	afterCharge := user.Balance(test, "NGN")

	var waitGroup sync.WaitGroup
	for worker := 0; worker < 10; worker++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			if err := environment.App.Products.ProcessPurchase(context.Background(), transaction.ID); err != nil {
				test.Errorf("process purchase: %v", err)
			}
		}()
	}
	waitGroup.Wait()

	var finished models.Transaction
	environment.App.Database.Where("id = ?", transaction.ID).Take(&finished)
	if finished.Status != models.TransactionStatusCompleted {
		test.Errorf("status = %s, want completed", finished.Status)
	}
	if settled := purchaseEntries(test, environment, transaction.ID, ":SETTLE"); settled != 1 {
		test.Errorf("%d settlement entries, want exactly 1", settled)
	}
	if balance := user.Balance(test, "NGN"); balance != afterCharge {
		test.Errorf("balance moved from %s to %s while settling; the customer must be charged once", afterCharge, balance)
	}
	assertNothingInTransit(test, environment, "NGN")
	assertBooksBalance(test, environment)
}

// TestGiftCardCodeIsReturnedAndCanBeRevealedAgain buys a gift card: the code
// comes back in the response, and the reveal endpoint (PIN required) shows
// the same code later.
func TestGiftCardCodeIsReturnedAndCanBeRevealedAgain(test *testing.T) {
	environment := testenv.Start(test)
	user := environment.RegisterUser(test, "giftbuyer")
	user.TopUp(test, "USD", "100")

	response := user.Call(test, http.MethodPost, "/api/mobile/v1/gift-cards/buy", map[string]any{
		"brand_id": "steam", "face_usd": "25", "asset": "USD", "pin": testenv.DemoPin,
	}, testenv.NewKey())
	if !response.Success {
		test.Fatalf("gift card failed: %s %s", response.Code, response.Message)
	}
	var bought struct {
		Transaction struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"transaction"`
		Code string `json:"code"`
	}
	response.Decode(test, &bought)
	if bought.Transaction.Status != "completed" || !strings.HasPrefix(bought.Code, "STE-") {
		test.Fatalf("bought = %+v, want completed with a STE- code", bought)
	}

	wrongPin := user.Call(test, http.MethodPost, "/api/mobile/v1/gift-cards/"+bought.Transaction.ID+"/reveal", map[string]string{"pin": "9871"}, "")
	if wrongPin.Success {
		test.Errorf("reveal with a wrong PIN should fail")
	}
	reveal := user.Call(test, http.MethodPost, "/api/mobile/v1/gift-cards/"+bought.Transaction.ID+"/reveal", map[string]string{"pin": testenv.DemoPin}, "")
	var revealed struct {
		Code string `json:"code"`
	}
	reveal.Decode(test, &revealed)
	if revealed.Code != bought.Code {
		test.Errorf("revealed %q, bought %q", revealed.Code, bought.Code)
	}
	assertNothingInTransit(test, environment, "USD")
	assertBooksBalance(test, environment)
}

// TestSweeperRequeuesStuckPurchases leaves a bill pending (the provider
// timed out and its job was "lost"), ages it, and checks the sweeper queues
// it again.
func TestSweeperRequeuesStuckPurchases(test *testing.T) {
	environment := testenv.Start(test)
	user := environment.RegisterUser(test, "billstuck")
	user.TopUp(test, "NGN", "20000")
	response := payBill(test, user, "45685555")
	var transaction struct {
		ID string `json:"id"`
	}
	response.Decode(test, &transaction)
	environment.App.Database.Exec("UPDATE transactions SET created_at = ? WHERE id = ?", time.Now().UTC().Add(-10*time.Minute), transaction.ID)

	message, err := environment.App.Products.RedrivePendingPurchases(context.Background(), "test-"+testenv.NewKey())
	if err != nil {
		test.Fatalf("sweeper: %v", err)
	}
	if !strings.Contains(message, "re-queued 1 ") {
		test.Errorf("sweeper said %q, want 1 re-queued", message)
	}
}

// assertNothingInTransit checks Purchases In Transit is back to zero: every
// purchase was either settled or refunded.
func assertNothingInTransit(test *testing.T, environment *testenv.Environment, assetCode string) {
	test.Helper()
	account, err := environment.App.Ledger.CompanyAccount(environment.App.Database, "purchase_in_transit", assetCode)
	if err != nil {
		test.Fatalf("purchase in transit %s: %v", assetCode, err)
	}
	balance, err := environment.App.Ledger.BalanceOf(context.Background(), account.ID)
	if err != nil {
		test.Fatalf("balance: %v", err)
	}
	if !balance.IsZero() {
		test.Errorf("Purchases In Transit %s holds %s, want 0", assetCode, balance.UnitsString())
	}
}
