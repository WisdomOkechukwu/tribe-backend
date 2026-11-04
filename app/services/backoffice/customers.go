// Package backoffice powers the admin panel: customers, KYC decisions,
// transactions with their journal legs, rates and fees, inventory, the
// ledger, providers, support, logs and jobs.
package backoffice

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"github.com/tribe-africa/tribe-backend/app/apperrors"
	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/app/services/adminauth"
	"github.com/tribe-africa/tribe-backend/app/services/auth"
	"github.com/tribe-africa/tribe-backend/app/services/ledger"
	"github.com/tribe-africa/tribe-backend/app/services/rates"
	"github.com/tribe-africa/tribe-backend/app/services/shared"
	"github.com/tribe-africa/tribe-backend/config"
	"github.com/tribe-africa/tribe-backend/database"
	"github.com/tribe-africa/tribe-backend/pkg/money"
)

// PageSize is how many rows admin lists show per page.
const PageSize = 50

// Service powers the admin panel.
type Service struct {
	database  *gorm.DB
	redis     *redis.Client
	ledger    *ledger.Service
	rates     *rates.Service
	auth      *auth.Service
	adminAuth *adminauth.Service
	settings  config.Config
}

// NewService creates the backoffice service.
func NewService(connection *gorm.DB, redisClient *redis.Client, ledgerService *ledger.Service, ratesService *rates.Service, authService *auth.Service, adminAuthService *adminauth.Service, settings config.Config) *Service {
	return &Service{database: connection, redis: redisClient, ledger: ledgerService, rates: ratesService, auth: authService, adminAuth: adminAuthService, settings: settings}
}

// Page is a page of rows (same shape as the admin's Page<T>).
type Page[Row any] struct {
	Rows     []Row `json:"rows"`
	Total    int64 `json:"total"`
	Page     int   `json:"page"`
	PageSize int   `json:"page_size"`
	Pages    int   `json:"pages"`
}

// newPage builds the page metadata.
func newPage[Row any](rows []Row, total int64, page int) Page[Row] {
	if rows == nil {
		rows = []Row{}
	}
	pages := int((total + PageSize - 1) / PageSize)
	return Page[Row]{Rows: rows, Total: total, Page: page, PageSize: PageSize, Pages: pages}
}

// offset returns the SQL OFFSET for a 1-based page.
func offset(page int) int {
	if page < 1 {
		page = 1
	}
	return (page - 1) * PageSize
}

// CustomerRow is one row in the customers list.
type CustomerRow struct {
	ID           string     `json:"id"`
	FirstName    string     `json:"first_name"`
	LastName     string     `json:"last_name"`
	Tag          string     `json:"tag"`
	Email        string     `json:"email"`
	Phone        string     `json:"phone"`
	Country      string     `json:"country"`
	Status       string     `json:"status"`
	KycTier      int        `json:"kyc_tier"`
	KycStatus    string     `json:"kyc_status"`
	Labels       []string   `json:"labels"`
	CreatedAt    time.Time  `json:"created_at"`
	LastActiveAt *time.Time `json:"last_active_at"`
	Version      int        `json:"version"`
}

// CustomerFilters filter the customers list.
type CustomerFilters struct {
	Search string
	Status string
	Kyc    string
	Label  string
	Page   int
}

// ListCustomers returns customers. Personal data is masked unless the
// admin has customers.pii (masking happens HERE, on the server).
func (service *Service) ListCustomers(requestContext context.Context, admin *adminauth.Admin, filters CustomerFilters) (Page[CustomerRow], error) {
	query := service.database.WithContext(requestContext).Model(&models.User{})
	if search := strings.TrimSpace(filters.Search); search != "" {
		like := "%" + strings.TrimPrefix(search, "@") + "%"
		query = query.Where("email LIKE ? OR tag LIKE ? OR first_name LIKE ? OR last_name LIKE ? OR id = ?", like, like, like, like, search)
	}
	if filters.Status != "" {
		query = query.Where("status = ?", filters.Status)
	}
	if filters.Kyc != "" {
		query = query.Where("kyc_status = ?", filters.Kyc)
	}
	if filters.Label != "" {
		query = query.Where("JSON_CONTAINS(labels, JSON_QUOTE(?))", filters.Label)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return Page[CustomerRow]{}, err
	}
	var users []models.User
	if err := query.Order("created_at DESC").Offset(offset(filters.Page)).Limit(PageSize).Find(&users).Error; err != nil {
		return Page[CustomerRow]{}, err
	}
	rows := make([]CustomerRow, 0, len(users))
	for _, user := range users {
		rows = append(rows, customerRow(user, admin.Can("customers.pii")))
	}
	return newPage(rows, total, filters.Page), nil
}

