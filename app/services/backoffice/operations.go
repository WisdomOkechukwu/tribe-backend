package backoffice

import (
	"context"
	"database/sql"
	"math/big"
	"net/http"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/tribe-africa/tribe-backend/app/apperrors"
	"github.com/tribe-africa/tribe-backend/app/events"
	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/app/services/adminauth"
	"github.com/tribe-africa/tribe-backend/app/services/shared"
	"github.com/tribe-africa/tribe-backend/database"
	"github.com/tribe-africa/tribe-backend/pkg/ids"
)

var (
	bigTen        = big.NewRat(10, 1)
	bigTwentyFive = big.NewRat(25, 1)
)

// ─── Overview ────────────────────────────────────────────────────────
//
// The dashboard is split into four small reads (stats, series, by
// category, failures) so the admin panel can load and refresh each card on
// its own. Overview still returns all of them together (plus the
// inventory) for older screens.

// OverviewStats is the row of headline numbers on the dashboard.
type OverviewStats struct {
	AsOf           time.Time `json:"as_of"`
	Customers      int64     `json:"customers"`
	Signups7       int64     `json:"signups_7"`
	KycPending     int64     `json:"kyc_pending"`
	OpenTickets    int64     `json:"open_tickets"`
	Transactions24 int64     `json:"transactions_24"`
	Failed24       int64     `json:"failed_24"`
	FailRate24     float64   `json:"fail_rate_24"` // a ratio (0..1), not money
	PendingAll     int64     `json:"pending_all"`
	Volume1Usd     string    `json:"volume_1_usd"` // completed USD volume in the last 24 hours ("" when none)
	Volume7Usd     string    `json:"volume_7_usd"` // completed USD volume in the last 7 days ("" when none)
}

// OverviewDay is one day of the 30-day volume chart.
type OverviewDay struct {
	Day       string `json:"day"`
	VolumeUsd string `json:"volume_usd"`
	Count     int    `json:"count"`
	Failed    int    `json:"failed"`
}

// OverviewCategory is one product's volume over the last 7 days.
type OverviewCategory struct {
	Category  string `json:"category"`
	VolumeUsd string `json:"volume_usd"`
	Count     int    `json:"count"`
}

// Overview returns every dashboard number in one map (stats, series,
// by_category_7, failed_recent and inventory). It is built from the same
// methods as the split /overview/... routes, so both always agree.
// showCustomers adds customer names to the recent failures (customers.view only).
func (service *Service) Overview(requestContext context.Context, showCustomers bool) (map[string]any, error) {
	stats, err := service.OverviewStats(requestContext)
	if err != nil {
		return nil, err
	}
	series, err := service.OverviewSeries(requestContext)
	if err != nil {
		return nil, err
	}
	byCategory, err := service.OverviewByCategory(requestContext)
	if err != nil {
		return nil, err
	}
	failures, err := service.OverviewFailures(requestContext, showCustomers)
	if err != nil {
		return nil, err
	}
	inventory, err := service.Inventory(requestContext)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"as_of": stats.AsOf, "customers": stats.Customers, "signups_7": stats.Signups7, "kyc_pending": stats.KycPending,
		"open_tickets": stats.OpenTickets, "transactions_24": stats.Transactions24, "failed_24": stats.Failed24,
		"fail_rate_24": stats.FailRate24, "pending_all": stats.PendingAll,
		"volume_1_usd": stats.Volume1Usd, "volume_7_usd": stats.Volume7Usd,
		"series": series, "by_category_7": byCategory, "failed_recent": failures, "inventory": inventory,
	}, nil
}

