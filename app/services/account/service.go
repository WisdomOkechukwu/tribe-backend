// Package account handles the user's own information: profile, @tag,
// devices, phone verification, KYC, inbox, support tickets and the
// transaction history.
//
// Every CHANGE here uses the same safe pattern:
//
//	database.WithTransaction(func(databaseTransaction) {
//	    database.LockForUpdate(...)   // 1. lock the row
//	    check the rules / version      // 2. decide using fresh data
//	    update and bump version        // 3. write
//	})                                 // 4. commit (or roll back on any error)
package account

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/tribe-africa/tribe-backend/app/apperrors"
	"github.com/tribe-africa/tribe-backend/app/events"
	"github.com/tribe-africa/tribe-backend/app/jobs"
	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/app/services/security"
	"github.com/tribe-africa/tribe-backend/app/services/shared"
	"github.com/tribe-africa/tribe-backend/config"
	"github.com/tribe-africa/tribe-backend/database"
	"github.com/tribe-africa/tribe-backend/pkg/ids"
)

// tagCooldown is how long a user must wait between @tag changes.
const tagCooldown = 30 * 24 * time.Hour

// Service handles the user's account.
type Service struct {
	database   *gorm.DB
	redis      *redis.Client
	toolkit    *security.Toolkit
	dispatcher *jobs.Dispatcher
	settings   config.Config
}

// NewService creates the account service.
func NewService(connection *gorm.DB, redisClient *redis.Client, toolkit *security.Toolkit, dispatcher *jobs.Dispatcher, settings config.Config) *Service {
	return &Service{database: connection, redis: redisClient, toolkit: toolkit, dispatcher: dispatcher, settings: settings}
}

// Profile loads the user.
func (service *Service) Profile(requestContext context.Context, userID string) (*models.User, error) {
	var user models.User
	if err := service.database.WithContext(requestContext).Where("id = ?", userID).Take(&user).Error; err != nil {
		return nil, apperrors.NotFound("User")
	}
	return &user, nil
}

// ProfileChanges are the fields the profile screen can change.
type ProfileChanges struct {
	FirstName         *string
	LastName          *string
	Address           *string
	DateOfBirth       *string
	World             *string
	BiometricsEnabled *bool
	TwoFactorEnabled  *bool
}

// UpdateProfile changes profile fields (lock, change, bump version).
func (service *Service) UpdateProfile(requestContext context.Context, userID string, changes ProfileChanges) (*models.User, error) {
	updates := map[string]any{}
	if changes.FirstName != nil && strings.TrimSpace(*changes.FirstName) != "" {
		updates["first_name"] = strings.TrimSpace(*changes.FirstName)
	}
	if changes.LastName != nil && strings.TrimSpace(*changes.LastName) != "" {
		updates["last_name"] = strings.TrimSpace(*changes.LastName)
	}
	if changes.Address != nil {
		updates["address"] = strings.TrimSpace(*changes.Address)
	}
	if changes.DateOfBirth != nil {
		birthDate, err := time.Parse(time.DateOnly, *changes.DateOfBirth)
		if err != nil {
			return nil, apperrors.Validation(map[string][]string{"date_of_birth": {"Use the format YYYY-MM-DD."}})
		}
		updates["date_of_birth"] = birthDate
	}
	if changes.World != nil {
		updates["world"] = *changes.World
	}
	if changes.BiometricsEnabled != nil {
		updates["biometrics_enabled"] = *changes.BiometricsEnabled
	}
	if changes.TwoFactorEnabled != nil {
		updates["two_factor_enabled"] = *changes.TwoFactorEnabled
	}
	return service.lockAndUpdate(requestContext, userID, func(user *models.User) error { return nil }, updates)
}

var validTag = regexp.MustCompile(`^[a-z0-9_.]{3,32}$`)

