// Package integration runs the API against a real MySQL + Redis and checks
// the promises that matter most for money: no double spending, no double
// posting, no deadlocks, one active phone per user, and books that balance.
//
//	go test -p 1 ./tests/...      (or: make test)
package integration

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/tests/testenv"
)

// TestConcurrentSendsNeverOverspend fires 50 sends of 3 USD at the same time
// from a wallet holding 100 USD. Exactly floor(100 / 3) = 33 may succeed and
// the wallet must end at 1.00, never below zero.
func TestConcurrentSendsNeverOverspend(test *testing.T) {
	environment := testenv.Start(test)
	sender := environment.RegisterUser(test, "sender50")
	environment.RegisterUser(test, "receiver50")
	sender.TopUp(test, "USD", "100")

	var succeeded, refused atomic.Int64
	var waitGroup sync.WaitGroup
	for attempt := 0; attempt < 50; attempt++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			response := sender.Call(test, http.MethodPost, "/api/mobile/v1/sends", map[string]any{
				"recipient_kind": "tribe", "handle": "@receiver50", "asset": "USD", "amount": "3", "pin": testenv.DemoPin,
			}, testenv.NewKey())
			switch {
			case response.Success:
				succeeded.Add(1)
			case response.Code == "INSUFFICIENT_FUNDS":
				refused.Add(1)
			default:
				test.Errorf("unexpected answer: %d %s %s", response.Status, response.Code, response.Message)
			}
		}()
	}
	waitGroup.Wait()

	if succeeded.Load() != 33 {
		test.Errorf("%d sends succeeded, want exactly 33 (refused %d)", succeeded.Load(), refused.Load())
	}
	if balance := sender.Balance(test, "USD"); balance != "1.00" {
		test.Errorf("sender balance = %s, want 1.00", balance)
	}
	assertBooksBalance(test, environment)
}

// TestOppositeTransfersDoNotDeadlock sends A->B and B->A at the same time,
// many times. Accounts are always locked in ascending id order, so neither
// side can wait for the other forever; every send must finish.
func TestOppositeTransfersDoNotDeadlock(test *testing.T) {
	environment := testenv.Start(test)
	alice := environment.RegisterUser(test, "alicedl")
	bob := environment.RegisterUser(test, "bobdl")
	alice.TopUp(test, "USD", "100")
	bob.TopUp(test, "USD", "100")

	var waitGroup sync.WaitGroup
	send := func(from *testenv.Client, toTag string) {
		defer waitGroup.Done()
		response := from.Call(test, http.MethodPost, "/api/mobile/v1/sends", map[string]any{
			"recipient_kind": "tribe", "handle": "@" + toTag, "asset": "USD", "amount": "1", "pin": testenv.DemoPin,
		}, testenv.NewKey())
		if !response.Success {
			test.Errorf("send to %s failed: %d %s %s", toTag, response.Status, response.Code, response.Message)
		}
	}
	for round := 0; round < 20; round++ {
		waitGroup.Add(2)
		go send(alice, "bobdl")
		go send(bob, "alicedl")
	}
	waitGroup.Wait()

	// 20 each way: both end where they started.
	if balance := alice.Balance(test, "USD"); balance != "100.00" {
		test.Errorf("alice = %s, want 100.00", balance)
	}
	if balance := bob.Balance(test, "USD"); balance != "100.00" {
		test.Errorf("bob = %s, want 100.00", balance)
	}
	assertBooksBalance(test, environment)
}

// TestSameIdempotencyKeyPostsOnce sends the SAME swap request 20 times at
// once with ONE Idempotency-Key. Only one journal entry may be written, and
// every answer is either the same success or "still in progress".
func TestSameIdempotencyKeyPostsOnce(test *testing.T) {
	environment := testenv.Start(test)
	user := environment.RegisterUser(test, "idemuser")
	user.TopUp(test, "USD", "100")

	quote := user.Call(test, http.MethodPost, "/api/mobile/v1/quotes", map[string]string{
		"transaction_type": "swap", "from_asset": "USD", "to_asset": "GBP", "amount": "20",
	}, "")
	var quoteData struct {
		ID string `json:"id"`
	}
	quote.Decode(test, &quoteData)

	key := testenv.NewKey()
	body := map[string]string{"quote_id": quoteData.ID, "pin": testenv.DemoPin}
	transactionIDs := sync.Map{}
	var waitGroup sync.WaitGroup
	for attempt := 0; attempt < 20; attempt++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			response := user.Call(test, http.MethodPost, "/api/mobile/v1/swaps", body, key)
			if response.Success {
				var swap struct {
					ID string `json:"id"`
				}
				response.Decode(test, &swap)
				transactionIDs.Store(swap.ID, true)
				return
			}
			if response.Code != "REQUEST_IN_PROGRESS" {
				test.Errorf("unexpected answer: %d %s %s", response.Status, response.Code, response.Message)
			}
		}()
	}
	waitGroup.Wait()

	// A retry after everything finished replays the saved answer.
	replay := user.Call(test, http.MethodPost, "/api/mobile/v1/swaps", body, key)
	if replay.Header.Get("Idempotent-Replay") != "true" {
		test.Errorf("a late retry was not replayed: %d %s", replay.Status, replay.Message)
	}

	distinct := 0
	transactionIDs.Range(func(key, value any) bool { distinct++; return true })
	if distinct != 1 {
		test.Errorf("%d different swap transactions were returned, want 1", distinct)
	}
	var swapEntries int64
	environment.App.Database.Model(&models.JournalEntry{}).Where("transaction_type = ?", "swap").Count(&swapEntries)
	if swapEntries != 1 {
		test.Errorf("%d swap journal entries were posted, want 1", swapEntries)
	}

	// The same key with a DIFFERENT body is a bug in the app and is refused.
	reused := user.Call(test, http.MethodPost, "/api/mobile/v1/swaps", map[string]string{"quote_id": "something-else", "pin": testenv.DemoPin}, key)
	if reused.Code != "IDEMPOTENCY_KEY_REUSED" {
		test.Errorf("reusing a key with another body gave %d %s, want IDEMPOTENCY_KEY_REUSED", reused.Status, reused.Code)
	}
}