// OverviewStats returns the headline numbers: customers, sign-ups, open
// work queues, and the 24-hour / 7-day transaction figures.
func (service *Service) OverviewStats(requestContext context.Context) (*OverviewStats, error) {
	query := service.database.WithContext(requestContext)
	now := time.Now().UTC()
	dayAgo, weekAgo := now.Add(-24*time.Hour), now.AddDate(0, 0, -7)

	stats := &OverviewStats{AsOf: now}
	counts := []struct {
		target     *int64
		countQuery *gorm.DB
	}{
		{&stats.Customers, query.Model(&models.User{})},
		{&stats.Signups7, query.Model(&models.User{}).Where("created_at > ?", weekAgo)},
		{&stats.KycPending, query.Model(&models.KycSubmission{}).Where("status = ?", models.KycSubmissionPending)},
		{&stats.OpenTickets, query.Model(&models.SupportThread{}).Where("status <> ?", "resolved")},
		{&stats.PendingAll, query.Model(&models.Transaction{}).Where("status = ?", "pending")},
	}
	for _, item := range counts {
		if err := item.countQuery.Count(item.target).Error; err != nil {
			return nil, err
		}
	}

	// The 24-hour and 7-day figures in ONE pass over the last 7 days of
	// transactions (it was four separate queries). Each SUM(CASE ...) only
	// counts the rows its condition picks. The dashboard index on
	// (created_at, status, category, usd_value) holds every column used, so
	// MySQL reads the index alone.
	var recent struct {
		Transactions24 int64
		Failed24       int64
		Volume1        sql.NullString
		Volume7        sql.NullString
	}
	err := query.Raw(`SELECT COALESCE(SUM(CASE WHEN created_at > ? THEN 1 ELSE 0 END), 0) AS transactions24,
	                         COALESCE(SUM(CASE WHEN created_at > ? AND status = 'failed' THEN 1 ELSE 0 END), 0) AS failed24,
	                         SUM(CASE WHEN created_at > ? AND status = 'completed' THEN usd_value END) AS volume1,
	                         SUM(CASE WHEN status = 'completed' THEN usd_value END) AS volume7
	                    FROM transactions WHERE created_at > ?`, dayAgo, dayAgo, dayAgo, weekAgo).Scan(&recent).Error
	if err != nil {
		return nil, err
	}
	stats.Transactions24, stats.Failed24 = recent.Transactions24, recent.Failed24
	stats.Volume1Usd, stats.Volume7Usd = recent.Volume1.String, recent.Volume7.String
	if stats.Transactions24 > 0 {
		stats.FailRate24 = float64(stats.Failed24) / float64(stats.Transactions24)
	}
	return stats, nil
}

// OverviewSeries returns the volume, count and failures per day for the
// last 30 days (days with no transactions are left out). Never nil.
func (service *Service) OverviewSeries(requestContext context.Context) ([]OverviewDay, error) {
	// Computed with GROUP BY; no window functions, so MySQL 5.7 works too.
	series := []OverviewDay{}
	err := service.database.WithContext(requestContext).Raw(`
		SELECT DATE(created_at) AS day, COALESCE(SUM(usd_value), 0) AS volume_usd, COUNT(*) AS count,
		       SUM(CASE WHEN status = 'failed' THEN 1 ELSE 0 END) AS failed
		  FROM transactions WHERE created_at > ? GROUP BY DATE(created_at) ORDER BY day`,
		time.Now().UTC().AddDate(0, 0, -30)).Scan(&series).Error
	if err != nil {
		return nil, err
	}
	if series == nil {
		series = []OverviewDay{}
	}
	return series, nil
}

// OverviewByCategory returns the volume and count per product category
// over the last 7 days. Never nil.
func (service *Service) OverviewByCategory(requestContext context.Context) ([]OverviewCategory, error) {
	byCategory := []OverviewCategory{}
	err := service.database.WithContext(requestContext).Raw(`
		SELECT category, COALESCE(SUM(usd_value), 0) AS volume_usd, COUNT(*) AS count
		  FROM transactions WHERE created_at > ? GROUP BY category ORDER BY category`,
		time.Now().UTC().AddDate(0, 0, -7)).Scan(&byCategory).Error
	if err != nil {
		return nil, err
	}
	if byCategory == nil {
		byCategory = []OverviewCategory{}
	}
	return byCategory, nil
}

// OverviewFailures returns the 8 newest failed transactions. showCustomers
// adds each customer's name and tag (customers.view only). Never nil.
func (service *Service) OverviewFailures(requestContext context.Context, showCustomers bool) ([]TransactionRow, error) {
	query := service.database.WithContext(requestContext)
	var failedRecent []models.Transaction
	if err := query.Where("status = ?", "failed").Order("created_at DESC").Limit(8).Find(&failedRecent).Error; err != nil {
		return nil, err
	}
	return service.withCustomers(query, failedRecent, showCustomers), nil
}

// ─── Support ─────────────────────────────────────────────────────────

// ThreadRow is a support thread with the customer's name.
type ThreadRow struct {
	models.SupportThread
	CustomerName string `json:"customer_name"`
	CustomerTag  string `json:"customer_tag"`
	Preview      string `json:"preview"`
}