// customerRow turns a user into a row, masking personal data if needed.
func customerRow(user models.User, canSeePersonalData bool) CustomerRow {
	var labels []string
	json.Unmarshal(user.Labels, &labels)
	if labels == nil {
		labels = []string{}
	}
	phone := ""
	if user.Phone != nil {
		phone = *user.Phone
	}
	email := user.Email
	if !canSeePersonalData {
		email = maskEmail(email)
		phone = maskEnd(phone, 3)
	}
	return CustomerRow{ID: user.ID, FirstName: user.FirstName, LastName: user.LastName, Tag: user.Tag, Email: email, Phone: phone,
		Country: user.Country, Status: user.Status, KycTier: user.KycTier, KycStatus: user.KycStatus, Labels: labels,
		CreatedAt: user.CreatedAt, LastActiveAt: user.LastActiveAt, Version: user.Version}
}

// CustomerDetail is the customer profile page.
type CustomerDetail struct {
	Customer  CustomerRow          `json:"customer"`
	Balances  []map[string]string  `json:"balances"`
	Devices   []models.UserDevice  `json:"devices"`
	Cards     []models.VirtualCard `json:"cards"`
	TxCount   int64                `json:"tx_count"`
	Address   string               `json:"address"`
	BirthDate string               `json:"date_of_birth"`
}

// GetCustomer returns a customer with balances, devices and cards.
func (service *Service) GetCustomer(requestContext context.Context, admin *adminauth.Admin, customerID string) (*CustomerDetail, error) {
	query := service.database.WithContext(requestContext)
	var user models.User
	if err := query.Where("id = ? OR tag = ?", customerID, strings.TrimPrefix(customerID, "@")).Take(&user).Error; err != nil {
		return nil, apperrors.NotFound("Customer")
	}
	detail := &CustomerDetail{Customer: customerRow(user, admin.Can("customers.pii")), Balances: []map[string]string{}}
	decimals := shared.AssetDecimals(query)
	balances, err := service.ledger.BalancesForUser(requestContext, user.ID)
	if err != nil {
		return nil, err
	}
	for _, balance := range balances {
		detail.Balances = append(detail.Balances, map[string]string{
			"account": balance.Account.Code, "account_number": balance.Account.AccountNumber,
			"asset": balance.Account.AssetCode, "balance": balance.Balance.Decimal(decimals[balance.Account.AssetCode]),
		})
	}
	query.Where("user_id = ?", user.ID).Order("updated_at DESC").Find(&detail.Devices)
	query.Where("user_id = ?", user.ID).Order("created_at").Find(&detail.Cards)
	query.Model(&models.Transaction{}).Where("user_id = ?", user.ID).Count(&detail.TxCount)
	if admin.Can("customers.pii") {
		if user.Address != nil {
			detail.Address = *user.Address
		}
		if user.DateOfBirth != nil {
			detail.BirthDate = user.DateOfBirth.Format(time.DateOnly)
		}
	}
	return detail, nil
}