// ChangeTag changes the @tag (at most once every 30 days).
func (service *Service) ChangeTag(requestContext context.Context, userID string, newTag string) (*models.User, error) {
	newTag = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(newTag), "@"))
	if !validTag.MatchString(newTag) {
		return nil, apperrors.Validation(map[string][]string{"tag": {"Use 3-32 letters, numbers, dots or underscores."}})
	}
	user, err := service.lockAndUpdate(requestContext, userID, func(user *models.User) error {
		if user.TagChangedAt != nil && time.Since(*user.TagChangedAt) < tagCooldown {
			return apperrors.New(http.StatusUnprocessableEntity, "TAG_COOLDOWN", "You can change your @tag once every 30 days.")
		}
		return nil
	}, map[string]any{"tag": newTag, "tag_changed_at": time.Now().UTC()})
	if err != nil && database.IsDuplicateEntry(err) {
		return nil, apperrors.New(http.StatusConflict, "TAG_TAKEN", "That @tag is taken.")
	}
	return user, err
}

// CompleteOnboarding records the "world" the user picked.
func (service *Service) CompleteOnboarding(requestContext context.Context, userID string, world string) (*models.User, error) {
	return service.lockAndUpdate(requestContext, userID, func(user *models.User) error { return nil },
		map[string]any{"world": world, "onboarding_completed_at": time.Now().UTC()})
}

// lockAndUpdate is the shared lock -> check -> update -> bump version helper.
func (service *Service) lockAndUpdate(requestContext context.Context, userID string, check func(user *models.User) error, updates map[string]any) (*models.User, error) {
	var user models.User
	err := database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		if err := database.LockForUpdate(databaseTransaction, &user, "id = ?", userID); err != nil {
			return apperrors.NotFound("User")
		}
		if err := check(&user); err != nil {
			return err
		}
		if len(updates) == 0 {
			return nil
		}
		updates["updated_at"] = time.Now().UTC()
		updates["version"] = user.Version + 1
		if err := databaseTransaction.Model(&models.User{}).Where("id = ?", userID).Updates(updates).Error; err != nil {
			return err
		}
		return databaseTransaction.Where("id = ?", userID).Take(&user).Error
	})
	return &user, err
}

// ─── Phone verification ──────────────────────────────────────────────

// Phone code limits: no wait between codes (waiting doesn't help anyone), but at most 10 an
// hour, because every code is a paid SMS.
const (
	phoneCodesPerHour  = 10
	phoneCodeAttempts  = 5
	phoneDigitsMinimum = 8 // digits in a phone number
)

// retryAfter builds a 429 that tells the app how many seconds to wait
// (errors.retry_after[0]), so its countdown matches the server.
func retryAfter(code string, message string, wait time.Duration) error {
	seconds := int(wait.Round(time.Second) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	return &apperrors.AppError{
		Status: http.StatusTooManyRequests, Code: code, Message: message,
		FieldErrors: map[string][]string{"retry_after": {strconv.Itoa(seconds)}},
	}
}

// SendPhoneCode texts a 6-digit code. The phone number is NOT saved on the
// user until the code is confirmed. In one transaction (user row locked, so
// two taps can't both send) it refuses more than 10 codes in an hour
// (TOO_MANY_CODES) and makes every earlier unused code stop working, so only
// the newest code is valid. There is no wait between codes.
// Returns the code for local development only.
func (service *Service) SendPhoneCode(requestContext context.Context, userID string, phone string) (string, error) {
	phone = strings.TrimSpace(phone)
	if len(regexp.MustCompile(`\d`).FindAllString(phone, -1)) < phoneDigitsMinimum {
		return "", apperrors.Validation(map[string][]string{"phone": {"Enter a valid phone number."}})
	}
	code := ids.RandomDigits(6)
	err := database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		var user models.User
		if err := database.LockForUpdate(databaseTransaction, &user, "id = ?", userID); err != nil {
			return apperrors.NotFound("User")
		}
		now := time.Now().UTC()
		var recent []models.PhoneVerification
		if err := databaseTransaction.Where("user_id = ? AND created_at > ?", userID, now.Add(-time.Hour)).Order("created_at ASC").Find(&recent).Error; err != nil {
			return err
		}
		if len(recent) >= phoneCodesPerHour {
			wait := recent[len(recent)-phoneCodesPerHour].CreatedAt.Add(time.Hour).Sub(now)
			minutes := int(wait.Round(time.Minute) / time.Minute)
			if minutes < 1 {
				minutes = 1
			}
			return retryAfter("TOO_MANY_CODES", "Too many codes. Try again in "+strconv.Itoa(minutes)+" minutes.", wait)
		}
		// earlier codes stop working the moment a new one is sent
		if err := databaseTransaction.Model(&models.PhoneVerification{}).
			Where("user_id = ? AND verified_at IS NULL AND expires_at > ?", userID, now).
			Update("expires_at", now).Error; err != nil {
			return err
		}
		return databaseTransaction.Create(&models.PhoneVerification{
			ID: ids.New(), UserID: userID, Phone: phone, CodeHash: service.toolkit.HashCode(code),
			ExpiresAt: now.Add(service.settings.Auth.CodeLifetime), CreatedAt: now,
		}).Error
	})
	if err != nil {
		return "", err
	}
	// No SMS provider yet: the email job logs it (see app/jobs/handlers).
	service.dispatcher.SendEmailCode(requestContext, jobs.SendEmailCodePayload{Email: "sms:" + phone, Code: code, Purpose: "phone"})
	return code, nil
}

