package ledger

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/database"
	"github.com/tribe-africa/tribe-backend/pkg/money"
)

// WriteDailySnapshots writes the closing balance of every account for one
// Lagos calendar day (format "2026-10-22"). The scheduler runs it every night
// at 00:05 for the day that just ended.
//
// For each account:
//
//	closing balance = previous snapshot + legs posted during the day
//
// It is safe to run twice: UNIQUE(account, date) means a day that already
// has a snapshot is simply skipped.
func (service *Service) WriteDailySnapshots(requestContext context.Context, snapshotDate string) (int, error) {
	day, err := time.ParseInLocation(time.DateOnly, snapshotDate, service.timeZone)
	if err != nil {
		return 0, fmt.Errorf("snapshot date must look like 2026-10-22: %w", err)
	}
	// Legs created before midnight at the END of the day belong to this day.
	endOfDayUTC := day.AddDate(0, 0, 1).UTC()
	dateOnly := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)

	written := 0
	lastAccountID := uint64(0)
	for {
		// Walk through accounts 200 at a time so memory stays small.
		var accounts []models.LedgerAccount
		err := service.database.WithContext(requestContext).
			Where("id > ?", lastAccountID).
			Order("id ASC").
			Limit(200).
			Find(&accounts).Error
		if err != nil {
			return written, err
		}
		if len(accounts) == 0 {
			return written, nil
		}
		for _, account := range accounts {
			lastAccountID = account.ID
			created, err := service.writeOneSnapshot(requestContext, account.ID, dateOnly, endOfDayUTC)
			if err != nil {
				return written, err
			}
			if created {
				written++
			}
		}
	}
}

// writeOneSnapshot writes the snapshot of one account in its own short transaction.
func (service *Service) writeOneSnapshot(requestContext context.Context, accountID uint64, snapshotDate time.Time, endOfDayUTC time.Time) (bool, error) {
	created := false
	err := database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		// Lock the account so no new leg can be added while we add up.
		var account models.LedgerAccount
		if err := database.LockForUpdate(databaseTransaction, &account, "id = ?", accountID); err != nil {
			return err
		}

		// Skip if this day already has a snapshot.
		var existingCount int64
		databaseTransaction.Model(&models.LedgerDailySnapshot{}).
			Where("ledger_account_id = ? AND snapshot_date = ?", accountID, snapshotDate).
			Count(&existingCount)
		if existingCount > 0 {
			return nil
		}

		// The previous snapshot (before this day) is our starting point.
		previousBalance := money.Zero()
		previousLastLeg := uint64(0)
		var previous models.LedgerDailySnapshot
		err := databaseTransaction.
			Where("ledger_account_id = ? AND snapshot_date < ?", accountID, snapshotDate).
			Order("snapshot_date DESC").
			Limit(1).
			Take(&previous).Error
		if err == nil {
			previousBalance, err = money.ParseUnits(previous.ClosingBalance)
			if err != nil {
				return err
			}
			previousLastLeg = previous.LastJournalLegID
		} else if !database.IsNotFound(err) {
			return err
		}

		// Add up the legs of this day, cut by LEG ID, not by time.
		//
		// A leg's created_at is taken before Post waits for the account
		// locks, so near midnight a leg with a HIGHER id can carry an EARLIER
		// time than the leg before it (A stamps 23:59:59, waits; B stamps
		// 00:00:01 and commits leg 100; A commits leg 101). Picking legs by
		// "created_at < midnight" would take 101 but not 100, record 101 as
		// the last leg, and every balance after that would skip leg 100.
		//
		// So: the boundary is the highest id stamped before midnight, and the
		// snapshot covers EVERY leg up to that id. Balances (snapshot + legs
		// with id > last_journal_leg_id) can then never miss one; at worst a
		// leg stamped just after midnight is counted in the day before.
		var boundary sql.NullInt64
		err = databaseTransaction.Raw(
			`SELECT MAX(id) FROM journal_legs WHERE ledger_account_id = ? AND id > ? AND created_at < ?`,
			accountID, previousLastLeg, endOfDayUTC,
		).Scan(&boundary).Error
		if err != nil {
			return err
		}
		upTo := previousLastLeg
		if boundary.Valid {
			upTo = uint64(boundary.Int64)
		}
		var totals struct {
			TotalDebits  sql.NullString
			TotalCredits sql.NullString
			LastLegID    sql.NullInt64
		}
		err = databaseTransaction.Raw(
			`SELECT SUM(CASE WHEN side = 'debit'  THEN amount ELSE 0 END) AS total_debits,
			        SUM(CASE WHEN side = 'credit' THEN amount ELSE 0 END) AS total_credits,
			        MAX(id) AS last_leg_id
			   FROM journal_legs
			  WHERE ledger_account_id = ? AND id > ? AND id <= ?`,
			accountID, previousLastLeg, upTo,
		).Scan(&totals).Error
		if err != nil {
			return err
		}
		totalDebits, err := money.ParseUnits(totals.TotalDebits.String)
		if err != nil {
			return err
		}
		totalCredits, err := money.ParseUnits(totals.TotalCredits.String)
		if err != nil {
			return err
		}
		lastLegID := previousLastLeg
		if totals.LastLegID.Valid {
			lastLegID = uint64(totals.LastLegID.Int64)
		}

		// Movement on the normal side increases the balance.
		movement := totalDebits.Subtract(totalCredits)
		if account.NormalSide == Credit {
			movement = movement.Negate()
		}

		snapshot := models.LedgerDailySnapshot{
			LedgerAccountID:  accountID,
			SnapshotDate:     snapshotDate,
			ClosingBalance:   previousBalance.Add(movement).UnitsString(),
			TotalDebits:      totalDebits.UnitsString(),
			TotalCredits:     totalCredits.UnitsString(),
			LastJournalLegID: lastLegID,
			CreatedAt:        time.Now().UTC(),
		}
		if err := databaseTransaction.Create(&snapshot).Error; err != nil {
			if database.IsDuplicateEntry(err) {
				return nil // another process wrote it first: fine
			}
			return err
		}
		created = true
		return nil
	})
	return created, err
}
