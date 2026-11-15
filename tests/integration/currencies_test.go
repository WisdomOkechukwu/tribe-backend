package integration

import (
	"net/http"
	"testing"

	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/app/services/ledger"
	"github.com/tribe-africa/tribe-backend/tests/testenv"
)

// TestAdminCanAddACurrency: an admin adds a new coin from the admin API. It
// starts switched off (the app does not list it and a wallet cannot be
// opened in it), it has every company ledger account and a margin row, and
// once switched on a customer sees it and can open a wallet in it. The code
// cannot be added twice, a stale version is refused, and Naira can never be
// switched off.
//
// The coin is pegged to the dollar (usd_peg), so the test never needs a
// price feed to answer.
func TestAdminCanAddACurrency(test *testing.T) {
	environment := testenv.Start(test)
	customer := environment.RegisterUser(test, "newcoinuser")
	admin := environment.SignInAdmin(test, "super@tribe.africa")

	// The new coin is converted by the same provider as USDT.
	var tether models.Asset
	if err := environment.App.Database.Where("code = ?", "USDT").Take(&tether).Error; err != nil || tether.ConversionProviderID == nil {
		test.Fatalf("read USDT and its provider: %v", err)
	}
	newCoin := map[string]any{
		"code": "TUSD", "name": "TrueUSD", "kind": "crypto", "symbol": "T$", "decimal_places": 6,
		"conversion_provider_id": *tether.ConversionProviderID, "price_source": "usd_peg",
	}
	appListsCoin := func() bool {
		var assets []struct {
			Code string `json:"code"`
		}
		customer.Call(test, http.MethodGet, "/api/mobile/v1/assets", nil, "").Decode(test, &assets)
		for _, asset := range assets {
			if asset.Code == "TUSD" {
				return true
			}
		}
		return false
	}

	// A coin cannot be priced from the fiat feed: refused before anything is saved.
	wrongSource := map[string]any{}
	for key, value := range newCoin {
		wrongSource[key] = value
	}
	wrongSource["price_source"] = "fiat_feed"
	if refused := admin.Call(test, http.MethodPost, "/admin-api/currencies", wrongSource, testenv.NewKey()); refused.Status != http.StatusUnprocessableEntity {
		test.Errorf("a coin priced from the fiat feed gave %d %s, want 422", refused.Status, refused.Code)
	}

	created := admin.Call(test, http.MethodPost, "/admin-api/currencies", newCoin, testenv.NewKey())
	if created.Status != http.StatusCreated {
		test.Fatalf("add TUSD: %d %s %v", created.Status, created.Message, created.Errors)
	}
	var coin struct {
		Enabled bool `json:"enabled"`
		Version int  `json:"version"`
	}
	created.Decode(test, &coin)
	if coin.Enabled {
		test.Errorf("a new currency should start switched off")
	}
	if again := admin.Call(test, http.MethodPost, "/admin-api/currencies", newCoin, testenv.NewKey()); again.Code != "CURRENCY_EXISTS" {
		test.Errorf("adding TUSD twice gave %d %s, want 409 CURRENCY_EXISTS", again.Status, again.Code)
	}

	// Everything money in the new currency needs is there.
	var companyAccounts int64
	environment.App.Database.Model(&models.LedgerAccount{}).Where("owner_key = ? AND asset_code = ?", "company", "TUSD").Count(&companyAccounts)
	if int(companyAccounts) != len(ledger.CompanyAccountTemplates) {
		test.Errorf("TUSD has %d company accounts, want %d", companyAccounts, len(ledger.CompanyAccountTemplates))
	}
	var margins int64
	environment.App.Database.Model(&models.RateMargin{}).Where("asset_code = ?", "TUSD").Count(&margins)
	if margins != 1 {
		test.Errorf("TUSD has %d margin rows, want 1", margins)
	}

	// While it is off the app does not see it and no wallet can be opened in it.
	if appListsCoin() {
		test.Errorf("the app lists TUSD before it is switched on")
	}
	if opened := customer.Call(test, http.MethodPost, "/api/mobile/v1/wallets/TUSD/open", nil, testenv.NewKey()); opened.Success {
		test.Errorf("a wallet was opened in a currency that is switched off")
	}

	switchTo := func(code string, enabled bool, version int) *testenv.Response {
		return admin.Call(test, http.MethodPut, "/admin-api/currencies/"+code, map[string]any{"enabled": enabled, "version": version}, testenv.NewKey())
	}
	if on := switchTo("TUSD", true, coin.Version); !on.Success {
		test.Fatalf("switch TUSD on: %d %s", on.Status, on.Message)
	}
	if stale := switchTo("TUSD", false, coin.Version); stale.Status != http.StatusConflict {
		test.Errorf("a stale version gave %d %s, want 409", stale.Status, stale.Code)
	}
	eventually(test, "the app lists TUSD once it is switched on", appListsCoin)
	eventually(test, "a wallet can be opened in TUSD once it is switched on", func() bool {
		return customer.Call(test, http.MethodPost, "/api/mobile/v1/wallets/TUSD/open", nil, testenv.NewKey()).Success
	})

	// Naira is the rail everything else runs on: it stays on.
	var naira models.Asset
	environment.App.Database.Where("code = ?", "NGN").Take(&naira)
	if refused := switchTo("NGN", false, naira.Version); refused.Code != "CURRENCY_ALWAYS_ON" {
		test.Errorf("switching Naira off gave %d %s, want CURRENCY_ALWAYS_ON", refused.Status, refused.Code)
	}
	assertBooksBalance(test, environment)
}
