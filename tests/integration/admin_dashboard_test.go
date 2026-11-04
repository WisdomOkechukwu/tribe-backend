package integration

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/tribe-africa/tribe-backend/tests/testenv"
)

// TestSplitOverviewJobsAndCounts checks the small dashboard reads the admin
// panel loads one card at a time: /overview/..., /jobs/... and
// /verifications/counts. Each must answer 200 with the agreed shape, and a
// list must be [] (never null) when it is empty.
func TestSplitOverviewJobsAndCounts(test *testing.T) {
	environment := testenv.Start(test)
	customer := environment.RegisterUser(test, "dashboard")
	customer.TopUp(test, "USD", "25") // at least one completed transaction
	admin := environment.SignInAdmin(test, "super@tribe.africa")

	get := func(path string) *testenv.Response {
		test.Helper()
		response := admin.Call(test, http.MethodGet, path, nil, "")
		if response.Status != http.StatusOK || !response.Success {
			test.Fatalf("GET %s = %d %s %s, want 200", path, response.Status, response.Code, response.Message)
		}
		return response
	}
	requireArray := func(path string, response *testenv.Response) []map[string]any {
		test.Helper()
		if !bytes.HasPrefix(bytes.TrimSpace(response.Data), []byte("[")) {
			test.Fatalf("GET %s data = %s, want a JSON array", path, string(response.Data))
		}
		var items []map[string]any
		response.Decode(test, &items)
		return items
	}
	requireKeys := func(path string, item map[string]any, keys ...string) {
		test.Helper()
		for _, key := range keys {
			if _, found := item[key]; !found {
				test.Errorf("GET %s: item %v has no %q", path, item, key)
			}
		}
	}

	// Overview stats: one object with every headline number.
	var stats map[string]any
	get("/admin-api/overview/stats").Decode(test, &stats)
	statKeys := []string{"as_of", "customers", "signups_7", "kyc_pending", "open_tickets", "transactions_24",
		"failed_24", "fail_rate_24", "pending_all", "volume_1_usd", "volume_7_usd"}
	requireKeys("/admin-api/overview/stats", stats, statKeys...)
	if len(stats) != len(statKeys) {
		test.Errorf("overview stats has %d keys, want exactly %d: %v", len(stats), len(statKeys), stats)
	}
	if customers, _ := stats["customers"].(float64); customers < 1 {
		test.Errorf("overview stats customers = %v, want at least 1", stats["customers"])
	}

	series := requireArray("/admin-api/overview/series", get("/admin-api/overview/series"))
	if len(series) == 0 {
		test.Errorf("overview series is empty, want today's top-up")
	}
	for _, day := range series {
		requireKeys("/admin-api/overview/series", day, "day", "volume_usd", "count", "failed")
	}
	byCategory := requireArray("/admin-api/overview/by-category", get("/admin-api/overview/by-category"))
	for _, category := range byCategory {
		requireKeys("/admin-api/overview/by-category", category, "category", "volume_usd", "count")
	}
	requireArray("/admin-api/overview/failures", get("/admin-api/overview/failures"))

	// The combined /overview still has every key.
	var overview map[string]any
	get("/admin-api/overview").Decode(test, &overview)
	requireKeys("/admin-api/overview", overview, append(statKeys, "series", "by_category_7", "failed_recent", "inventory")...)

	// Jobs: four arrays, plus the combined object.
	for _, path := range []string{"/admin-api/jobs/runs", "/admin-api/jobs/queues", "/admin-api/jobs/failed", "/admin-api/jobs/schedule"} {
		requireArray(path, get(path))
	}
	schedule := requireArray("/admin-api/jobs/schedule", get("/admin-api/jobs/schedule"))
	if len(schedule) == 0 {
		test.Errorf("job schedule is empty, want the cron jobs")
	}
	for _, job := range schedule {
		requireKeys("/admin-api/jobs/schedule", job, "name", "cron")
	}
	var jobs map[string]any
	get("/admin-api/jobs").Decode(test, &jobs)
	requireKeys("/admin-api/jobs", jobs, "schedule", "cron_runs", "queues", "failed_jobs")

	// Verification counts: every status the list filters by, plus all.
	var counts map[string]int64
	get("/admin-api/verifications/counts").Decode(test, &counts)
	for _, status := range []string{"pending", "approved", "rejected", "all"} {
		if _, found := counts[status]; !found {
			test.Errorf("verification counts %v has no %q", counts, status)
		}
	}
	if counts["all"] != counts["pending"]+counts["approved"]+counts["rejected"] {
		test.Errorf("verification counts %v: all is not the sum of the statuses", counts)
	}

	// The same permission as the combined routes: support has no jobs.view.
	support := environment.SignInAdmin(test, "support@tribe.africa")
	if denied := support.Call(test, http.MethodGet, "/admin-api/jobs/runs", nil, ""); denied.Status != http.StatusForbidden {
		test.Errorf("support reading /jobs/runs = %d, want 403", denied.Status)
	}
}
