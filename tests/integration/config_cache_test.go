package integration

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/tribe-africa/tribe-backend/app/cache"
	"github.com/tribe-africa/tribe-backend/pkg/money"
	"github.com/tribe-africa/tribe-backend/tests/testenv"
)

// percentOf reads a DECIMAL fee percent ("1.000000") as a whole number.
func percentOf(test *testing.T, text string) int64 {
	test.Helper()
	rate, err := money.ParseRate(text)
	if err != nil || !rate.IsInt() {
		test.Fatalf("fee percent %q is not a whole number: %v", text, err)
	}
	return rate.Num().Int64()
}

// TestFeeChangeReachesEveryProgramQuickly checks the configuration cache
// never serves an old fee for long after an admin edits it:
//   - the program that saved the change uses the new fee on the very next
//     payment;
//   - another program (here: a second cache, like the worker's) sees it
//     within about a second, through the Redis version counter.
func TestFeeChangeReachesEveryProgramQuickly(test *testing.T) {
	environment := testenv.Start(test)
	sender := environment.RegisterUser(test, "feesender")
	environment.RegisterUser(test, "feefriend")
	sender.TopUp(test, "USD", "50")

	otherProgram := cache.NewConfigCache(environment.App.Database, environment.App.Redis)
	before, err := otherProgram.Snapshot(context.Background())
	if err != nil {
		test.Fatalf("snapshot: %v", err)
	}
	if percentOf(test, before.Fees["send_tribe"].FeePercent) != 0 {
		test.Fatalf("seeded send_tribe fee = %s%%, want 0", before.Fees["send_tribe"].FeePercent)
	}

	admin := environment.SignInAdmin(test, "super@tribe.africa")
	saved := admin.Call(test, http.MethodPut, "/admin-api/transaction-fees/send_tribe", map[string]any{
		"fee_percent": "1", "flat_fee": "0", "flat_fee_asset": "USD", "active": true, "version": 1,
	}, testenv.NewKey())
	if !saved.Success {
		test.Fatalf("save fee: %s %s", saved.Code, saved.Message)
	}

	// The very next payment pays the new 1% fee: 10.00 + 0.10.
	response := sender.Call(test, http.MethodPost, "/api/mobile/v1/sends", map[string]any{
		"recipient_kind": "tribe", "handle": "@feefriend", "asset": "USD", "amount": "10", "pin": testenv.DemoPin,
	}, testenv.NewKey())
	if !response.Success {
		test.Fatalf("send failed: %s", response.Message)
	}
	if balance := sender.Balance(test, "USD"); balance != "39.90" {
		test.Errorf("balance = %s, want 39.90 (the new 1%% fee must apply at once)", balance)
	}

	// Another program picks the change up within about a second.
	deadline := time.Now().Add(3 * time.Second)
	for {
		after, err := otherProgram.Snapshot(context.Background())
		if err != nil {
			test.Fatalf("snapshot: %v", err)
		}
		if percentOf(test, after.Fees["send_tribe"].FeePercent) == 1 {
			break
		}
		if time.Now().After(deadline) {
			test.Fatalf("another program still sees the old fee after 3 seconds")
		}
		time.Sleep(100 * time.Millisecond)
	}
	assertBooksBalance(test, environment)
}
