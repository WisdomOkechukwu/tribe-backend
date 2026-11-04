package ledger

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/database"
	"github.com/tribe-africa/tribe-backend/pkg/ids"
	"github.com/tribe-africa/tribe-backend/pkg/money"
)

// VerifyProblem describes one thing ledger:verify found wrong.
type VerifyProblem struct {
	Description string
}

// VerifyReport is the result of one verification run.
type VerifyReport struct {
	RunID            uint64          `json:"run_id"`
	Mode             string          `json:"mode"` // full | incremental
	EntriesChecked   int64           `json:"entries_checked"`
	SnapshotsChecked int64           `json:"snapshots_checked"`
	ProblemCount     int64           `json:"problem_count"`
	Problems         []VerifyProblem `json:"problems"` // the first 1000 at most
}

// Verification modes.
const (
	VerifyFull        = "full"
	VerifyIncremental = "incremental"
)

// Page sizes: how many rows one verification query looks at. Memory use
// stays the same however big the books grow.
const (
	verifyEntryPageSize    = 5000
	verifySnapshotPageSize = 1000
	maximumReportedProblem = 1000
)

// verifyOverlap is how far before the last clean run an incremental run
// starts again. A money transaction stamps its time a moment before it
// commits, so an entry stamped just before the last run may only have
// become visible after it. An hour of overlap covers that many times over.
const verifyOverlap = time.Hour

// Verify runs the FULL check of the books and returns what it found (the
// first 1000 problems). Used by tests and `artisan ledger:verify`.
func (service *Service) Verify(requestContext context.Context) ([]VerifyProblem, error) {
	report, err := service.RunVerify(requestContext, VerifyFull)
	if err != nil {
		return nil, err
	}
	return report.Problems, nil
}

// RunVerify checks the books:
//  1. every journal entry balances for every asset (debits = credits),
//  2. every journal entry has at least two legs,
//  3. every snapshot's closing balance equals the sum of ALL legs of its
//     account up to its last leg.
//
// Check 3 is done as a CHAIN instead of re-adding every leg from the start
// for every snapshot (which got slower every day, and loaded every snapshot
// into memory at once). Each snapshot is compared with the one before it:
//
//	this closing = previous closing + the legs between the two
//
// and the first snapshot of an account with zero. If every link holds, then
// by induction every closing balance equals the full sum of the journal, so
// this is still a FULL reconciliation, but each leg is read about once.
//
// Mode "full" checks everything. Mode "incremental" (the nightly job) only
// checks entries and snapshots added since the last CLEAN run, chaining new
// snapshots onto ones that run already proved. If there is no clean run yet
// it does a full run. A full run every week still re-proves everything.
//
// Rows are read in pages (5000 entries, 1000 snapshots), so memory stays
// small however big the books grow. Every run is recorded in
// ledger_verify_runs.
func (service *Service) RunVerify(requestContext context.Context, mode string) (*VerifyReport, error) {
	query := service.database.WithContext(requestContext)
	startedAt := time.Now().UTC()
	run := models.LedgerVerifyRun{Mode: VerifyFull, Status: "running", StartedAt: startedAt}

	// Where to start. An incremental run starts from the last clean run.
	if mode == VerifyIncremental {
		var lastClean models.LedgerVerifyRun
		err := query.Where("status = ?", "clean").Order("id DESC").Limit(1).Take(&lastClean).Error
		if err == nil {
			run.Mode = VerifyIncremental
			run.EntriesFromID = ids.FirstForTime(lastClean.StartedAt.Add(-verifyOverlap))
			run.SnapshotsFromID = lastClean.SnapshotsToID
			run.SnapshotsFromDate = lastClean.SnapshotsToDate
		} else if !database.IsNotFound(err) {
			return nil, err
		}
	}

	// Where to stop: everything that exists right now.
	var newest struct {
		LastID   sql.NullInt64
		LastDate sql.NullTime
	}
	if err := query.Raw("SELECT MAX(id) AS last_id, MAX(snapshot_date) AS last_date FROM ledger_daily_snapshots").Scan(&newest).Error; err != nil {
		return nil, err
	}
	run.SnapshotsToID = uint64(newest.LastID.Int64)
	if newest.LastDate.Valid {
		run.SnapshotsToDate = &newest.LastDate.Time
	}
	if err := query.Create(&run).Error; err != nil {
		return nil, err
	}

	report := &VerifyReport{RunID: run.ID, Mode: run.Mode, Problems: []VerifyProblem{}}
	checks := &verifyChecks{query: query, report: report, seen: map[string]bool{}}
	err := checks.entries(run.EntriesFromID)
	if err == nil {
		err = checks.snapshots(run.SnapshotsFromID, run.SnapshotsToID)
	}
	if err == nil && run.Mode == VerifyIncremental && run.SnapshotsFromDate != nil {
		// Also re-check snapshots of the last checked day and later. A
		// snapshot still being written during the last run (the nightly
		// snapshot job can overlap) may have an id the last run skipped.
		err = checks.snapshotsSince(*run.SnapshotsFromDate, run.SnapshotsFromID)
	}

	finished := time.Now().UTC()
	updates := map[string]any{
		"entries_checked": report.EntriesChecked, "snapshots_checked": report.SnapshotsChecked,
		"problem_count": report.ProblemCount, "finished_at": finished,
	}
	switch {
	case err != nil:
		updates["status"], updates["message"] = "failed", truncateMessage(err.Error())
	case report.ProblemCount > 0:
		updates["status"], updates["message"] = "problems", truncateMessage(report.Problems[0].Description)
	default:
		updates["status"] = "clean"
	}
	// Recorded even if the caller's request was cancelled, so the run
	// never stays "running" forever.
	service.database.WithContext(context.WithoutCancel(requestContext)).Model(&models.LedgerVerifyRun{}).Where("id = ?", run.ID).Updates(updates)
	if err != nil {
		return nil, err
	}
	return report, nil
}

