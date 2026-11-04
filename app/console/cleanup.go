package console

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tribe-africa/tribe-backend/app/bootstrap"
	"github.com/tribe-africa/tribe-backend/database"
)

// retentionRule says which rows of one operational table are old enough to
// delete. Only short-lived, operational tables are listed here.
//
// Deliberately NOT listed (kept forever):
//   - audit_logs: the record of who changed what; required for audits.
//   - journal_entries, journal_legs, ledger_daily_snapshots: the books.
//   - transactions, inbox_messages: the customer's own history.
type retentionRule struct {
	table     string
	condition string // rows matching this are deleted; the ? is the cut-off time
	keepFor   time.Duration
}

// retentionRules lists every table the daily cleanup trims.
func retentionRules(application *bootstrap.App) []retentionRule {
	return []retentionRule{
		// The admin "API trail": an operational log, 90 days by default.
		{table: "api_request_logs", condition: "created_at < ?", keepFor: application.Settings.App.ApiLogRetention},
		// Webhooks are only kept to spot duplicates. Providers retry for
		// hours or days, never months, so 180 days of processed events is
		// plenty. Failed or unprocessed events are kept for investigation.
		{table: "provider_webhook_events", condition: "status = 'processed' AND received_at < ?", keepFor: 180 * 24 * time.Hour},
		// Sessions that expired more than 90 days ago. A signed-out session
		// still has its original expiry, so it is removed the same way.
		{table: "user_sessions", condition: "expires_at < ?", keepFor: 90 * 24 * time.Hour},
		{table: "admin_sessions", condition: "expires_at < ?", keepFor: 90 * 24 * time.Hour},
		// One-time codes live minutes; a week covers any support question.
		{table: "email_verifications", condition: "created_at < ?", keepFor: 7 * 24 * time.Hour},
		{table: "phone_verifications", condition: "created_at < ?", keepFor: 7 * 24 * time.Hour},
		{table: "device_verifications", condition: "created_at < ?", keepFor: 7 * 24 * time.Hour},
		// The admin Jobs page shows the latest 100 runs.
		{table: "scheduled_job_runs", condition: "started_at < ?", keepFor: 90 * 24 * time.Hour},
	}
}

// RunDailyCleanup deletes old rows from the operational tables above, a few
// thousand at a time (see database.DeleteInBatches).
func RunDailyCleanup(requestContext context.Context, application *bootstrap.App) (string, error) {
	var summary []string
	for _, rule := range retentionRules(application) {
		cutOff := time.Now().UTC().Add(-rule.keepFor)
		// The same cut-off fills every ? in the condition.
		arguments := make([]any, strings.Count(rule.condition, "?"))
		for index := range arguments {
			arguments[index] = cutOff
		}
		removed, err := database.DeleteInBatches(requestContext, application.Database, rule.table, rule.condition, arguments...)
		if err != nil {
			return strings.Join(summary, ", "), fmt.Errorf("cleaning %s: %w", rule.table, err)
		}
		summary = append(summary, fmt.Sprintf("%s %d", rule.table, removed))
	}
	return "removed " + strings.Join(summary, ", "), nil
}