// VerifyPhone checks the SMS code and only then saves the number. It all
// happens in one transaction with the code row locked. A wrong code still
// counts as an attempt (the attempt is committed before WRONG_CODE is
// returned), and after 5 wrong tries the code is dead (TOO_MANY_ATTEMPTS).
func (service *Service) VerifyPhone(requestContext context.Context, userID string, code string) (*models.User, error) {
	var outcome error
	err := database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		outcome = nil
		var user models.User
		if err := database.LockForUpdate(databaseTransaction, &user, "id = ?", userID); err != nil {
			return apperrors.NotFound("User")
		}
		now := time.Now().UTC()
		var verification models.PhoneVerification
		err := databaseTransaction.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("user_id = ? AND verified_at IS NULL AND expires_at > ?", userID, now).
			Order("created_at DESC").Take(&verification).Error
		if err != nil {
			return apperrors.New(http.StatusUnprocessableEntity, "CODE_EXPIRED", "This code has expired. Ask for a new one.")
		}
		if verification.Attempts >= phoneCodeAttempts {
			return apperrors.New(http.StatusTooManyRequests, "TOO_MANY_ATTEMPTS", "Too many wrong codes. Ask for a new one.")
		}
		if err := databaseTransaction.Model(&models.PhoneVerification{}).Where("id = ?", verification.ID).Update("attempts", verification.Attempts+1).Error; err != nil {
			return err
		}
		if !service.toolkit.CodesMatch(verification.CodeHash, code) {
			// commit the attempt, then report the wrong code
			outcome = apperrors.New(http.StatusUnprocessableEntity, "WRONG_CODE", "That code is not correct.")
			return nil
		}
		if err := databaseTransaction.Model(&models.PhoneVerification{}).Where("id = ?", verification.ID).Update("verified_at", now).Error; err != nil {
			return err
		}
		return databaseTransaction.Model(&models.User{}).Where("id = ?", userID).Updates(map[string]any{
			"phone": verification.Phone, "phone_verified_at": now, "updated_at": now, "version": user.Version + 1,
		}).Error
	})
	if err != nil {
		return nil, err
	}
	if outcome != nil {
		return nil, outcome
	}
	return service.Profile(requestContext, userID)
}

// ─── Devices ─────────────────────────────────────────────────────────

// Devices lists every phone the user has used (active one first).
func (service *Service) Devices(requestContext context.Context, userID string) ([]models.UserDevice, error) {
	var devices []models.UserDevice
	err := service.database.WithContext(requestContext).Where("user_id = ?", userID).
		Order("CASE WHEN status = 'active' THEN 0 ELSE 1 END, updated_at DESC").Find(&devices).Error
	return devices, err
}

// SavePushToken stores an Expo push token for notifications.
func (service *Service) SavePushToken(requestContext context.Context, userID string, token string, platform string) error {
	err := service.database.WithContext(requestContext).Create(&models.PushToken{ID: ids.New(), UserID: userID, Token: token, Platform: platform, CreatedAt: time.Now().UTC()}).Error
	if database.IsDuplicateEntry(err) {
		return service.database.WithContext(requestContext).Model(&models.PushToken{}).Where("token = ?", token).Update("user_id", userID).Error
	}
	return err
}

// ─── KYC (simulated) ─────────────────────────────────────────────────
//
// A customer sends ONE request (models.KycSubmission) per tier, holding that
// tier's checks. While a request is open nothing else can be sent: SubmitKyc
// refuses with VERIFICATION_IN_REVIEW, and a unique index on
// kyc_submissions.open_key backs that up if two requests race.

