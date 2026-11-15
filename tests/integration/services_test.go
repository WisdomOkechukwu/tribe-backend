package integration

import (
	"net/http"
	"testing"
	"time"

	"github.com/tribe-africa/tribe-backend/tests/testenv"
)

// serviceSwitch is one row of GET /admin-api/services.
type serviceSwitch struct {
	Code    string `json:"code"`
	Enabled bool   `json:"enabled"`
	Version int    `json:"version"`
}

// findService returns the admin's current view of one service.
func findService(test *testing.T, admin *testenv.Client, code string) serviceSwitch {
	test.Helper()
	response := admin.Call(test, http.MethodGet, "/admin-api/services", nil, "")
	if !response.Success {
		test.Fatalf("list services: %d %s", response.Status, response.Message)
	}
	var list []serviceSwitch
	response.Decode(test, &list)
	for _, row := range list {
		if row.Code == code {
			return row
		}
	}
	test.Fatalf("service %s is not in the admin list", code)
	return serviceSwitch{}
}

// setService switches a service on or off as the admin, using the version given.
func setService(test *testing.T, admin *testenv.Client, code string, enabled bool, message string, version int) *testenv.Response {
	test.Helper()
	return admin.Call(test, http.MethodPut, "/admin-api/services/"+code, map[string]any{
		"enabled": enabled, "disabled_message": message, "version": version,
	}, testenv.NewKey())
}

// eventually retries a check for a few seconds. A switch reaches every
// program through the shared configuration copy, which reloads within about
// a second of the admin's change.
func eventually(test *testing.T, what string, check func() bool) {
	test.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			test.Fatalf("still not true after 5 seconds: %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestAdminCanSwitchAServiceOff: an admin switches swaps off. The customer's
// swap quote and swap are refused with 503 SERVICE_DISABLED and the admin's
// message, the app is told the service is off, and a different service (a
// send to a Tribe tag) keeps working. A stale version is refused, and
// switching swaps back on makes quotes work again.
func TestAdminCanSwitchAServiceOff(test *testing.T) {
	environment := testenv.Start(test)
	customer := environment.RegisterUser(test, "switchuser")
	environment.RegisterUser(test, "switchfriend")
	customer.TopUp(test, "USD", "50")
	admin := environment.SignInAdmin(test, "super@tribe.africa")
	const message = "Swaps are paused for maintenance."
	swapQuote := func() *testenv.Response {
		return customer.Call(test, http.MethodPost, "/api/mobile/v1/quotes", map[string]string{
			"transaction_type": "swap", "from_asset": "USD", "to_asset": "GBP", "amount": "20",
		}, "")
	}

	before := findService(test, admin, "swap")
	if !before.Enabled {
		test.Fatalf("swap should start switched on")
	}
	if off := setService(test, admin, "swap", false, message, before.Version); !off.Success {
		test.Fatalf("switch swap off: %d %s", off.Status, off.Message)
	}
	eventually(test, "a swap quote is refused once swap is off", func() bool {
		return swapQuote().Status == http.StatusServiceUnavailable
	})
	if refused := swapQuote(); refused.Code != "SERVICE_DISABLED" || refused.Message != message {
		test.Errorf("swap quote while off: %d %s %q, want 503 SERVICE_DISABLED with the admin's message", refused.Status, refused.Code, refused.Message)
	}
	swap := customer.Call(test, http.MethodPost, "/api/mobile/v1/swaps", map[string]string{"quote_id": "01HXXXXXXXXXXXXXXXXXXXXXXX", "pin": testenv.DemoPin}, testenv.NewKey())
	if swap.Status != http.StatusServiceUnavailable || swap.Code != "SERVICE_DISABLED" {
		test.Errorf("swap while off: %d %s, want 503 SERVICE_DISABLED", swap.Status, swap.Code)
	}

	// The app is told, so it can grey the button out.
	var statuses []struct {
		Code    string `json:"code"`
		Enabled bool   `json:"enabled"`
		Message string `json:"message"`
	}
	customer.Call(test, http.MethodGet, "/api/mobile/v1/services", nil, "").Decode(test, &statuses)
	seenSwap := false
	for _, status := range statuses {
		if status.Code == "swap" {
			seenSwap = true
			if status.Enabled || status.Message != message {
				test.Errorf("the app sees swap as %+v, want switched off with the admin's message", status)
			}
		} else if !status.Enabled {
			test.Errorf("the app sees %s switched off, but only swap was", status.Code)
		}
	}
	if !seenSwap {
		test.Errorf("GET /services does not list swap")
	}

	// A different service is untouched.
	send := customer.Call(test, http.MethodPost, "/api/mobile/v1/sends", map[string]any{
		"recipient_kind": "tribe", "handle": "@switchfriend", "asset": "USD", "amount": "5", "pin": testenv.DemoPin,
	}, testenv.NewKey())
	if !send.Success {
		test.Errorf("a Tribe send while swap is off: %d %s, want it to work", send.Status, send.Message)
	}

	// The version the admin first saw is stale now: someone else's screen cannot undo the switch by accident.
	if stale := setService(test, admin, "swap", true, "", before.Version); stale.Status != http.StatusConflict {
		test.Errorf("a stale version gave %d %s, want 409", stale.Status, stale.Code)
	}
	if on := setService(test, admin, "swap", true, "", findService(test, admin, "swap").Version); !on.Success {
		test.Fatalf("switch swap back on: %d %s", on.Status, on.Message)
	}
	eventually(test, "a swap quote works again once swap is back on", func() bool {
		return swapQuote().Success
	})
}

// TestBillServicesSwitchOffByCategory: bills pick their service from the
// biller's category, so switching electricity off refuses an electricity
// bill (before any money moves) while the customer's balance is untouched.
func TestBillServicesSwitchOffByCategory(test *testing.T) {
	environment := testenv.Start(test)
	customer := environment.RegisterUser(test, "billswitch")
	customer.TopUp(test, "NGN", "20000")
	admin := environment.SignInAdmin(test, "super@tribe.africa")
	balanceBefore := customer.Balance(test, "NGN")

	if off := setService(test, admin, "bill_electricity", false, "", findService(test, admin, "bill_electricity").Version); !off.Success {
		test.Fatalf("switch electricity off: %d %s", off.Status, off.Message)
	}
	eventually(test, "an electricity bill is refused once electricity is off", func() bool {
		return payBill(test, customer, "45021234567").Status == http.StatusServiceUnavailable
	})
	if refused := payBill(test, customer, "45021234567"); refused.Code != "SERVICE_DISABLED" || refused.Message == "" {
		test.Errorf("electricity bill while off: %d %s %q, want 503 SERVICE_DISABLED with a message", refused.Status, refused.Code, refused.Message)
	}
	if balanceAfter := customer.Balance(test, "NGN"); balanceAfter != balanceBefore {
		test.Errorf("the NGN balance moved from %s to %s while the bill was refused", balanceBefore, balanceAfter)
	}
}