// TestQuoteCanOnlyBeUsedOnce uses one quote for two DIFFERENT swap requests.
// The second must be refused: a locked price is good for one conversion.
func TestQuoteCanOnlyBeUsedOnce(test *testing.T) {
	environment := testenv.Start(test)
	user := environment.RegisterUser(test, "quoteuser")
	user.TopUp(test, "USD", "100")
	quote := user.Call(test, http.MethodPost, "/api/mobile/v1/quotes", map[string]string{
		"transaction_type": "swap", "from_asset": "USD", "to_asset": "GBP", "amount": "10",
	}, "")
	var quoteData struct {
		ID string `json:"id"`
	}
	quote.Decode(test, &quoteData)

	first := user.Call(test, http.MethodPost, "/api/mobile/v1/swaps", map[string]string{"quote_id": quoteData.ID, "pin": testenv.DemoPin}, testenv.NewKey())
	if !first.Success {
		test.Fatalf("first swap failed: %s %s", first.Code, first.Message)
	}
	second := user.Call(test, http.MethodPost, "/api/mobile/v1/swaps", map[string]string{"quote_id": quoteData.ID, "pin": testenv.DemoPin}, testenv.NewKey())
	if second.Success {
		test.Fatal("the same quote was used twice")
	}
	assertBooksBalance(test, environment)
}

// TestTwoPhonesTakingOverAtOnce: two new phones verify at the same moment.
// The UNIQUE "one active device" column lets exactly one win.
func TestTwoPhonesTakingOverAtOnce(test *testing.T) {
	environment := testenv.Start(test)
	original := environment.RegisterUser(test, "deviceuser")

	type pendingLogin struct {
		client         *testenv.Client
		verificationID string
		code           string
	}
	pending := []pendingLogin{}
	for index := 0; index < 2; index++ {
		phone := environment.NewClient(fmt.Sprintf("new-phone-%d-%s", index, testenv.NewKey()))
		login := phone.Call(test, http.MethodPost, "/api/mobile/v1/auth/login", map[string]any{
			"email": original.Email, "password": "Tribe@2026",
			"device": map[string]string{"imei": phone.DeviceImei, "name": "New phone"},
		}, "")
		var loginData struct {
			Next           string `json:"next"`
			VerificationID string `json:"verification_id"`
			DebugCode      string `json:"debug_code"`
		}
		login.Decode(test, &loginData)
		if loginData.Next != "verify_new_device" {
			test.Fatalf("login on a new phone gave next=%q, want verify_new_device", loginData.Next)
		}
		pending = append(pending, pendingLogin{client: phone, verificationID: loginData.VerificationID, code: loginData.DebugCode})
	}

	var winners atomic.Int64
	var waitGroup sync.WaitGroup
	for _, login := range pending {
		waitGroup.Add(1)
		go func(login pendingLogin) {
			defer waitGroup.Done()
			response := login.client.Call(test, http.MethodPost, "/api/mobile/v1/auth/device/verify", map[string]string{
				"verification_id": login.verificationID, "code": login.code,
			}, "")
			if response.Success {
				winners.Add(1)
			}
		}(login)
	}
	waitGroup.Wait()

	if winners.Load() < 1 {
		test.Fatal("neither new phone could take over")
	}
	var activeDevices int64
	environment.App.Database.Raw("SELECT COUNT(*) FROM user_devices d JOIN users u ON u.id = d.user_id WHERE u.email = ? AND d.status = 'active'", original.Email).Scan(&activeDevices)
	if activeDevices != 1 {
		test.Errorf("%d active devices after two takeovers, want exactly 1", activeDevices)
	}
	// The first phone has been signed out.
	if response := original.Call(test, http.MethodGet, "/api/mobile/v1/wallets", nil, ""); response.Code != "DEVICE_NOT_ACTIVE" {
		test.Errorf("old phone got %d %s, want DEVICE_NOT_ACTIVE", response.Status, response.Code)
	}
}

// assertBooksBalance runs the same check as `artisan ledger:verify`: every
// journal entry balances and every snapshot matches its legs.
func assertBooksBalance(test *testing.T, environment *testenv.Environment) {
	test.Helper()
	problems, err := environment.App.Ledger.Verify(context.Background())
	if err != nil {
		test.Fatalf("ledger verify: %v", err)
	}
	for _, problem := range problems {
		test.Errorf("ledger problem: %+v", problem)
	}
}