// The five states of a tier, the same words the app and the admin panel use.
const (
	KycStateVerified       = "verified"
	KycStateInReview       = "in_review"
	KycStateNeedsAttention = "needs_attention"
	KycStateAvailable      = "available"
	KycStateLocked         = "locked"
)

// KycTierState is where one tier stands for the customer.
type KycTierState struct {
	Tier        int        `json:"tier"`
	State       string     `json:"state"`
	SubmittedAt *time.Time `json:"submitted_at"`
	DecidedAt   *time.Time `json:"decided_at"`
	Reason      *string    `json:"reason"` // the rejection reason (needs_attention)
}

// KycOverview is everything the verification screens need (GET /kyc).
type KycOverview struct {
	User   *models.User
	Open   *models.KycSubmission // the request in review, or nil
	Tiers  []KycTierState        // always tiers 1, 2 and 3
	Checks []models.KycVerification
}

// KycStatus returns the user's tier, the open request, each tier's state and every check.
func (service *Service) KycStatus(requestContext context.Context, userID string) (*KycOverview, error) {
	user, err := service.Profile(requestContext, userID)
	if err != nil {
		return nil, err
	}
	query := service.database.WithContext(requestContext)
	var submissions []models.KycSubmission
	if err := query.Where("user_id = ?", userID).Order("submitted_at DESC, id DESC").Find(&submissions).Error; err != nil {
		return nil, err
	}
	var checks []models.KycVerification
	if err := query.Where("user_id = ?", userID).Order("submitted_at DESC").Find(&checks).Error; err != nil {
		return nil, err
	}
	overview := &KycOverview{User: user, Checks: checks, Tiers: kycTierStates(user, submissions)}
	for index := range submissions {
		if submissions[index].Status == models.KycSubmissionPending {
			overview.Open = &submissions[index]
			break
		}
	}
	return overview, nil
}

// kycTierStates works out the state of tiers 1-3 from the user and their
// requests (newest first):
//
//	verified         kyc_tier >= tier
//	in_review        this tier has the open request
//	locked           another tier is in review, or an earlier tier is not done
//	needs_attention  the next tier, and its latest request was rejected
//	available        the next tier, free to start
func kycTierStates(user *models.User, submissions []models.KycSubmission) []KycTierState {
	var open *models.KycSubmission
	for index := range submissions {
		if submissions[index].Status == models.KycSubmissionPending {
			open = &submissions[index]
			break
		}
	}
	states := make([]KycTierState, 0, 3)
	for tier := 1; tier <= 3; tier++ {
		state := KycTierState{Tier: tier}
		var latest *models.KycSubmission
		for index := range submissions {
			if submissions[index].Tier == tier {
				latest = &submissions[index]
				break
			}
		}
		if latest != nil {
			submittedAt := latest.SubmittedAt
			state.SubmittedAt, state.DecidedAt = &submittedAt, latest.DecidedAt
		}
		switch {
		case user.KycTier >= tier:
			state.State = KycStateVerified
		case open != nil && open.Tier == tier:
			state.State = KycStateInReview
		case open != nil || tier > user.KycTier+1:
			state.State = KycStateLocked
		case latest != nil && latest.Status == models.KycSubmissionRejected:
			state.State = KycStateNeedsAttention
			reason := latest.Reason
			state.Reason = &reason
		default:
			state.State = KycStateAvailable
		}
		states = append(states, state)
	}
	return states
}

// AttachOpenKyc fills user.KycPendingTier and user.KycSubmittedAt from the
// user's open request (left nil when nothing is in review), so every profile
// response tells the app what is waiting for review.
func (service *Service) AttachOpenKyc(requestContext context.Context, user *models.User) {
	var open models.KycSubmission
	err := service.database.WithContext(requestContext).Where("user_id = ? AND status = ?", user.ID, models.KycSubmissionPending).Take(&open).Error
	if err != nil {
		return
	}
	tier, submittedAt := open.Tier, open.SubmittedAt
	user.KycPendingTier, user.KycSubmittedAt = &tier, &submittedAt
}

