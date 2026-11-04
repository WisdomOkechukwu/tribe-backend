package database

import (
	"context"
	"database/sql"
	"errors"
	"math/rand/v2"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// MySQL error numbers we care about.
const (
	mysqlErrorDuplicateEntry   = 1062 // a UNIQUE index already has this value
	mysqlErrorLockWaitTimeout  = 1205 // we waited too long for someone else's row lock
	mysqlErrorDeadlockDetected = 1213 // two transactions were waiting for each other
)

// maximumAttempts is how many times WithTransaction tries before giving up.
const maximumAttempts = 3

// WithTransaction runs the work inside ONE database transaction.
//
// Think of a transaction as "all or nothing": if the work returns an error,
// every change it made is undone (rolled back). If it returns nil, every
// change is saved together (committed).
//
// It also retries automatically when MySQL reports a deadlock or a lock
// timeout. Those happen when two requests try to change the same rows at the
// same moment; trying again a few milliseconds later almost always works.
//
// We use READ COMMITTED isolation: each SELECT sees the latest committed
// data, and rows we lock with FOR UPDATE stay locked until we finish.
func WithTransaction(requestContext context.Context, connection *gorm.DB, work func(databaseTransaction *gorm.DB) error) error {
	var lastError error
	for attempt := 1; attempt <= maximumAttempts; attempt++ {
		lastError = connection.WithContext(requestContext).Transaction(work, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
		if lastError == nil || !IsRetryable(lastError) {
			return lastError
		}
		// Wait a little (with some randomness so two retries don't collide again).
		waitTime := time.Duration(attempt*20+rand.IntN(30)) * time.Millisecond
		select {
		case <-time.After(waitTime):
		case <-requestContext.Done():
			return requestContext.Err()
		}
	}
	return lastError
}

// LockForUpdate loads a row AND locks it until the transaction ends
// (SELECT ... FOR UPDATE). Anyone else who tries to lock the same row waits
// for us to finish. This is how we stop two requests from changing the same
// thing at the same time (a "race condition").
//
// Example:
//
//	var card models.VirtualCard
//	err := database.LockForUpdate(databaseTransaction, &card, "id = ? AND user_id = ?", cardID, userID)
func LockForUpdate(databaseTransaction *gorm.DB, destination any, condition string, arguments ...any) error {
	return databaseTransaction.
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where(condition, arguments...).
		Take(destination).Error
}

// LockForShare loads a row with a SHARED lock (LOCK IN SHARE MODE). Many
// readers can hold a shared lock at once, but nobody can change the row
// until they all finish. We use it to make sure a device is still active
// while we move money, without blocking other readers.
// (We write "LOCK IN SHARE MODE" because MySQL 5.7 does not know "FOR SHARE".)
func LockForShare(databaseTransaction *gorm.DB, destination any, condition string, arguments ...any) error {
	return databaseTransaction.
		Clauses(lockInShareMode{}).
		Where(condition, arguments...).
		Take(destination).Error
}

// lockInShareMode is a tiny GORM clause that ends a SELECT with
// "LOCK IN SHARE MODE" (GORM's built-in one writes "FOR SHARE", which
// MySQL 5.7 does not understand). It takes the place of the "FOR" clause.
type lockInShareMode struct{}

// Name tells GORM which slot of the SELECT this clause fills.
func (lockInShareMode) Name() string { return "FOR" }

// Build writes the SQL.
func (lockInShareMode) Build(builder clause.Builder) { builder.WriteString("LOCK IN SHARE MODE") }

// MergeClause replaces any earlier locking clause with this one.
func (lockInShareMode) MergeClause(mergeClause *clause.Clause) {
	mergeClause.Name = ""
	mergeClause.Expression = lockInShareMode{}
}

// IsDuplicateEntry is true when an INSERT failed because a UNIQUE index
// already has that value. We rely on this to make things idempotent.
func IsDuplicateEntry(err error) bool {
	return mysqlErrorNumber(err) == mysqlErrorDuplicateEntry
}

// IsRetryable is true for deadlocks and lock wait timeouts.
func IsRetryable(err error) bool {
	number := mysqlErrorNumber(err)
	return number == mysqlErrorDeadlockDetected || number == mysqlErrorLockWaitTimeout
}

// IsNotFound is true when a query found no row.
func IsNotFound(err error) bool {
	return errors.Is(err, gorm.ErrRecordNotFound)
}

// mysqlErrorNumber pulls the MySQL error number out of an error (0 if none).
func mysqlErrorNumber(err error) uint16 {
	var mysqlError *mysqldriver.MySQLError
	if errors.As(err, &mysqlError) {
		return mysqlError.Number
	}
	return 0
}
