package integration

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tribe-africa/tribe-backend/app/console"
	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/tests/testenv"
)

// TestOversizedBodyIsRefusedBeforeItIsRead sends a body bigger than
// REQUEST_BODY_MAX_BYTES. It must be refused with 413 before the API trail,
// rate limits or controllers hold it in memory.
func TestOversizedBodyIsRefusedBeforeItIsRead(test *testing.T) {
	environment := testenv.Start(test)
	huge := `{"email":"` + strings.Repeat("a", int(environment.App.Settings.App.RequestBodyMaxBytes)) + `@tribe.africa"}`
	request, _ := http.NewRequest(http.MethodPost, environment.Server.URL+"/api/mobile/v1/auth/email/check", bytes.NewBufferString(huge))
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		test.Fatalf("request: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		test.Errorf("status = %d, want 413", response.StatusCode)
	}
}

// TestApiTrailIsSavedInBatchesWithSecretsHidden makes a money call and waits
// for the batched writer to save its API trail row, with the PIN hidden.
func TestApiTrailIsSavedInBatchesWithSecretsHidden(test *testing.T) {
	environment := testenv.Start(test)
	user := environment.RegisterUser(test, "trailuser")
	environment.RegisterUser(test, "trailfriend")
	user.TopUp(test, "USD", "5")
	response := user.Call(test, http.MethodPost, "/api/mobile/v1/sends", map[string]any{
		"recipient_kind": "tribe", "handle": "@trailfriend", "asset": "USD", "amount": "1", "pin": testenv.DemoPin,
	}, testenv.NewKey())
	if !response.Success {
		test.Fatalf("send failed: %s", response.Message)
	}

	var row models.ApiRequestLog
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := environment.App.Database.Where("path = ? AND method = 'POST'", "/api/mobile/v1/sends").Take(&row).Error
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			test.Fatalf("the API trail row was never saved: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if row.RequestBody == nil || strings.Contains(*row.RequestBody, testenv.DemoPin) || !strings.Contains(*row.RequestBody, "[hidden]") {
		test.Errorf("request body should hide the PIN: %v", row.RequestBody)
	}
}

// TestDailyCleanupKeepsRecentRowsAndTheBooks deletes an old API trail row
// and a used code, keeps recent ones, and never touches the ledger.
func TestDailyCleanupKeepsRecentRowsAndTheBooks(test *testing.T) {
	environment := testenv.Start(test)
	user := environment.RegisterUser(test, "cleanupuser")
	user.TopUp(test, "USD", "5")
	database := environment.App.Database
	old := time.Now().UTC().Add(-200 * 24 * time.Hour)
	recent := time.Now().UTC().Add(-time.Hour)
	for _, createdAt := range []time.Time{old, recent} {
		row := models.ApiRequestLog{RequestID: "cleanup", ActorType: "user", Method: "GET", Path: "/cleanup-test", Status: 200, CreatedAt: createdAt}
		if err := database.Create(&row).Error; err != nil {
			test.Fatalf("insert log row: %v", err)
		}
	}
	var legsBefore int64
	database.Model(&models.JournalLeg{}).Count(&legsBefore)

	message, err := console.RunDailyCleanup(context.Background(), environment.App)
	if err != nil {
		test.Fatalf("cleanup: %v", err)
	}
	test.Log(message)

	var oldCount, recentCount, legsAfter int64
	database.Model(&models.ApiRequestLog{}).Where("path = ? AND created_at < ?", "/cleanup-test", recent.Add(-time.Minute)).Count(&oldCount)
	database.Model(&models.ApiRequestLog{}).Where("path = ? AND created_at >= ?", "/cleanup-test", recent.Add(-time.Minute)).Count(&recentCount)
	database.Model(&models.JournalLeg{}).Count(&legsAfter)
	if oldCount != 0 || recentCount != 1 {
		test.Errorf("after cleanup: %d old rows (want 0), %d recent rows (want 1)", oldCount, recentCount)
	}
	if legsAfter != legsBefore {
		test.Errorf("cleanup changed the books: %d legs before, %d after", legsBefore, legsAfter)
	}
	if balance := user.Balance(test, "USD"); balance != "5.00" {
		test.Errorf("balance = %s after cleanup, want 5.00", balance)
	}
}