// KycInput is what a verify screen submits.
type KycInput struct {
	Tier           int
	Bvn            string
	DateOfBirth    string
	Nin            string
	DocumentType   string
	DocumentNumber string
	Address        string
}

// kycDocumentRules is the format of each Tier 3 document number (spaces are
// removed and letters upper-cased before checking).
var kycDocumentRules = map[string]struct {
	pattern *regexp.Regexp
	hint    string
}{
	"nin_slip":        {regexp.MustCompile(`^\d{11}$`), "A NIN slip number has 11 digits."},
	"voters_card":     {regexp.MustCompile(`^[A-Z0-9]{19}$`), "A voter's card number has 19 letters and digits."},
	"drivers_licence": {regexp.MustCompile(`^[A-Z0-9]{8,15}$`), "A driver's licence number has 8 to 15 letters and digits."},
	"passport":        {regexp.MustCompile(`^[A-Z]\d{8}$`), "A passport number is a letter and 8 digits."},
}

var elevenDigits = regexp.MustCompile(`^\d{11}$`)

// kycCheck is one check a request will create.
type kycCheck struct {
	checkType string
	secret    string // encrypted at rest; only the last 4 characters stay readable
	document  string
}

// validateKyc checks the input for a tier and lists the checks it creates.
// Every problem is reported on its own field so the form can point at it.
func validateKyc(input *KycInput) ([]kycCheck, *time.Time, error) {
	fieldErrors := map[string][]string{}
	var checks []kycCheck
	var birthDate *time.Time
	switch input.Tier {
	case 1:
		if !elevenDigits.MatchString(input.Bvn) {
			fieldErrors["bvn"] = []string{"A BVN has 11 digits."}
		}
		parsed, err := time.Parse(time.DateOnly, input.DateOfBirth)
		switch {
		case err != nil:
			fieldErrors["date_of_birth"] = []string{"Enter your date of birth."}
		case parsed.AddDate(18, 0, 0).After(time.Now().UTC()):
			fieldErrors["date_of_birth"] = []string{"You must be 18 or older to use Tribe."}
		default:
			birthDate = &parsed
		}
		checks = []kycCheck{{"bvn", input.Bvn, "BVN"}, {"dob", input.DateOfBirth, ""}}
	case 2:
		if !elevenDigits.MatchString(input.Nin) {
			fieldErrors["nin"] = []string{"A NIN has 11 digits."}
		}
		checks = []kycCheck{{"nin", input.Nin, "NIN"}, {"liveness", "", ""}}
	case 3:
		input.DocumentNumber = strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(input.DocumentNumber), " ", ""))
		input.Address = strings.TrimSpace(input.Address)
		rule, known := kycDocumentRules[input.DocumentType]
		switch {
		case !known:
			fieldErrors["document_type"] = []string{"Choose a driver's licence, voter's card, NIN slip or passport."}
		case !rule.pattern.MatchString(input.DocumentNumber):
			fieldErrors["document_number"] = []string{rule.hint}
		}
		if len(input.Address) < 8 {
			fieldErrors["address"] = []string{"Enter your full home address."}
		}
		checks = []kycCheck{{"id_document", input.DocumentNumber, input.DocumentType}, {"address", input.Address, ""}}
	default:
		return nil, nil, apperrors.Validation(map[string][]string{"tier": {"Tier must be 1, 2 or 3."}})
	}
	if len(fieldErrors) > 0 {
		return nil, nil, apperrors.Validation(fieldErrors)
	}
	return checks, birthDate, nil
}

// inReview is the error for "a request is already waiting for review".
func inReview(tier int) error {
	return apperrors.New(http.StatusConflict, "VERIFICATION_IN_REVIEW", "Tier "+strconv.Itoa(tier)+" is in review. We'll let you know when it's done.")
}