// SetCustomerStatus freezes or unfreezes a customer (lock + version + audit).
// Freezing also freezes their ledger accounts so no money can leave them,
// and clears their cached sessions.
func (service *Service) SetCustomerStatus(requestContext context.Context, admin *adminauth.Admin, customerID string, status string, reason string, version int) (*CustomerRow, error) {
	if status != "active" && status != "frozen" {
		return nil, apperrors.Validation(map[string][]string{"status": {"Status must be active or frozen."}})
	}
	if len(strings.TrimSpace(reason)) < 3 {
		return nil, apperrors.Validation(map[string][]string{"reason": {"Give a reason."}})
	}
	var before, after models.User
	err := database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		if err := database.LockForUpdate(databaseTransaction, &before, "id = ?", customerID); err != nil {
			return apperrors.NotFound("Customer")
		}
		if version != 0 && version != before.Version {
			return apperrors.ErrStaleData
		}
		if before.Status == "closed" {
			return apperrors.New(http.StatusUnprocessableEntity, "CLOSED", "Closed accounts can't be changed.")
		}
		if err := databaseTransaction.Model(&models.User{}).Where("id = ?", customerID).
			Updates(map[string]any{"status": status, "version": before.Version + 1, "updated_at": time.Now().UTC()}).Error; err != nil {
			return err
		}
		accountStatus := "active"
		if status == "frozen" {
			accountStatus = "frozen"
		}
		if err := databaseTransaction.Model(&models.LedgerAccount{}).Where("user_id = ?", customerID).Update("status", accountStatus).Error; err != nil {
			return err
		}
		return databaseTransaction.Where("id = ?", customerID).Take(&after).Error
	})
	if err != nil {
		return nil, err
	}
	service.auth.ForgetUserSessions(requestContext, customerID)
	service.adminAuth.Audit(requestContext, admin, "customer.status", customerID, "Set status to "+status+": "+reason,
		map[string]string{"status": before.Status}, map[string]string{"status": after.Status})
	row := customerRow(after, admin.Can("customers.pii"))
	return &row, nil
}

var validLabel = regexp.MustCompile(`^[a-z0-9-]{2,24}$`)

// SetCustomerLabels replaces a customer's internal labels.
func (service *Service) SetCustomerLabels(requestContext context.Context, admin *adminauth.Admin, customerID string, labels []string) (*CustomerRow, error) {
	if len(labels) > 6 {
		return nil, apperrors.Validation(map[string][]string{"labels": {"At most 6 labels."}})
	}
	for _, label := range labels {
		if !validLabel.MatchString(label) {
			return nil, apperrors.Validation(map[string][]string{"labels": {"Labels use 2-24 lowercase letters, numbers or dashes."}})
		}
	}
	var before, after models.User
	err := database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		if err := database.LockForUpdate(databaseTransaction, &before, "id = ?", customerID); err != nil {
			return apperrors.NotFound("Customer")
		}
		if err := databaseTransaction.Model(&models.User{}).Where("id = ?", customerID).
			Updates(map[string]any{"labels": shared.JSON(labels), "version": before.Version + 1, "updated_at": time.Now().UTC()}).Error; err != nil {
			return err
		}
		return databaseTransaction.Where("id = ?", customerID).Take(&after).Error
	})
	if err != nil {
		return nil, err
	}
	service.adminAuth.Audit(requestContext, admin, "customer.labels", customerID, "Changed labels", json.RawMessage(before.Labels), labels)
	row := customerRow(after, admin.Can("customers.pii"))
	return &row, nil
}

// ─── KYC ─────────────────────────────────────────────────────────────

// SubmissionCheck is one check inside a verification request, as admins see
// it (never the encrypted value).
type SubmissionCheck struct {
	ID             string  `json:"id"`
	CheckType      string  `json:"check_type"`
	Status         string  `json:"status"`
	Provider       string  `json:"provider"`
	Reference      string  `json:"reference"`
	DocumentType   *string `json:"document_type"`
	DocumentMasked *string `json:"document_masked"`
	MatchScore     *int    `json:"match_score"`
}

// VerificationRow is one verification REQUEST in the review queue: the tier,
// its checks, which attempt this is and why the last one was rejected.
type VerificationRow struct {
	models.KycSubmission
	Attempt        int               `json:"attempt"`
	PreviousReason *string           `json:"previous_reason"`
	Checks         []SubmissionCheck `json:"checks"`
	CustomerName   string            `json:"customer_name"`
	CustomerTag    string            `json:"customer_tag"`
	CustomerEmail  string            `json:"customer_email"`
	CurrentTier    int               `json:"current_tier"`
}

// VerificationStatuses are the status values GET /verifications?status=
// filters by (see KycSubmission* in app/models).
var VerificationStatuses = []string{models.KycSubmissionPending, models.KycSubmissionApproved, models.KycSubmissionRejected}