// verifyChecks holds the state of one run while it pages through the books.
type verifyChecks struct {
	query  *gorm.DB
	report *VerifyReport
	seen   map[string]bool // problems already reported (pages can overlap)
}

// add records a problem once, keeping at most 1000 in memory.
func (checks *verifyChecks) add(description string) {
	if checks.seen[description] {
		return
	}
	checks.report.ProblemCount++
	if len(checks.report.Problems) < maximumReportedProblem {
		checks.seen[description] = true
		checks.report.Problems = append(checks.report.Problems, VerifyProblem{Description: description})
	}
}

// entries runs checks 1 and 2 on every journal entry whose id is at or
// after fromID ("" = all), 5000 entries at a time, walking the primary key.
func (checks *verifyChecks) entries(fromID string) error {
	cursor := fromID
	first := true
	for {
		var entryIDs []string
		page := checks.query.Model(&models.JournalEntry{})
		if first {
			page = page.Where("id >= ?", cursor)
		} else {
			page = page.Where("id > ?", cursor)
		}
		if err := page.Order("id ASC").Limit(verifyEntryPageSize).Pluck("id", &entryIDs).Error; err != nil {
			return err
		}
		if len(entryIDs) == 0 {
			return nil
		}
		first = false
		cursor = entryIDs[len(entryIDs)-1]
		checks.report.EntriesChecked += int64(len(entryIDs))

		// 1. Entries that do not balance in some asset.
		var unbalanced []struct {
			JournalEntryID string
			AssetCode      string
			Net            string
		}
		err := checks.query.Raw(
			`SELECT journal_entry_id, asset_code,
			        SUM(CASE WHEN side = 'debit' THEN amount ELSE -amount END) AS net
			   FROM journal_legs
			  WHERE journal_entry_id IN ?
			  GROUP BY journal_entry_id, asset_code
			 HAVING SUM(CASE WHEN side = 'debit' THEN amount ELSE -amount END) <> 0`, entryIDs,
		).Scan(&unbalanced).Error
		if err != nil {
			return err
		}
		for _, row := range unbalanced {
			checks.add(fmt.Sprintf("entry %s does not balance in %s (net %s)", row.JournalEntryID, row.AssetCode, row.Net))
		}

		// 2. Entries with fewer than two legs.
		var lonely []struct {
			ID       string
			LegCount int
		}
		err = checks.query.Raw(
			`SELECT entries.id, COUNT(legs.id) AS leg_count
			   FROM journal_entries entries
			   LEFT JOIN journal_legs legs ON legs.journal_entry_id = entries.id
			  WHERE entries.id IN ?
			  GROUP BY entries.id
			 HAVING COUNT(legs.id) < 2`, entryIDs,
		).Scan(&lonely).Error
		if err != nil {
			return err
		}
		for _, row := range lonely {
			checks.add(fmt.Sprintf("entry %s has only %d legs", row.ID, row.LegCount))
		}
	}
}

// snapshots runs check 3 on snapshots with fromID < id <= toID, 1000 at a time.
func (checks *verifyChecks) snapshots(fromID uint64, toID uint64) error {
	cursor := fromID
	for {
		var snapshotIDs []uint64
		err := checks.query.Model(&models.LedgerDailySnapshot{}).
			Where("id > ? AND id <= ?", cursor, toID).
			Order("id ASC").Limit(verifySnapshotPageSize).Pluck("id", &snapshotIDs).Error
		if err != nil {
			return err
		}
		if len(snapshotIDs) == 0 {
			return nil
		}
		cursor = snapshotIDs[len(snapshotIDs)-1]
		if err := checks.snapshotPage(snapshotIDs); err != nil {
			return err
		}
	}
}