// SubmitKyc sends ONE verification request for a tier. In one transaction it
// locks the user, then refuses if the tier is already verified
// (ALREADY_VERIFIED), if any request is still open (VERIFICATION_IN_REVIEW) or
// if the previous tier isn't done (TIER_ORDER). With the simulator, tier 1
// (BVN) is approved straight away; tiers 2 and 3 wait for an admin.
// Sensitive numbers are encrypted and only the last 4 characters are kept readable.
func (service *Service) SubmitKyc(requestContext context.Context, userID string, input KycInput) (*models.User, error) {
	checks, birthDate, err := validateKyc(&input)
	if err != nil {
		return nil, err
	}
	autoApprove := input.Tier == 1
	now := time.Now().UTC()
	err = database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		// The user row lock makes two submits from the same customer queue up:
		// the second one sees the first one's open request and is refused.
		var user models.User
		if err := database.LockForUpdate(databaseTransaction, &user, "id = ?", userID); err != nil {
			return apperrors.NotFound("User")
		}
		if input.Tier <= user.KycTier {
			return apperrors.New(http.StatusConflict, "ALREADY_VERIFIED", "Tier "+strconv.Itoa(input.Tier)+" is already verified.")
		}
		var open models.KycSubmission
		if err := databaseTransaction.Where("user_id = ? AND status = ?", userID, models.KycSubmissionPending).Take(&open).Error; err == nil {
			return inReview(open.Tier)
		} else if !database.IsNotFound(err) {
			return err
		}
		if user.KycTier < input.Tier-1 {
			return apperrors.New(http.StatusUnprocessableEntity, "TIER_ORDER", "Finish the previous tier first.")
		}

		submission := models.KycSubmission{
			ID: ids.New(), UserID: userID, Tier: input.Tier, Status: models.KycSubmissionPending, SubmittedAt: now, Version: 1,
		}
		if autoApprove {
			decidedBy := "simulator"
			submission.Status, submission.DecidedAt, submission.DecidedBy = models.KycSubmissionApproved, &now, &decidedBy
		} else {
			openKey := userID
			submission.OpenKey = &openKey
		}
		if err := databaseTransaction.Create(&submission).Error; err != nil {
			if database.IsDuplicateEntry(err) {
				return inReview(input.Tier)
			}
			return err
		}
		for _, item := range checks {
			submissionID := submission.ID
			row := models.KycVerification{
				ID: ids.New(), UserID: userID, SubmissionID: &submissionID, Tier: input.Tier, CheckType: item.checkType, Provider: "simulator",
				Reference: "KYC-" + strings.ToUpper(ids.RandomToken(3)), Status: submission.Status, SubmittedAt: now, Version: 1,
			}
			if item.secret != "" {
				encrypted, err := service.toolkit.Encrypt(item.secret)
				if err != nil {
					return err
				}
				row.DataEncrypted = encrypted
				masked := "•••• " + item.secret[max(0, len(item.secret)-4):]
				row.DocumentMasked = &masked
			}
			if item.document != "" {
				document := item.document
				row.DocumentType = &document
			}
			if autoApprove {
				row.DecidedAt, row.DecidedBy = submission.DecidedAt, submission.DecidedBy
			}
			if err := databaseTransaction.Create(&row).Error; err != nil {
				return err
			}
		}

		updates := map[string]any{"kyc_status": "pending", "updated_at": now, "version": user.Version + 1}
		if autoApprove {
			// never lower the tier: a higher tier can't reach here (ALREADY_VERIFIED)
			updates["kyc_status"], updates["kyc_tier"] = "approved", max(user.KycTier, input.Tier)
		}
		if birthDate != nil {
			updates["date_of_birth"] = *birthDate
		}
		if input.Tier == 3 {
			updates["address"] = input.Address
		}
		return databaseTransaction.Model(&models.User{}).Where("id = ?", userID).Updates(updates).Error
	})
	if err != nil {
		return nil, err
	}
	return service.Profile(requestContext, userID)
}

// ApplyForGlobalAccounts saves the answers for USD/GBP/EUR accounts.
func (service *Service) ApplyForGlobalAccounts(requestContext context.Context, userID string, answers map[string]any) (*models.User, error) {
	err := service.database.WithContext(requestContext).Create(&models.GlobalAccountApplication{
		ID: ids.New(), UserID: userID, Answers: shared.JSON(answers), Status: "review", CreatedAt: time.Now().UTC(),
	}).Error
	if err != nil {
		return nil, err
	}
	return service.lockAndUpdate(requestContext, userID, func(user *models.User) error { return nil }, map[string]any{"global_accounts_status": "review"})
}

// ─── Inbox ───────────────────────────────────────────────────────────

