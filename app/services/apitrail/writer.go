// Package apitrail saves the admin "API trail" (api_request_logs) in the
// background, in batches, with a hard limit on memory.
//
// Before, every request started its own goroutine that inserted one row.
// Under load that meant thousands of goroutines all fighting the money
// requests for the same database connections. Now:
//
//   - the request puts its row in a queue (a buffered channel) and moves on;
//   - ONE background goroutine takes up to 100 rows at a time and saves them
//     with a single multi-row INSERT;
//   - the queue has a fixed size. If the database falls behind and the queue
//     is full, new rows are DROPPED (and counted in the log) instead of
//     piling up in memory until the server runs out.
//
// The API trail is an operational log for support. It is NOT the audit
// trail: audit_logs are written by the services inside their own
// transactions and are never dropped or batched.
package apitrail

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"gorm.io/gorm"

	"github.com/tribe-africa/tribe-backend/app/models"
)

// queueCapacity is the most rows waiting in memory. Each row is at most a
// few KB (bodies are cut to 8 KB), so this caps the queue at about 30 MB.
const queueCapacity = 2000

// batchSize is the most rows saved in one INSERT.
const batchSize = 100

// flushEvery is the longest a row waits before a smaller batch is saved.
const flushEvery = time.Second

// Writer queues API trail rows and saves them in batches.
type Writer struct {
	database *gorm.DB
	queue    chan models.ApiRequestLog
	dropped  atomic.Int64 // rows thrown away because the queue was full
	start    sync.Once
	done     chan struct{}

	// closing guards the queue: Add holds a read lock while it sends, and
	// Close takes the write lock before closing the queue, so a late Add
	// can never send on a closed channel (which would crash the program).
	closing sync.RWMutex
	closed  bool
}

// NewWriter creates a writer. Its background goroutine starts on the first
// row, so programs that never log requests (worker, scheduler) pay nothing.
func NewWriter(database *gorm.DB) *Writer {
	return &Writer{database: database, queue: make(chan models.ApiRequestLog, queueCapacity), done: make(chan struct{})}
}

// Add queues one row without waiting. If the queue is full the row is
// dropped, so a slow database can never make requests wait or memory grow.
func (writer *Writer) Add(row models.ApiRequestLog) {
	writer.closing.RLock()
	defer writer.closing.RUnlock()
	if writer.closed {
		return // the program is stopping
	}
	writer.start.Do(func() { go writer.run() })
	select {
	case writer.queue <- row:
	default:
		writer.dropped.Add(1)
	}
}

// Close saves whatever is still queued and stops the background goroutine.
// Call it once when the program stops.
func (writer *Writer) Close() {
	writer.closing.Lock()
	if writer.closed {
		writer.closing.Unlock()
		return
	}
	writer.closed = true
	writer.start.Do(func() { close(writer.done) }) // never started: nothing to wait for
	close(writer.queue)
	writer.closing.Unlock()
	<-writer.done
}

// run is the background goroutine: collect rows, save them in batches.
func (writer *Writer) run() {
	defer close(writer.done)
	ticker := time.NewTicker(flushEvery)
	defer ticker.Stop()
	batch := make([]models.ApiRequestLog, 0, batchSize)
	for {
		select {
		case row, open := <-writer.queue:
			if !open { // Close was called: save the rest and finish
				writer.save(batch)
				return
			}
			batch = append(batch, row)
			if len(batch) >= batchSize {
				writer.save(batch)
				batch = batch[:0]
			}
		case <-ticker.C:
			writer.save(batch)
			batch = batch[:0]
			if dropped := writer.dropped.Swap(0); dropped > 0 {
				slog.Warn("API trail queue was full; rows were not saved", "dropped", dropped)
			}
		}
	}
}

// save writes one batch with a single INSERT. A failure is logged and the
// batch is skipped: the API trail must never block or break real requests.
func (writer *Writer) save(batch []models.ApiRequestLog) {
	if len(batch) == 0 {
		return
	}
	saveContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := writer.database.WithContext(saveContext).Create(&batch).Error; err != nil {
		slog.Warn("could not save API trail rows", "rows", len(batch), "error", err)
	}
}
