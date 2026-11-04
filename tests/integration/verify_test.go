package integration

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tribe-africa/tribe-backend/app/console"
	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/app/services/ledger"
	"github.com/tribe-africa/tribe-backend/tests/testenv"
)

// TestIncrementalVerifyChecksOnlyNewRowsButStillCatchesProblems runs the
// nightly (incremental) check three times:
//  1. no clean run yet, so it checks everything (a full run);
//  2. after more money moves and a new snapshot, it checks only the new
//     entries and the new snapshot, and stays clean;
//  3. after a NEW snapshot is tampered with, it reports it, and the
//     starting point does not move until the problem is fixed.
func TestIncrementalVerifyChecksOnlyNewRowsButStillCatchesProblems(test *testing.T) {
	environment := testenv.Start(test)
	user := environment.RegisterUser(test, "verifyuser")
	environment.RegisterUser(test, "verifyfriend")
	user.TopUp(test, "USD", "40")
	send := func(amount string) {
		response := user.Call(test, http.MethodPost, "/api/mobile/v1/sends", map[string]any{
			"recipient_kind": "tribe", "handle": "@verifyfriend", "asset": "USD", "amount": amount, "pin": testenv.DemoPin,
		}, testenv.NewKey())
		if !response.Success {
			test.Fatalf("send failed: %s", response.Message)
		}
	}
	ledgerService := environment.App.Ledger
	database := environment.App.Database
	timeZone := environment.App.Settings.App.TimeZone
	day := func(daysAgo int) string { return time.Now().In(timeZone).AddDate(0, 0, -daysAgo).Format(time.DateOnly) }

	send("3")
	for _, snapshotDay := range []string{day(2), day(1)} {
		if _, err := ledgerService.WriteDailySnapshots(context.Background(), snapshotDay); err != nil {
			test.Fatalf("snapshot: %v", err)
		}
	}

	// 1. First run: nothing clean yet, so everything is checked.
	first, err := ledgerService.RunVerify(context.Background(), ledger.VerifyIncremental)
	if err != nil {
		test.Fatalf("first run: %v", err)
	}
	var allSnapshots int64
	database.Model(&models.LedgerDailySnapshot{}).Count(&allSnapshots)
	if first.Mode != ledger.VerifyFull || first.ProblemCount != 0 || first.SnapshotsChecked != allSnapshots {
		test.Fatalf("first run = %+v, want a clean full run over %d snapshots", first, allSnapshots)
	}

	// 2. More activity and today's snapshot: only the new rows are checked.
	send("4")
	if _, err := ledgerService.WriteDailySnapshots(context.Background(), day(0)); err != nil {
		test.Fatalf("snapshot: %v", err)
	}
	// It should check today's new snapshots plus yesterday's again (the
	// last day the previous run saw, re-checked as overlap), not day(2).
	var expected int64
	database.Model(&models.LedgerDailySnapshot{}).Where("snapshot_date >= ?", day(1)).Count(&expected)
	second, err := ledgerService.RunVerify(context.Background(), ledger.VerifyIncremental)
	if err != nil {
		test.Fatalf("second run: %v", err)
	}
	if second.Mode != ledger.VerifyIncremental || second.ProblemCount != 0 {
		test.Fatalf("second run = %+v, want a clean incremental run", second)
	}
	if second.SnapshotsChecked != expected {
		test.Errorf("incremental run checked %d snapshots, want %d (today + yesterday)", second.SnapshotsChecked, expected)
	}

	// 3. Tamper with one of today's snapshots (snapshots are not
	// insert-only like journal legs, so this is possible with raw SQL).
	var victim models.LedgerDailySnapshot
	database.Where("snapshot_date = ? AND closing_balance > 0", day(0)).Order("id").Take(&victim)
	database.Exec("UPDATE ledger_daily_snapshots SET closing_balance = closing_balance + 1 WHERE id = ?", victim.ID)
	third, err := ledgerService.RunVerify(context.Background(), ledger.VerifyIncremental)
	if err != nil {
		test.Fatalf("third run: %v", err)
	}
	if third.ProblemCount != 1 || !strings.Contains(third.Problems[0].Description, day(0)) {
		test.Fatalf("third run should report the tampered snapshot, got %+v", third)
	}
	var lastClean models.LedgerVerifyRun
	database.Where("status = ?", "clean").Order("id DESC").Take(&lastClean)
	if lastClean.ID != second.RunID {
		test.Errorf("a run with problems moved the starting point (last clean run %d, want %d)", lastClean.ID, second.RunID)
	}

	// The full check agrees, and once fixed everything is clean again.
	if problems, _ := ledgerService.Verify(context.Background()); len(problems) != 1 {
		test.Errorf("full check found %d problems, want 1", len(problems))
	}
	database.Exec("UPDATE ledger_daily_snapshots SET closing_balance = closing_balance - 1 WHERE id = ?", victim.ID)
	assertBooksBalance(test, environment)
}

// TestSnapshotCatchUpWritesMissedDaysOldestFirst pretends the scheduler was
// down for two nights: the catch-up writes both missing days in order,
// marks them complete, and a second call finds nothing left to do.
func TestSnapshotCatchUpWritesMissedDaysOldestFirst(test *testing.T) {
	environment := testenv.Start(test)
	user := environment.RegisterUser(test, "catchupuser")
	user.TopUp(test, "USD", "15")
	database := environment.App.Database
	timeZone := environment.App.Settings.App.TimeZone
	day := func(daysAgo int) string { return time.Now().In(timeZone).AddDate(0, 0, -daysAgo).Format(time.DateOnly) }

	// The last night that ran was three days ago.
	if _, err := environment.App.Ledger.WriteDailySnapshots(context.Background(), day(3)); err != nil {
		test.Fatalf("snapshot: %v", err)
	}
	message, err := console.CatchUpDailySnapshots(context.Background(), environment.App)
	if err != nil {
		test.Fatalf("catch-up: %v", err)
	}
	test.Log(message)

	var accounts int64
	database.Model(&models.LedgerAccount{}).Count(&accounts)
	for _, missedDay := range []string{day(2), day(1)} {
		var written int64
		database.Model(&models.LedgerDailySnapshot{}).Where("snapshot_date = ?", missedDay).Count(&written)
		if written != accounts {
			test.Errorf("day %s has %d snapshots, want one per account (%d)", missedDay, written, accounts)
		}
		var marked int64
		database.Model(&models.ScheduledJobRun{}).Where("job_name = ? AND window_key = ? AND status = ?", "ledger:snapshot-day", missedDay, "succeeded").Count(&marked)
		if marked != 1 {
			test.Errorf("day %s is not marked complete", missedDay)
		}
	}
	again, err := console.CatchUpDailySnapshots(context.Background(), environment.App)
	if err != nil || !strings.Contains(again, "complete") {
		test.Errorf("second catch-up = %q, %v; want nothing left to do", again, err)
	}
	if balance := user.Balance(test, "USD"); balance != "15.00" {
		test.Errorf("balance = %s, want 15.00", balance)
	}
	assertBooksBalance(test, environment)
}