// Inbox returns the newest messages.
func (service *Service) Inbox(requestContext context.Context, userID string) ([]models.InboxMessage, error) {
	var messages []models.InboxMessage
	err := service.database.WithContext(requestContext).Where("user_id = ?", userID).Order("created_at DESC").Limit(200).Find(&messages).Error
	return messages, err
}

// MarkInboxRead marks one message (or all, when messageID is "") as read.
func (service *Service) MarkInboxRead(requestContext context.Context, userID string, messageID string) error {
	query := service.database.WithContext(requestContext).Model(&models.InboxMessage{}).Where("user_id = ? AND read_at IS NULL", userID)
	if messageID != "" {
		query = query.Where("id = ?", messageID)
	}
	return query.Update("read_at", time.Now().UTC()).Error
}

// ─── Support ─────────────────────────────────────────────────────────

// SupportTickets lists the user's support threads.
func (service *Service) SupportTickets(requestContext context.Context, userID string) ([]models.SupportThread, error) {
	var threads []models.SupportThread
	err := service.database.WithContext(requestContext).Where("user_id = ?", userID).Order("updated_at DESC").Find(&threads).Error
	return threads, err
}

// SupportTicket returns one thread with its messages.
func (service *Service) SupportTicket(requestContext context.Context, userID string, threadID string) (*models.SupportThread, []models.SupportMessage, error) {
	var thread models.SupportThread
	if err := service.database.WithContext(requestContext).Where("id = ? AND user_id = ?", threadID, userID).Take(&thread).Error; err != nil {
		return nil, nil, apperrors.NotFound("Ticket")
	}
	var messages []models.SupportMessage
	err := service.database.WithContext(requestContext).Where("thread_id = ?", threadID).Order("created_at").Find(&messages).Error
	return &thread, messages, err
}

// OpenTicket starts a support conversation.
func (service *Service) OpenTicket(requestContext context.Context, userID string, subject string, body string, transactionID string) (*models.SupportThread, error) {
	user, err := service.Profile(requestContext, userID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(subject) == "" || strings.TrimSpace(body) == "" {
		return nil, apperrors.Validation(map[string][]string{"body": {"Tell us what happened."}})
	}
	now := time.Now().UTC()
	thread := models.SupportThread{ID: ids.New(), UserID: userID, Channel: "ticket", Subject: subject, Status: "open", Priority: "normal", UnreadForAdmin: 1, Version: 1, CreatedAt: now, UpdatedAt: now}
	if transactionID != "" {
		thread.TransactionID = &transactionID
	}
	err = database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		if err := databaseTransaction.Create(&thread).Error; err != nil {
			return err
		}
		return databaseTransaction.Create(&models.SupportMessage{ID: ids.New(), ThreadID: thread.ID, SenderType: "customer", AuthorName: user.FullName(), Body: body, CreatedAt: now}).Error
	})
	service.publishSupportEvent(requestContext, "ticket.new", thread.ID, userID)
	return &thread, err
}

// ReplyToTicket adds a customer message to a thread.
func (service *Service) ReplyToTicket(requestContext context.Context, userID string, threadID string, body string) error {
	user, err := service.Profile(requestContext, userID)
	if err != nil {
		return err
	}
	err = database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		var thread models.SupportThread
		if err := database.LockForUpdate(databaseTransaction, &thread, "id = ? AND user_id = ?", threadID, userID); err != nil {
			return apperrors.NotFound("Ticket")
		}
		now := time.Now().UTC()
		if err := databaseTransaction.Create(&models.SupportMessage{ID: ids.New(), ThreadID: threadID, SenderType: "customer", AuthorName: user.FullName(), Body: body, CreatedAt: now}).Error; err != nil {
			return err
		}
		return databaseTransaction.Model(&models.SupportThread{}).Where("id = ?", threadID).
			Updates(map[string]any{"status": "open", "unread_for_admin": thread.UnreadForAdmin + 1, "updated_at": now, "version": thread.Version + 1}).Error
	})
	if err == nil {
		service.publishSupportEvent(requestContext, "chat.message", threadID, userID)
	}
	return err
}

// publishSupportEvent tells the admin panel and the customer's other screens
// (via Redis) that one of their support threads changed.
func (service *Service) publishSupportEvent(requestContext context.Context, eventType string, threadID string, userID string) {
	events.PublishSupportEvent(requestContext, service.redis, eventType, threadID, userID)
}
