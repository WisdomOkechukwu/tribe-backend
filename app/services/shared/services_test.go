package shared

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/tribe-africa/tribe-backend/database/migrations"
)

// TestEveryServiceHasAMigrationRow guards the rule written above
// ServiceCatalog: a service code added in Go must also be inserted into the
// `services` table by a migration. Without its row a service cannot be
// switched off from the admin panel (it has no row to flip).
func TestEveryServiceHasAMigrationRow(test *testing.T) {
	var allMigrations strings.Builder
	fileNames, err := fs.Glob(migrations.Files, "*.sql")
	if err != nil {
		test.Fatalf("list migrations: %v", err)
	}
	for _, fileName := range fileNames {
		content, err := fs.ReadFile(migrations.Files, fileName)
		if err != nil {
			test.Fatalf("read %s: %v", fileName, err)
		}
		if strings.Contains(string(content), "INTO services") {
			allMigrations.Write(content)
		}
	}
	seen := map[string]bool{}
	for _, definition := range ServiceCatalog {
		if seen[definition.Code] {
			test.Errorf("service %q is in the catalog twice", definition.Code)
		}
		seen[definition.Code] = true
		if len(definition.Code) > 32 {
			test.Errorf("service code %q is longer than services.code (32 characters)", definition.Code)
		}
		if !strings.Contains(allMigrations.String(), "('"+definition.Code+"',") {
			test.Errorf("service %q has no INSERT INTO services row in any migration", definition.Code)
		}
	}
}

// TestSendServiceFor checks every kind of send lands on exactly one service.
func TestSendServiceFor(test *testing.T) {
	cases := []struct {
		recipientKind string
		payoutAsset   string
		want          string
	}{
		{"tribe", "USD", ServiceSendTribe},
		{"tribe", "NGN", ServiceSendTribe},
		{"wallet", "USDT", ServiceSendCrypto},
		{"bank", "NGN", ServiceSendBank},
		{"bank", "GBP", ServiceSendGlobal},
		{"bank", "KES", ServiceSendGlobal},
	}
	for _, testCase := range cases {
		if got := SendServiceFor(testCase.recipientKind, testCase.payoutAsset); got != testCase.want {
			test.Errorf("SendServiceFor(%q, %q) = %q, want %q", testCase.recipientKind, testCase.payoutAsset, got, testCase.want)
		}
	}
}