// CountVerificationsByStatus returns how many verification requests have
// each status, plus "all" (the total), for the tabs above the KYC list.
// Every status is present, with 0 when there are none. One GROUP BY query
// on the (status, submitted_at) index.
func (service *Service) CountVerificationsByStatus(requestContext context.Context) (map[string]int64, error) {
	var rows []struct {
		Status string
		Total  int64
	}
	err := service.database.WithContext(requestContext).Model(&models.KycSubmission{}).
		Select("status, COUNT(*) AS total").Group("status").Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	counts := map[string]int64{"all": 0}
	for _, status := range VerificationStatuses {
		counts[status] = 0
	}
	for _, row := range rows {
		counts[row.Status] = row.Total
		counts["all"] += row.Total
	}
	return counts, nil
}

// ListVerifications returns verification requests: the review queue (status,
// oldest first while pending so nobody waits longest) or one customer's
// history (customerID, newest first). Users, earlier attempts and checks are
// each loaded in ONE query for the whole page.
func (service *Service) ListVerifications(requestContext context.Context, admin *adminauth.Admin, status string, customerID string, page int) (Page[VerificationRow], error) {
	query := service.database.WithContext(requestContext)
	filtered := query.Model(&models.KycSubmission{})
	if status != "" {
		filtered = filtered.Where("status = ?", status)
	}
	if customerID != "" {
		filtered = filtered.Where("user_id = ?", customerID)
	}
	var total int64
	filtered.Count(&total)
	order := "submitted_at DESC, id DESC"
	if status == models.KycSubmissionPending && customerID == "" {
		order = "submitted_at ASC, id ASC"
	}
	var submissions []models.KycSubmission
	if err := filtered.Order(order).Offset(offset(page)).Limit(PageSize).Find(&submissions).Error; err != nil {
		return Page[VerificationRow]{}, err
	}
	if len(submissions) == 0 {
		return newPage([]VerificationRow{}, total, page), nil
	}

	userIDs := make([]string, 0, len(submissions))
	submissionIDs := make([]string, 0, len(submissions))
	for _, submission := range submissions {
		userIDs = append(userIDs, submission.UserID)
		submissionIDs = append(submissionIDs, submission.ID)
	}
	var users []models.User
	if err := query.Where("id IN ?", userIDs).Find(&users).Error; err != nil {
		return Page[VerificationRow]{}, err
	}
	usersByID := map[string]models.User{}
	for _, user := range users {
		usersByID[user.ID] = user
	}
	// every request these customers ever made, oldest first, to number the attempts
	var history []models.KycSubmission
	if err := query.Where("user_id IN ?", userIDs).Order("submitted_at ASC, id ASC").Find(&history).Error; err != nil {
		return Page[VerificationRow]{}, err
	}
	var checks []models.KycVerification
	if err := query.Where("submission_id IN ?", submissionIDs).Order("check_type").Find(&checks).Error; err != nil {
		return Page[VerificationRow]{}, err
	}
	checksBySubmission := map[string][]SubmissionCheck{}
	for _, check := range checks {
		checksBySubmission[*check.SubmissionID] = append(checksBySubmission[*check.SubmissionID], SubmissionCheck{
			ID: check.ID, CheckType: check.CheckType, Status: check.Status, Provider: check.Provider, Reference: check.Reference,
			DocumentType: check.DocumentType, DocumentMasked: check.DocumentMasked, MatchScore: check.MatchScore,
		})
	}

	rows := make([]VerificationRow, 0, len(submissions))
	for _, submission := range submissions {
		user := usersByID[submission.UserID]
		email := user.Email
		if !admin.Can("customers.pii") {
			email = maskEmail(email)
		}
		row := VerificationRow{
			KycSubmission: submission, Checks: checksBySubmission[submission.ID],
			CustomerName: user.FullName(), CustomerTag: user.Tag, CustomerEmail: email, CurrentTier: user.KycTier,
		}
		if row.Checks == nil {
			row.Checks = []SubmissionCheck{}
		}
		// attempt = this request's place among the customer's requests for the same tier
		var previous *models.KycSubmission
		for index := range history {
			earlier := &history[index]
			if earlier.UserID != submission.UserID || earlier.Tier != submission.Tier {
				continue
			}
			row.Attempt++
			if earlier.ID == submission.ID {
				break
			}
			previous = earlier
		}
		if previous != nil && previous.Status == models.KycSubmissionRejected && previous.Reason != "" {
			reason := previous.Reason
			row.PreviousReason = &reason
		}
		rows = append(rows, row)
	}
	return newPage(rows, total, page), nil
}

