package integration

import (
	"context"
	"net/http"
	"testing"

	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/tests/testenv"
)

// TestEveryMoneyFlowKeepsTheProvidersMirrored runs every kind of money
// movement a customer can make, one after the other, and after EACH ONE
// checks the rule the ledger rests on (assertBooksBalance): in every
// currency, what the providers hold minus what we owe customers is exactly
// Tribe Funds, and the card issuer holds exactly the money on the cards.
// It also checks the provider vaults move where the money really goes.
func TestEveryMoneyFlowKeepsTheProvidersMirrored(test *testing.T) {
	environment := testenv.Start(test)
	ada := environment.RegisterUser(test, "mirrorada")
	bola := environment.RegisterUser(test, "mirrorbola")
	ok := func(step string, response *testenv.Response) *testenv.Response {
		test.Helper()
		if !response.Success {
			test.Fatalf("%s failed: %d %s %s", step, response.Status, response.Code, response.Message)
		}
		assertBooksBalance(test, environment)
		return response
	}
	post := func(client *testenv.Client, path string, body any) *testenv.Response {
		return client.Call(test, http.MethodPost, "/api/mobile/v1"+path, body, testenv.NewKey())
	}
	quote := func(client *testenv.Client, transactionType string, fromAsset string, toAsset string, amount string) string {
		var answer struct {
			ID string `json:"id"`
		}
		client.Call(test, http.MethodPost, "/api/mobile/v1/quotes", map[string]string{
			"transaction_type": transactionType, "from_asset": fromAsset, "to_asset": toAsset, "amount": amount,
		}, "").Decode(test, &answer)
		return answer.ID
	}

	// Deposits land in each currency's home provider vault.
	globalUsdBefore := providerVaultBalance(test, environment, "global-simulator", "USD")
	ada.TopUp(test, "USD", "500")
	ada.TopUp(test, "NGN", "200000")
	ada.TopUp(test, "USDT", "300")
	assertBooksBalance(test, environment)
	if grew := providerVaultBalance(test, environment, "global-simulator", "USD").Subtract(globalUsdBefore); grew.Decimal(2) != "500.00" {
		test.Fatalf("Global FX · USD grew by %s after a $500 deposit, want 500.00", grew.Decimal(2))
	}

	// Swaps: direct (both at the crypto provider) and on the NGN rail.
	cryptoUsdcBefore := providerVaultBalance(test, environment, "crypto-simulator", "USDC")
	ok("direct swap USDT -> USDC", post(ada, "/swaps", map[string]string{"quote_id": quote(ada, "swap", "USDT", "USDC", "100"), "pin": testenv.DemoPin}))
	if !providerVaultBalance(test, environment, "crypto-simulator", "USDC").Subtract(cryptoUsdcBefore).IsPositive() {
		test.Errorf("Crypto Desk · USDC did not grow after it bought USDC")
	}
	ok("NGN rail swap USD -> GBP", post(ada, "/swaps", map[string]string{"quote_id": quote(ada, "swap", "USD", "GBP", "40"), "pin": testenv.DemoPin}))

	// Tribe sends: same currency and with a quote.
	ok("Tribe send USD", post(ada, "/sends", map[string]any{"recipient_kind": "tribe", "handle": "@mirrorbola", "asset": "USD", "amount": "20", "pin": testenv.DemoPin}))
	ok("Tribe send NGN -> USD", post(ada, "/sends", map[string]any{"recipient_kind": "tribe", "handle": "@mirrorbola", "quote_id": quote(ada, "send", "NGN", "USD", "15000"), "pin": testenv.DemoPin}))

	// Cards: every dollar on a card sits at the card issuer.
	issuerBefore := providerVaultBalance(test, environment, "cards-simulator", "USD")
	var created struct {
		Card struct {
			ID string `json:"id"`
		} `json:"card"`
	}
	ok("create card", post(ada, "/cards", map[string]any{
		"label": "Mirror", "network": "visa", "design_finish": "midnight", "first_top_up": "30", "pay_with": "USD", "pin": testenv.DemoPin,
	})).Decode(test, &created)
	ok("fund card from NGN", post(ada, "/cards/"+created.Card.ID+"/fund", map[string]string{"amount_usd": "10", "asset": "NGN", "pin": testenv.DemoPin}))
	if onCard := providerVaultBalance(test, environment, "cards-simulator", "USD").Subtract(issuerBefore); onCard.Decimal(2) != "40.00" {
		test.Errorf("the card issuer gained %s, want the 40.00 now on the card", onCard.Decimal(2))
	}
	ok("withdraw card to USD", post(ada, "/cards/"+created.Card.ID+"/withdraw", map[string]string{"amount_usd": "5", "asset": "USD", "pin": testenv.DemoPin}))
	ok("delete card", ada.Call(test, http.MethodDelete, "/api/mobile/v1/cards/"+created.Card.ID, map[string]string{"pin": testenv.DemoPin}, testenv.NewKey()))
	if left := providerVaultBalance(test, environment, "cards-simulator", "USD"); left.Compare(issuerBefore) != 0 {
		test.Errorf("the card issuer holds %s after the card was closed, want %s", left.Decimal(2), issuerBefore.Decimal(2))
	}

	// Bills (confirmed, then refused) and gift cards (bought, then sold).
	ok("bill paid with USD", post(ada, "/bills", map[string]any{"biller_id": "phed", "target": "45678901", "amount_ngn": "3000", "pay_with": "USD", "pin": testenv.DemoPin}))
	if refused := post(ada, "/bills", map[string]any{"biller_id": "phed", "target": "45670000", "amount_ngn": "3000", "pay_with": "NGN", "pin": testenv.DemoPin}); refused.Code != "PROVIDER_REFUSED" {
		test.Fatalf("refused bill gave %s, want PROVIDER_REFUSED", refused.Code)
	}
	assertBooksBalance(test, environment)
	ok("gift card bought with NGN", post(ada, "/gift-cards/buy", map[string]any{"brand_id": "steam", "face_usd": "10", "asset": "NGN", "pin": testenv.DemoPin}))
	ok("gift card sold for NGN", post(ada, "/gift-cards/sell", map[string]any{"brand_id": "amazon", "face_usd": "20", "asset": "NGN", "code": "AMZN-123456", "pin": testenv.DemoPin}))
	ok("gift card sold for USDT", post(ada, "/gift-cards/sell", map[string]any{"brand_id": "amazon", "face_usd": "20", "asset": "USDT", "code": "AMZN-654321", "pin": testenv.DemoPin}))

	// Payouts: one delivered, one refused (the fee is kept).
	payout := func(handle string) string {
		var sent struct {
			ID string `json:"id"`
		}
		ok("payout to "+handle, post(ada, "/sends", map[string]any{
			"recipient_kind": "bank", "handle": handle, "asset": "NGN", "amount": "5000", "channel_id": "ngn-bank", "country": "NG", "pin": testenv.DemoPin,
		})).Decode(test, &sent)
		if err := environment.App.Transfers.ProcessPayout(context.Background(), sent.ID); err != nil {
			test.Fatalf("process payout %s: %v", handle, err)
		}
		assertBooksBalance(test, environment)
		return sent.ID
	}
	statusOf := func(transactionID string) string {
		var transaction models.Transaction
		environment.App.Database.Where("id = ?", transactionID).Take(&transaction)
		return transaction.Status
	}
	if delivered := payout("0123456789"); statusOf(delivered) != models.TransactionStatusCompleted {
		test.Errorf("delivered payout is %s, want completed", statusOf(delivered))
	}
	if refused := payout("fail-0123456789"); statusOf(refused) != models.TransactionStatusFailed {
		test.Errorf("refused payout is %s, want failed", statusOf(refused))
	}

	// Cashpoints: the Naira never leaves the provider, only who it is owed to.
	globalNgnBefore := providerVaultBalance(test, environment, "global-simulator", "NGN")
	var cashpoint struct {
		ID   string `json:"id"`
		Code string `json:"code"`
	}
	ok("create cashpoint", post(ada, "/cashpoints", map[string]string{"amount_ngn": "4000", "pin": testenv.DemoPin})).Decode(test, &cashpoint)
	ok("redeem cashpoint", post(bola, "/cashpoints/redeem", map[string]string{"code": cashpoint.Code}))
	var second struct {
		ID string `json:"id"`
	}
	ok("create second cashpoint", post(ada, "/cashpoints", map[string]string{"amount_ngn": "1000", "pin": testenv.DemoPin})).Decode(test, &second)
	ok("cancel cashpoint", post(ada, "/cashpoints/"+second.ID+"/cancel", nil))
	if after := providerVaultBalance(test, environment, "global-simulator", "NGN"); after.Compare(globalNgnBefore) != 0 {
		test.Errorf("Global FX · NGN moved from %s to %s for cashpoints, want no change", globalNgnBefore.Decimal(2), after.Decimal(2))
	}
	if balance := bola.Balance(test, "NGN"); balance != "4000.00" {
		test.Errorf("Bola's NGN after redeeming = %s, want 4000.00", balance)
	}
}