// snapshotsSince runs check 3 on snapshots dated fromDate or later whose id
// is at most upToID (the ones above it were already checked by snapshots).
func (checks *verifyChecks) snapshotsSince(fromDate time.Time, upToID uint64) error {
	cursor := uint64(0)
	for {
		var snapshotIDs []uint64
		err := checks.query.Model(&models.LedgerDailySnapshot{}).
			Where("snapshot_date >= ? AND id > ? AND id <= ?", fromDate, cursor, upToID).
			Order("id ASC").Limit(verifySnapshotPageSize).Pluck("id", &snapshotIDs).Error
		if err != nil {
			return err
		}
		if len(snapshotIDs) == 0 {
			return nil
		}
		cursor = snapshotIDs[len(snapshotIDs)-1]
		if err := checks.snapshotPage(snapshotIDs); err != nil {
			return err
		}
	}
}

// snapshotPage checks one page of snapshots with ONE query.
//
// For each snapshot ("current") the query also finds the snapshot of the
// same account on the closest EARLIER day ("previous") and adds up the legs
// between the two. Leg ids only grow with time, so normally previous ends
// at a lower leg; LEAST/GREATEST make the range work either way round, and
// the sign is fixed in Go. "previous" is found with MAX(snapshot_date) on
// the UNIQUE (account, date) index, which MySQL answers with one index jump.
func (checks *verifyChecks) snapshotPage(snapshotIDs []uint64) error {
	var rows []struct {
		ID               uint64
		SnapshotDate     time.Time
		ClosingBalance   string
		LastJournalLegID uint64
		AccountNumber    string
		NormalSide       string
		PreviousClosing  sql.NullString
		PreviousLastLeg  uint64
		NetDebits        sql.NullString
	}
	err := checks.query.Raw(`
		SELECT current.id, current.snapshot_date, current.closing_balance, current.last_journal_leg_id,
		       accounts.account_number, accounts.normal_side,
		       previous.closing_balance AS previous_closing,
		       COALESCE(previous.last_journal_leg_id, 0) AS previous_last_leg,
		       (SELECT SUM(CASE WHEN legs.side = 'debit' THEN legs.amount ELSE -legs.amount END)
		          FROM journal_legs legs
		         WHERE legs.ledger_account_id = current.ledger_account_id
		           AND legs.id >  LEAST(COALESCE(previous.last_journal_leg_id, 0), current.last_journal_leg_id)
		           AND legs.id <= GREATEST(COALESCE(previous.last_journal_leg_id, 0), current.last_journal_leg_id)) AS net_debits
		  FROM ledger_daily_snapshots current
		  JOIN ledger_accounts accounts ON accounts.id = current.ledger_account_id
		  LEFT JOIN ledger_daily_snapshots previous
		         ON previous.ledger_account_id = current.ledger_account_id
		        AND previous.snapshot_date = (SELECT MAX(earlier.snapshot_date)
		                                        FROM ledger_daily_snapshots earlier
		                                       WHERE earlier.ledger_account_id = current.ledger_account_id
		                                         AND earlier.snapshot_date < current.snapshot_date)
		 WHERE current.id IN ?`, snapshotIDs).Scan(&rows).Error
	if err != nil {
		return err
	}
	checks.report.SnapshotsChecked += int64(len(rows))
	for _, row := range rows {
		previousClosing, err := money.ParseUnits(row.PreviousClosing.String) // "" (no earlier snapshot) = 0
		if err != nil {
			return err
		}
		movement, err := money.ParseUnits(row.NetDebits.String)
		if err != nil {
			return err
		}
		if row.NormalSide == Credit {
			movement = movement.Negate()
		}
		if row.LastJournalLegID < row.PreviousLastLeg {
			movement = movement.Negate() // the legs between were counted backwards
		}
		expected := previousClosing.Add(movement)
		closing, err := money.ParseUnits(row.ClosingBalance)
		if err != nil {
			return err
		}
		if closing.Compare(expected) != 0 {
			checks.add(fmt.Sprintf("snapshot %s of account %s says %s but the journal adds up to %s",
				row.SnapshotDate.Format(time.DateOnly), row.AccountNumber, closing.UnitsString(), expected.UnitsString()))
		}
	}
	return nil
}

// truncateMessage keeps a message within the 500-character column.
func truncateMessage(message string) string {
	if len(message) > 500 {
		return message[:500]
	}
	return message
}