// DecideSubmission approves or rejects a whole verification request (every
// check in it). Approving moves the customer up to that tier (never down);
// rejecting needs a reason, which the customer sees. Locks are taken user
// first, then the request, the same order SubmitKyc uses, so the two can't
// deadlock. The customer gets an inbox message (and a push via the outbox).
func (service *Service) DecideSubmission(requestContext context.Context, admin *adminauth.Admin, submissionID string, decision string, reason string) error {
	if decision != "approve" && decision != "reject" {
		return apperrors.Validation(map[string][]string{"decision": {"Decision must be approve or reject."}})
	}
	reason = strings.TrimSpace(reason)
	if decision == "reject" && len([]rune(reason)) < 3 {
		return apperrors.Validation(map[string][]string{"reason": {"Say why you're rejecting it (3+ characters). The customer sees this."}})
	}
	var preview models.KycSubmission
	if err := service.database.WithContext(requestContext).Where("id = ?", submissionID).Take(&preview).Error; err != nil {
		return apperrors.NotFound("Verification")
	}
	var submission models.KycSubmission
	err := database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		var user models.User
		if err := database.LockForUpdate(databaseTransaction, &user, "id = ?", preview.UserID); err != nil {
			return err
		}
		if err := database.LockForUpdate(databaseTransaction, &submission, "id = ?", submissionID); err != nil {
			return apperrors.NotFound("Verification")
		}
		if submission.Status != models.KycSubmissionPending {
			return apperrors.New(http.StatusConflict, "ALREADY_DECIDED", "This request was already "+submission.Status+".")
		}
		now := time.Now().UTC()
		submissionStatus, checkStatus := models.KycSubmissionApproved, "approved"
		if decision == "reject" {
			submissionStatus, checkStatus = models.KycSubmissionRejected, "failed"
		}
		if err := databaseTransaction.Model(&models.KycVerification{}).Where("submission_id = ?", submission.ID).Updates(map[string]any{
			"status": checkStatus, "decided_at": now, "decided_by": admin.Name, "reason": reason, "version": gorm.Expr("version + 1"),
		}).Error; err != nil {
			return err
		}
		// open_key back to NULL releases the "one open request" lock for this customer
		if err := databaseTransaction.Model(&models.KycSubmission{}).Where("id = ?", submission.ID).Updates(map[string]any{
			"status": submissionStatus, "reason": reason, "decided_at": now, "decided_by": admin.Name,
			"open_key": nil, "version": submission.Version + 1,
		}).Error; err != nil {
			return err
		}
		userUpdates := map[string]any{"updated_at": now, "version": user.Version + 1, "kyc_status": "failed"}
		title := "Tier " + itoa(submission.Tier) + " needs attention"
		body := reason
		if !strings.HasSuffix(body, ".") {
			body += "."
		}
		body += " Open Verification to try again."
		if decision == "approve" {
			userUpdates["kyc_status"], userUpdates["kyc_tier"] = "approved", max(user.KycTier, submission.Tier)
			title = "Tier " + itoa(submission.Tier) + " approved"
			body = "Your new daily limit is " + shared.KycDailyLimits[max(user.KycTier, submission.Tier)] + "."
		}
		if err := databaseTransaction.Model(&models.User{}).Where("id = ?", user.ID).Updates(userUpdates).Error; err != nil {
			return err
		}
		return shared.Notify(databaseTransaction, user.ID, "security", title, body, "/verify")
	})
	if err == nil {
		service.adminAuth.Audit(requestContext, admin, "kyc.decide", submissionID, decision+" tier "+itoa(submission.Tier)+" request", nil, map[string]string{"decision": decision, "reason": reason})
	}
	return err
}

// maskEmail turns "ada@example.com" into "a••@example.com".
func maskEmail(email string) string {
	name, domain, found := strings.Cut(email, "@")
	if !found || len(name) == 0 {
		return "•••"
	}
	return name[:1] + "••@" + domain
}

// maskEnd keeps only the last N characters.
func maskEnd(text string, visible int) string {
	if len(text) <= visible {
		return text
	}
	return strings.Repeat("•", len(text)-visible) + text[len(text)-visible:]
}

// itoa turns a number into text.
func itoa(number int) string {
	return money.FromUnits(int64(number)).UnitsString()
}
