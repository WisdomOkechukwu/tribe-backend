package database

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// deleteBatchSize is how many rows one DELETE removes at most.
const deleteBatchSize = 5000

// DeleteInBatches deletes every row of table matching condition, at most
// 5000 rows per statement, until none are left.
//
// Why not one big DELETE? A DELETE that removes millions of rows runs as one
// huge transaction: it holds its locks for minutes, blocks other writers,
// and fills the undo log. Many small DELETEs each finish in a moment, so
// normal traffic carries on in between. The short pause between batches
// gives replication and other queries room to keep up.
//
// table and condition are written by our own code (never user input);
// values go through the ? placeholders in arguments.
func DeleteInBatches(requestContext context.Context, connection *gorm.DB, table string, condition string, arguments ...any) (int64, error) {
	statement := fmt.Sprintf("DELETE FROM %s WHERE %s LIMIT %d", table, condition, deleteBatchSize)
	var total int64
	for {
		result := connection.WithContext(requestContext).Exec(statement, arguments...)
		if result.Error != nil {
			return total, result.Error
		}
		total += result.RowsAffected
		if result.RowsAffected < deleteBatchSize {
			return total, nil
		}
		select {
		case <-requestContext.Done():
			return total, requestContext.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