// ListThreads returns support threads.
func (service *Service) ListThreads(requestContext context.Context, status string, channel string) ([]ThreadRow, error) {
	query := service.database.WithContext(requestContext)
	threadsQuery := query.Model(&models.SupportThread{})
	if status != "" {
		threadsQuery = threadsQuery.Where("status = ?", status)
	}
	if channel != "" {
		threadsQuery = threadsQuery.Where("channel = ?", channel)
	}
	var threads []models.SupportThread
	if err := threadsQuery.Order("updated_at DESC").Limit(80).Find(&threads).Error; err != nil {
		return nil, err
	}
	if len(threads) == 0 {
		return []ThreadRow{}, nil
	}

	// Three queries in total (threads, customers, last messages) instead of
	// two more per thread.
	userIDs := make([]string, 0, len(threads))
	threadIDs := make([]string, 0, len(threads))
	for _, thread := range threads {
		userIDs = append(userIDs, thread.UserID)
		threadIDs = append(threadIDs, thread.ID)
	}
	var users []models.User
	if err := query.Where("id IN ?", userIDs).Find(&users).Error; err != nil {
		return nil, err
	}
	userByID := make(map[string]models.User, len(users))
	for _, user := range users {
		userByID[user.ID] = user
	}
	// The newest message of each thread. The inner SELECT finds each
	// thread's newest time using the (thread_id, created_at) index.
	var lastMessages []models.SupportMessage
	err := query.Raw(`
		SELECT messages.*
		  FROM support_messages messages
		 WHERE messages.thread_id IN ?
		   AND messages.created_at = (SELECT MAX(newest.created_at)
		                                FROM support_messages newest
		                               WHERE newest.thread_id = messages.thread_id)`, threadIDs).Scan(&lastMessages).Error
	if err != nil {
		return nil, err
	}
	previewByThread := make(map[string]string, len(lastMessages))
	for _, message := range lastMessages {
		previewByThread[message.ThreadID] = truncateText(message.Body, 120)
	}

	rows := make([]ThreadRow, 0, len(threads))
	for _, thread := range threads {
		user := userByID[thread.UserID]
		rows = append(rows, ThreadRow{SupportThread: thread, CustomerName: user.FullName(), CustomerTag: user.Tag, Preview: previewByThread[thread.ID]})
	}
	return rows, nil
}

// CountThreadsByStatus returns how many threads are open, pending and resolved.
func (service *Service) CountThreadsByStatus(requestContext context.Context) (map[string]int64, error) {
	var rows []struct {
		Status string
		Total  int64
	}
	err := service.database.WithContext(requestContext).Model(&models.SupportThread{}).
		Select("status, COUNT(*) AS total").Group("status").Scan(&rows).Error
	counts := map[string]int64{"open": 0, "pending": 0, "resolved": 0}
	for _, row := range rows {
		counts[row.Status] = row.Total
	}
	return counts, err
}

// GetThread returns a thread and its messages, and marks it read.
func (service *Service) GetThread(requestContext context.Context, threadID string) (*ThreadRow, []models.SupportMessage, error) {
	query := service.database.WithContext(requestContext)
	var thread models.SupportThread
	if err := query.Where("id = ?", threadID).Take(&thread).Error; err != nil {
		return nil, nil, apperrors.NotFound("Thread")
	}
	var user models.User
	query.Where("id = ?", thread.UserID).Take(&user)
	var messages []models.SupportMessage
	query.Where("thread_id = ?", threadID).Order("created_at").Find(&messages)
	query.Model(&models.SupportThread{}).Where("id = ?", threadID).Update("unread_for_admin", 0)
	return &ThreadRow{SupportThread: thread, CustomerName: user.FullName(), CustomerTag: user.Tag}, messages, nil
}

// ReplyToThread sends an agent message (open threads become "pending" = waiting on the customer).
func (service *Service) ReplyToThread(requestContext context.Context, admin *adminauth.Admin, threadID string, body string) error {
	if len(body) == 0 || len(body) > 2000 {
		return apperrors.Validation(map[string][]string{"body": {"Write 1 to 2000 characters."}})
	}
	var customerID string // whose thread it is, for their live chat socket
	err := database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		var thread models.SupportThread
		if err := database.LockForUpdate(databaseTransaction, &thread, "id = ?", threadID); err != nil {
			return apperrors.NotFound("Thread")
		}
		customerID = thread.UserID
		now := time.Now().UTC()
		if err := databaseTransaction.Create(&models.SupportMessage{ID: ids.New(), ThreadID: threadID, SenderType: "agent", AuthorName: admin.Name, Body: body, CreatedAt: now}).Error; err != nil {
			return err
		}
		status := thread.Status
		if status == "open" {
			status = "pending"
		}
		if err := databaseTransaction.Model(&models.SupportThread{}).Where("id = ?", threadID).
			Updates(map[string]any{"status": status, "updated_at": now, "version": thread.Version + 1}).Error; err != nil {
			return err
		}
		// the push opens the chat itself (the app's /support/:id screen)
		return shared.Notify(databaseTransaction, thread.UserID, "support", "Support replied", truncateText(body, 140), "/support/"+threadID)
	})
	if err == nil {
		service.adminAuth.Audit(requestContext, admin, "support.reply", threadID, "Replied to customer", nil, nil)
		events.PublishSupportEvent(requestContext, service.redis, "thread.updated", threadID, customerID)
	}
	return err
}

// SetThreadStatus opens, parks or resolves a thread.
func (service *Service) SetThreadStatus(requestContext context.Context, admin *adminauth.Admin, threadID string, status string) error {
	if status != "open" && status != "pending" && status != "resolved" {
		return apperrors.Validation(map[string][]string{"status": {"Status must be open, pending or resolved."}})
	}
	var customerID string // whose thread it is, for their live chat socket
	err := database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		var thread models.SupportThread
		if err := database.LockForUpdate(databaseTransaction, &thread, "id = ?", threadID); err != nil {
			return apperrors.NotFound("Thread")
		}
		customerID = thread.UserID
		now := time.Now().UTC()
		databaseTransaction.Create(&models.SupportMessage{ID: ids.New(), ThreadID: threadID, SenderType: "system", AuthorName: admin.Name, Body: admin.Name + " marked this " + status, CreatedAt: now})
		return databaseTransaction.Model(&models.SupportThread{}).Where("id = ?", threadID).
			Updates(map[string]any{"status": status, "updated_at": now, "version": thread.Version + 1}).Error
	})
	if err == nil {
		service.adminAuth.Audit(requestContext, admin, "support.status", threadID, "Set status to "+status, nil, nil)
		events.PublishSupportEvent(requestContext, service.redis, "thread.status", threadID, customerID)
	}
	return err
}

// ─── Logs ────────────────────────────────────────────────────────────

// ApiRequestFilters narrows the API trail.
type ApiRequestFilters struct {
	CustomerID   string
	Status       string // 2xx | 4xx | 5xx
	Method       string // GET, POST ...
	Path         string // part of the path, e.g. "swaps"
	MinLatencyMs int    // only slow calls
	Page         int
}

// ListApiRequests returns the API trail (bodies removed without api.bodies).
func (service *Service) ListApiRequests(requestContext context.Context, admin *adminauth.Admin, filters ApiRequestFilters) (Page[models.ApiRequestLog], error) {
	page := filters.Page
	query := service.database.WithContext(requestContext).Model(&models.ApiRequestLog{})
	if filters.CustomerID != "" {
		query = query.Where("user_id = ?", filters.CustomerID)
	}
	if filters.Method != "" {
		query = query.Where("method = ?", strings.ToUpper(filters.Method))
	}
	if filters.Path != "" {
		query = query.Where("path LIKE ?", "%"+filters.Path+"%")
	}
	if filters.MinLatencyMs > 0 {
		query = query.Where("latency_ms >= ?", filters.MinLatencyMs)
	}
	switch filters.Status {
	case "2xx":
		query = query.Where("status BETWEEN 200 AND 299")
	case "4xx":
		query = query.Where("status BETWEEN 400 AND 499")
	case "5xx":
		query = query.Where("status >= 500")
	}
	var total int64
	query.Count(&total)
	var rows []models.ApiRequestLog
	if err := query.Order("id DESC").Offset(offset(page)).Limit(PageSize).Find(&rows).Error; err != nil {
		return Page[models.ApiRequestLog]{}, err
	}
	if !admin.Can("api.bodies") {
		for index := range rows {
			rows[index].RequestBody, rows[index].ResponseBody = nil, nil
		}
	}
	return newPage(rows, total, page), nil
}

// ListAuditLogs returns the audit log (optionally filtered by action prefix).
func (service *Service) ListAuditLogs(requestContext context.Context, actionPrefix string, page int) (Page[models.AuditLog], error) {
	query := service.database.WithContext(requestContext).Model(&models.AuditLog{})
	if actionPrefix != "" {
		query = query.Where("action LIKE ?", actionPrefix+"%")
	}
	var total int64
	query.Count(&total)
	var rows []models.AuditLog
	err := query.Order("created_at DESC").Offset(offset(page)).Limit(PageSize).Find(&rows).Error
	return newPage(rows, total, page), err
}

// ListJobRuns returns recent cron runs.
func (service *Service) ListJobRuns(requestContext context.Context) ([]models.ScheduledJobRun, error) {
	runs := []models.ScheduledJobRun{} // never nil, so the JSON is [] and not null
	if err := service.database.WithContext(requestContext).Order("started_at DESC").Limit(100).Find(&runs).Error; err != nil {
		return nil, err
	}
	return runs, nil
}

// RequireFound is a small helper for controllers.
func RequireFound(found bool, thing string) error {
	if !found {
		return apperrors.New(http.StatusNotFound, "NOT_FOUND", thing+" not found.")
	}
	return nil
}

// truncateText shortens text for previews.
func truncateText(text string, maximumLength int) string {
	if len(text) <= maximumLength {
		return text
	}
	return text[:maximumLength] + "…"
}
