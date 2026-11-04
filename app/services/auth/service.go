// Package auth signs mobile users up and in, and enforces SINGLE-DEVICE
// sign-in.
//
// The single-device rule in plain words:
//
//   - Each phone sends its ID in the X-Device-Imei header.
//   - A user can have only ONE active phone at a time. The database enforces
//     this with a UNIQUE index (see the user_devices migration).
//   - Signing up, or signing in when no phone is active yet, is a "new
//     entry": that phone simply becomes the active one.
//   - Signing in on the SAME active phone works straight away.
//   - Signing in on a DIFFERENT phone does not give tokens. Instead we email a
//     code. Once the code is entered (POST auth/device/verify), the new phone
//     becomes active, the old phone is marked "replaced" and every session on
//     it is revoked, so the old phone is signed out on its next request.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"github.com/tribe-africa/tribe-backend/app/apperrors"
	"github.com/tribe-africa/tribe-backend/app/cache"
	"github.com/tribe-africa/tribe-backend/app/jobs"
	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/app/services/ledger"
	"github.com/tribe-africa/tribe-backend/app/services/security"
	"github.com/tribe-africa/tribe-backend/config"
	"github.com/tribe-africa/tribe-backend/database"
	"github.com/tribe-africa/tribe-backend/pkg/ids"
)

// Purposes of email codes.
const (
	PurposeSignup        = "signup"
	PurposePasswordReset = "password_reset"
)

// maximumCodeAttempts is how many wrong guesses a code allows.
const maximumCodeAttempts = 5

// Service handles sign-up, sign-in, devices and tokens for the mobile app.
type Service struct {
	database   *gorm.DB
	redis      *redis.Client
	toolkit    *security.Toolkit
	dispatcher *jobs.Dispatcher
	ledger     *ledger.Service
	settings   config.Config
}

// NewService creates the auth service.
func NewService(connection *gorm.DB, redisClient *redis.Client, toolkit *security.Toolkit, dispatcher *jobs.Dispatcher, ledgerService *ledger.Service, settings config.Config) *Service {
	return &Service{database: connection, redis: redisClient, toolkit: toolkit, dispatcher: dispatcher, ledger: ledgerService, settings: settings}
}

// DeviceInput describes the phone making the request.
type DeviceInput struct {
	Imei       string `json:"imei"`
	Name       string `json:"name"`
	Platform   string `json:"platform"`
	Model      string `json:"model"`
	AppVersion string `json:"app_version"`
}

// TokenPair is what a successful sign-in returns.
type TokenPair struct {
	AccessToken           string    `json:"access_token"`
	AccessTokenExpiresAt  time.Time `json:"access_token_expires_at"`
	RefreshToken          string    `json:"refresh_token"`
	RefreshTokenExpiresAt time.Time `json:"refresh_token_expires_at"`
	SessionID             string    `json:"session_id"`
}

// ─── Email codes ─────────────────────────────────────────────────────

// EmailIsTaken tells the sign-up screen whether an email already has an account.
func (service *Service) EmailIsTaken(requestContext context.Context, email string) (bool, error) {
	var count int64
	err := service.database.WithContext(requestContext).Model(&models.User{}).Where("email = ?", normaliseEmail(email)).Count(&count).Error
	return count > 0, err
}

// TagIsAvailable tells the sign-up screen whether a @tag is free.
func (service *Service) TagIsAvailable(requestContext context.Context, tag string) (bool, error) {
	var count int64
	err := service.database.WithContext(requestContext).Model(&models.User{}).Where("tag = ?", normaliseTag(tag)).Count(&count).Error
	return count == 0, err
}

// Daily limits on email codes.
//   - maximumCodesPerDayFromOneAddress: codes for one email asked from one IP
//     address. Someone spamming a victim's email only uses up THEIR OWN
//     budget, so the real owner (on another network) can still get a code.
//   - maximumCodesPerDayPerEmail: codes for one email from all addresses.
//   - maximumWrongCodesPerDay: wrong guesses for one email across ALL its
//     codes. This is what stops guessing: 20 guesses at a 6-digit code is a
//     1 in 50,000 chance per day, however many codes are requested.
const (
	maximumCodesPerDayFromOneAddress = 10
	maximumCodesPerDayPerEmail       = 50
	maximumWrongCodesPerDay          = 20
)

// SendEmailCode creates a 6-digit code and queues an email with it, with no
// waiting between requests (see the daily limits above).
// Asking for a new code cancels the older ones, so only the newest works.
// It returns the code ONLY so local development can show it (never in production).
func (service *Service) SendEmailCode(requestContext context.Context, email string, purpose string, clientAddress string) (string, error) {
	email = normaliseEmail(email)
	// No "wait 30 seconds" rule: someone who goes back to fix their email, or asks again, gets a
	// new code straight away. The daily limits below stop spam, the wrong-guess limit stops
	// guessing, and a new code cancels the older ones, so only the newest ever works.
	fromThisAddress, err := service.countToday(requestContext, "email-codes-today:"+purpose+":"+email+":"+clientAddress)
	if err != nil {
		return "", err
	}
	forThisEmail, err := service.countToday(requestContext, "email-codes-today:"+purpose+":"+email)
	if err != nil {
		return "", err
	}
	if fromThisAddress > maximumCodesPerDayFromOneAddress || forThisEmail > maximumCodesPerDayPerEmail {
		return "", apperrors.New(http.StatusTooManyRequests, "TOO_MANY_CODES", "Too many codes were requested for this email today. Try again tomorrow.")
	}

	now := time.Now().UTC()
	// Cancel every older code that has not been used yet.
	if err := service.database.WithContext(requestContext).Model(&models.EmailVerification{}).
		Where("email = ? AND purpose = ? AND verified_at IS NULL AND expires_at > ?", email, purpose, now).
		Update("expires_at", now).Error; err != nil {
		return "", err
	}

	code := ids.RandomDigits(6)
	verification := models.EmailVerification{
		ID:        ids.New(),
		Email:     email,
		Purpose:   purpose,
		CodeHash:  service.toolkit.HashCode(code),
		ExpiresAt: time.Now().UTC().Add(service.settings.Auth.CodeLifetime),
		CreatedAt: time.Now().UTC(),
	}
	if err := service.database.WithContext(requestContext).Create(&verification).Error; err != nil {
		return "", err
	}
	// The email is sent by the worker, so the app does not wait for the mail server.
	service.dispatcher.SendEmailCode(requestContext, jobs.SendEmailCodePayload{Email: email, Code: code, Purpose: purpose})
	return code, nil
}

// countToday adds one to a 24-hour counter and returns the new value.
// INCR and EXPIRE NX run together in one MULTI/EXEC, so a counter can never
// be left without an expiry (which would block an email forever).
func (service *Service) countToday(requestContext context.Context, key string) (int64, error) {
	var counter *redis.IntCmd
	_, err := service.redis.TxPipelined(requestContext, func(pipeline redis.Pipeliner) error {
		counter = pipeline.Incr(requestContext, key)
		pipeline.ExpireNX(requestContext, key, 24*time.Hour)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return counter.Val(), nil
}

// VerifyEmailCode checks a code and returns a one-time verification token
// that the next step (register / reset password) must send back.
func (service *Service) VerifyEmailCode(requestContext context.Context, email string, purpose string, code string) (string, error) {
	email = normaliseEmail(email)
	var verification models.EmailVerification
	err := service.database.WithContext(requestContext).
		Where("email = ? AND purpose = ? AND verified_at IS NULL AND expires_at > ?", email, purpose, time.Now().UTC()).
		Order("created_at DESC").
		Take(&verification).Error
	if database.IsNotFound(err) {
		return "", apperrors.New(http.StatusUnprocessableEntity, "CODE_EXPIRED", "This code has expired. Ask for a new one.")
	}
	if err != nil {
		return "", err
	}
	wrongCodesKey := "email-wrong-codes-today:" + purpose + ":" + email
	if wrongToday, _ := service.redis.Get(requestContext, wrongCodesKey).Int64(); wrongToday >= maximumWrongCodesPerDay {
		return "", apperrors.New(http.StatusTooManyRequests, "TOO_MANY_ATTEMPTS", "Too many wrong codes today. Try again tomorrow.")
	}
	if err := service.countAttempt(requestContext, "email_verifications", verification.ID); err != nil {
		return "", err
	}
	if !service.toolkit.CodesMatch(verification.CodeHash, code) {
		if _, err := service.countToday(requestContext, wrongCodesKey); err != nil {
			return "", err
		}
		return "", apperrors.New(http.StatusUnprocessableEntity, "WRONG_CODE", "That code is not correct.")
	}

	token := ids.RandomToken(32)
	tokenHash := service.toolkit.HashCode(token)
	now := time.Now().UTC()
	result := service.database.WithContext(requestContext).Model(&models.EmailVerification{}).
		Where("id = ? AND verified_at IS NULL", verification.ID).
		Updates(map[string]any{"verified_at": now, "verification_token_hash": tokenHash})
	if result.Error != nil {
		return "", result.Error
	}
	if result.RowsAffected != 1 {
		return "", apperrors.New(http.StatusConflict, "CODE_USED", "This code was already used.")
	}
	return token, nil
}

// countAttempt adds one to a code's attempt counter in a single atomic
// UPDATE, and refuses once the limit is reached. Doing it in one statement
// means two guesses at the same time can't both sneak under the limit.
func (service *Service) countAttempt(requestContext context.Context, table string, rowID string) error {
	result := service.database.WithContext(requestContext).Exec(
		"UPDATE "+table+" SET attempts = attempts + 1 WHERE id = ? AND attempts < ?", rowID, maximumCodeAttempts)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return apperrors.New(http.StatusTooManyRequests, "TOO_MANY_ATTEMPTS", "Too many wrong codes. Ask for a new one.")
	}
	return nil
}

// consumeVerificationToken marks a verified email token as used, once.
func (service *Service) consumeVerificationToken(databaseTransaction *gorm.DB, email string, purpose string, token string) error {
	result := databaseTransaction.Model(&models.EmailVerification{}).
		Where("email = ? AND purpose = ? AND verification_token_hash = ? AND consumed_at IS NULL AND verified_at > ?",
			email, purpose, service.toolkit.HashCode(token), time.Now().UTC().Add(-time.Hour)).
		Update("consumed_at", time.Now().UTC())
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return apperrors.New(http.StatusUnprocessableEntity, "EMAIL_NOT_VERIFIED", "Please verify your email again.")
	}
	return nil
}

// verificationTokenIsUsable checks, WITHOUT using it up, that a verification
// token would be accepted by consumeVerificationToken. We call it before the
// slow password hash so a made-up token is refused cheaply; the real,
// one-time consume still happens inside the transaction.
func (service *Service) verificationTokenIsUsable(requestContext context.Context, email string, purpose string, token string) error {
	var matching int64
	err := service.database.WithContext(requestContext).Model(&models.EmailVerification{}).
		Where("email = ? AND purpose = ? AND verification_token_hash = ? AND consumed_at IS NULL AND verified_at > ?",
			email, purpose, service.toolkit.HashCode(token), time.Now().UTC().Add(-time.Hour)).
		Count(&matching).Error
	if err != nil {
		return err
	}
	if matching == 0 {
		return apperrors.New(http.StatusUnprocessableEntity, "EMAIL_NOT_VERIFIED", "Please verify your email again.")
	}
	return nil
}

// ─── Register ────────────────────────────────────────────────────────

// RegisterInput is everything the sign-up screens collect.
type RegisterInput struct {
	Email             string
	VerificationToken string
	Password          string
	FirstName         string
	LastName          string
	Tag               string
	Phone             string
	Country           string
	ReferralCode      string
	HeardFrom         string
	Device            DeviceInput
	IPAddress         string
	UserAgent         string
}

// Register creates the account, makes this phone the active device (a "new
// entry", so no device check is needed) and signs the user in.
func (service *Service) Register(requestContext context.Context, input RegisterInput) (*models.User, *TokenPair, error) {
	input.Email = normaliseEmail(input.Email)
	input.Tag = normaliseTag(input.Tag)
	if problem := PasswordProblem(input.Password); problem != "" {
		return nil, nil, apperrors.Validation(map[string][]string{"password": {problem}})
	}
	// Refuse a fake token BEFORE the 64 MB password hash below.
	if err := service.verificationTokenIsUsable(requestContext, input.Email, PurposeSignup, input.VerificationToken); err != nil {
		return nil, nil, err
	}

	now := time.Now().UTC()
	user := models.User{
		ID:                   ids.New(),
		Email:                input.Email,
		PasswordHash:         service.toolkit.HashPassword(input.Password),
		FirstName:            strings.TrimSpace(input.FirstName),
		LastName:             strings.TrimSpace(input.LastName),
		Tag:                  input.Tag,
		Country:              strings.ToUpper(input.Country),
		Status:               "active",
		KycStatus:            "none",
		GlobalAccountsStatus: "none",
		ReferralCode:         newReferralCode(input.Tag),
		Labels:               []byte("[]"),
		EmailVerifiedAt:      &now,
		LastActiveAt:         &now,
		Version:              1,
		CreatedAt:            now,
		UpdatedAt:            now,
	}
	if input.Phone != "" {
		phone := input.Phone
		user.Phone = &phone // saved, but phone_verified_at stays empty until the SMS code is entered
	}
	if input.HeardFrom != "" {
		heardFrom := input.HeardFrom
		user.HeardFrom = &heardFrom
	}

	var tokens *TokenPair
	err := database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		if err := service.consumeVerificationToken(databaseTransaction, input.Email, PurposeSignup, input.VerificationToken); err != nil {
			return err
		}
		if input.ReferralCode != "" {
			var referrer models.User
			if err := databaseTransaction.Where("referral_code = ?", strings.ToUpper(input.ReferralCode)).Take(&referrer).Error; err == nil {
				user.ReferredByUserID = &referrer.ID
			}
		}
		if err := databaseTransaction.Create(&user).Error; err != nil {
			if database.IsDuplicateEntry(err) {
				return apperrors.New(http.StatusConflict, "ALREADY_EXISTS", "That email or @tag is already used.")
			}
			return err
		}
		device, err := service.createActiveDevice(databaseTransaction, user.ID, input.Device)
		if err != nil {
			return err
		}
		tokens, err = service.startSession(databaseTransaction, user.ID, device, input.IPAddress, input.UserAgent)
		return err
	})
	if err != nil {
		return nil, nil, err
	}

	// Open the default wallets (NGN and USD) and the referral account.
	// Done AFTER the transaction on purpose (see ledger.EnsureUserAccount).
	for _, assetCode := range []string{"NGN", "USD"} {
		if _, err := service.ledger.EnsureUserAccount(requestContext, user.ID, ledger.CodeUserPrimary, assetCode); err != nil {
			return nil, nil, err
		}
	}
	if _, err := service.ledger.EnsureUserAccount(requestContext, user.ID, ledger.CodeUserReferral, "NGN"); err != nil {
		return nil, nil, err
	}
	return &user, tokens, nil
}

// ─── Login ───────────────────────────────────────────────────────────

// LoginInput is what the sign-in screen sends.
type LoginInput struct {
	Email     string
	Password  string
	Device    DeviceInput
	IPAddress string
	UserAgent string
}

// LoginResult has EITHER tokens (signed in) OR a device challenge (new phone).
type LoginResult struct {
	Tokens               *TokenPair
	User                 *models.User
	DeviceVerificationID string
	DebugCode            string // local development only
}

// Login checks the password and applies the single-device rule.
func (service *Service) Login(requestContext context.Context, input LoginInput) (*LoginResult, error) {
	email := normaliseEmail(input.Email)
	invalidCredentials := apperrors.New(http.StatusUnauthorized, "INVALID_CREDENTIALS", "Email or password is not correct.")

	// Block password guessing: 10 attempts per email per 15 minutes,
	// counted BEFORE the slow password hash (so a burst of parallel guesses
	// cannot all slip past the limit) and counted for unknown emails too.
	// A successful sign-in clears the counter.
	failureKey := "login-failures:" + email
	if !cache.TakeAttempt(requestContext, service.redis, failureKey, 10, 15*time.Minute) {
		return nil, apperrors.New(http.StatusTooManyRequests, "TOO_MANY_ATTEMPTS", "Too many attempts. Try again in 15 minutes.")
	}

	var user models.User
	err := service.database.WithContext(requestContext).Where("email = ?", email).Take(&user).Error
	if database.IsNotFound(err) {
		service.toolkit.DummyPasswordCheck(input.Password) // same timing as a real check
		return nil, invalidCredentials
	}
	if err != nil {
		return nil, err
	}
	if !service.toolkit.CheckPassword(user.PasswordHash, input.Password) {
		return nil, invalidCredentials // already counted by TakeAttempt above
	}
	cache.ForgetAttempts(requestContext, service.redis, failureKey)
	if user.Status == "closed" {
		return nil, apperrors.New(http.StatusForbidden, "ACCOUNT_CLOSED", "This account is closed.")
	}
	if strings.TrimSpace(input.Device.Imei) == "" {
		return nil, apperrors.Validation(map[string][]string{"device.imei": {"The device ID is required."}})
	}

	result := &LoginResult{User: &user}
	var codeToSend string
	var deviceName string
	err = database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		// Lock the user row: two logins of the same user now happen one after the other.
		var lockedUser models.User
		if err := database.LockForUpdate(databaseTransaction, &lockedUser, "id = ?", user.ID); err != nil {
			return err
		}

		var activeDevice models.UserDevice
		activeError := databaseTransaction.Where("user_id = ? AND status = ?", user.ID, models.DeviceStatusActive).Take(&activeDevice).Error
		hasActiveDevice := activeError == nil
		if activeError != nil && !database.IsNotFound(activeError) {
			return activeError
		}

		// Case 1: same phone as the active one -> sign in.
		if hasActiveDevice && activeDevice.DeviceImei == input.Device.Imei {
			tokens, err := service.startSession(databaseTransaction, user.ID, &activeDevice, input.IPAddress, input.UserAgent)
			result.Tokens = tokens
			return err
		}

		// Case 2: no active phone at all -> "new entry", this phone becomes active.
		if !hasActiveDevice {
			device, err := service.createActiveDevice(databaseTransaction, user.ID, input.Device)
			if err != nil {
				return err
			}
			tokens, err := service.startSession(databaseTransaction, user.ID, device, input.IPAddress, input.UserAgent)
			result.Tokens = tokens
			return err
		}

		// Case 3: a DIFFERENT phone -> park it as "pending" and email a code.
		pendingDevice, err := service.upsertDevice(databaseTransaction, user.ID, input.Device, models.DeviceStatusPending)
		if err != nil {
			return err
		}
		codeToSend = ids.RandomDigits(6)
		deviceName = pendingDevice.Name
		verification := models.DeviceVerification{
			ID:        ids.New(),
			UserID:    user.ID,
			DeviceID:  pendingDevice.ID,
			CodeHash:  service.toolkit.HashCode(codeToSend),
			ExpiresAt: time.Now().UTC().Add(service.settings.Auth.CodeLifetime),
			CreatedAt: time.Now().UTC(),
		}
		result.DeviceVerificationID = verification.ID
		return databaseTransaction.Create(&verification).Error
	})
	if err != nil {
		return nil, err
	}

	if codeToSend != "" {
		// Only send the email AFTER the transaction committed.
		service.dispatcher.SendDeviceCode(requestContext, jobs.SendDeviceCodePayload{UserID: user.ID, Email: user.Email, Code: codeToSend, DeviceName: deviceName})
		if service.settings.IsLocal() {
			result.DebugCode = codeToSend
		}
	}
	return result, nil
}

// VerifyNewDevice finishes a sign-in on a new phone: it checks the emailed
// code, signs the OLD phone out and makes this phone the active one.
func (service *Service) VerifyNewDevice(requestContext context.Context, verificationID string, code string, deviceImei string, ipAddress string, userAgent string) (*models.User, *TokenPair, error) {
	var verification models.DeviceVerification
	if err := service.database.WithContext(requestContext).Where("id = ?", verificationID).Take(&verification).Error; err != nil {
		return nil, nil, apperrors.New(http.StatusUnprocessableEntity, "CODE_EXPIRED", "This code has expired. Sign in again.")
	}
	if err := service.countAttempt(requestContext, "device_verifications", verification.ID); err != nil {
		return nil, nil, err
	}

	var user models.User
	var tokens *TokenPair
	var revokedSessionIDs []string
	err := database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		// Lock order: user first, then the verification. Every device change
		// for this user now waits in line, so two phones can't both win.
		if err := database.LockForUpdate(databaseTransaction, &user, "id = ?", verification.UserID); err != nil {
			return err
		}
		var lockedVerification models.DeviceVerification
		if err := database.LockForUpdate(databaseTransaction, &lockedVerification, "id = ?", verification.ID); err != nil {
			return err
		}
		if lockedVerification.VerifiedAt != nil || lockedVerification.ExpiresAt.Before(time.Now().UTC()) {
			return apperrors.New(http.StatusUnprocessableEntity, "CODE_EXPIRED", "This code has expired. Sign in again.")
		}
		if !service.toolkit.CodesMatch(lockedVerification.CodeHash, code) {
			return apperrors.New(http.StatusUnprocessableEntity, "WRONG_CODE", "That code is not correct.")
		}

		var newDevice models.UserDevice
		if err := database.LockForUpdate(databaseTransaction, &newDevice, "id = ?", lockedVerification.DeviceID); err != nil {
			return err
		}
		if newDevice.DeviceImei != deviceImei {
			return apperrors.New(http.StatusUnprocessableEntity, "WRONG_DEVICE", "Finish signing in on the phone that asked for the code.")
		}

		now := time.Now().UTC()
		// 1. The old phone is replaced.
		if err := databaseTransaction.Model(&models.UserDevice{}).
			Where("user_id = ? AND status = ?", user.ID, models.DeviceStatusActive).
			Updates(map[string]any{"status": models.DeviceStatusReplaced, "replaced_at": now, "updated_at": now}).Error; err != nil {
			return err
		}
		// 2. Every open session is revoked (the old phone is signed out).
		if err := databaseTransaction.Model(&models.UserSession{}).
			Where("user_id = ? AND revoked_at IS NULL", user.ID).
			Pluck("id", &revokedSessionIDs).Error; err != nil {
			return err
		}
		if err := databaseTransaction.Model(&models.UserSession{}).
			Where("user_id = ? AND revoked_at IS NULL", user.ID).
			Updates(map[string]any{"revoked_at": now, "revoked_reason": "device_replaced"}).Error; err != nil {
			return err
		}
		// 3. The new phone becomes active. If somehow another phone is still
		//    active, the UNIQUE index rejects this and the whole thing rolls back.
		if err := databaseTransaction.Model(&models.UserDevice{}).Where("id = ?", newDevice.ID).
			Updates(map[string]any{"status": models.DeviceStatusActive, "activated_at": now, "updated_at": now}).Error; err != nil {
			if database.IsDuplicateEntry(err) {
				return apperrors.New(http.StatusConflict, "DEVICE_CHANGED", "Another phone was just activated. Sign in again.")
			}
			return err
		}
		newDevice.Status = models.DeviceStatusActive
		if err := databaseTransaction.Model(&models.DeviceVerification{}).Where("id = ?", lockedVerification.ID).
			Update("verified_at", now).Error; err != nil {
			return err
		}
		// 4. Tell the user (shows in the inbox of the new phone too).
		if err := databaseTransaction.Create(&models.InboxMessage{
			ID: ids.New(), UserID: user.ID, Kind: "security",
			Title:     "New phone signed in",
			Body:      "Tribe is now active on " + newDevice.Name + ". Your other phone was signed out. Wasn't you? Contact support right away.",
			CreatedAt: now,
		}).Error; err != nil {
			return err
		}
		var err error
		tokens, err = service.startSession(databaseTransaction, user.ID, &newDevice, ipAddress, userAgent)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	// The old phone's cached sessions must go too, or it could keep working for up to a minute.
	service.forgetSessions(requestContext, revokedSessionIDs)
	return &user, tokens, nil
}

// ─── Tokens and sessions ─────────────────────────────────────────────

// Refresh swaps a refresh token for a new access + refresh token. Each
// refresh token works ONCE. If an old one is used again, someone may have
// stolen it, so we revoke the whole session.
func (service *Service) Refresh(requestContext context.Context, refreshToken string, deviceImei string) (*TokenPair, error) {
	var tokens *TokenPair
	revokeSession := ""
	err := database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		var stored models.RefreshToken
		if err := database.LockForUpdate(databaseTransaction, &stored, "token_hash = ?", service.toolkit.HashCode(refreshToken)); err != nil {
			return apperrors.ErrUnauthenticated
		}
		if stored.UsedAt != nil {
			revokeSession = stored.SessionID
			return apperrors.ErrUnauthenticated
		}
		if stored.ExpiresAt.Before(time.Now().UTC()) {
			return apperrors.ErrUnauthenticated
		}
		var session models.UserSession
		if err := database.LockForUpdate(databaseTransaction, &session, "id = ?", stored.SessionID); err != nil {
			return apperrors.ErrUnauthenticated
		}
		if session.RevokedAt != nil {
			return apperrors.ErrDeviceNotActive
		}
		var device models.UserDevice
		if err := databaseTransaction.Where("id = ?", session.DeviceID).Take(&device).Error; err != nil {
			return err
		}
		if device.Status != models.DeviceStatusActive || device.DeviceImei != deviceImei {
			return apperrors.ErrDeviceNotActive
		}

		now := time.Now().UTC()
		if err := databaseTransaction.Model(&models.RefreshToken{}).Where("id = ?", stored.ID).Update("used_at", now).Error; err != nil {
			return err
		}
		newRefreshToken, refreshExpiresAt, err := service.createRefreshToken(databaseTransaction, session.ID)
		if err != nil {
			return err
		}
		if err := databaseTransaction.Model(&models.UserSession{}).Where("id = ?", session.ID).
			Updates(map[string]any{"last_used_at": now, "expires_at": refreshExpiresAt}).Error; err != nil {
			return err
		}
		accessToken, accessExpiresAt, err := service.toolkit.IssueAccessToken(security.AudienceMobile, session.UserID, session.ID, session.DeviceID, service.settings.Auth.AccessTokenLifetime)
		if err != nil {
			return err
		}
		tokens = &TokenPair{AccessToken: accessToken, AccessTokenExpiresAt: accessExpiresAt, RefreshToken: newRefreshToken, RefreshTokenExpiresAt: refreshExpiresAt, SessionID: session.ID}
		return nil
	})
	if revokeSession != "" {
		service.RevokeSession(requestContext, revokeSession, "refresh_token_reused")
	}
	return tokens, err
}

// RevokeSession signs one session out (used by logout).
func (service *Service) RevokeSession(requestContext context.Context, sessionID string, reason string) error {
	err := service.database.WithContext(requestContext).Model(&models.UserSession{}).
		Where("id = ? AND revoked_at IS NULL", sessionID).
		Updates(map[string]any{"revoked_at": time.Now().UTC(), "revoked_reason": reason}).Error
	service.forgetSessions(requestContext, []string{sessionID})
	return err
}

// ResetPassword sets a new password after the reset code was verified, and
// signs out every session (the person resetting may not own the old phone).
func (service *Service) ResetPassword(requestContext context.Context, email string, verificationToken string, newPassword string) error {
	email = normaliseEmail(email)
	if problem := PasswordProblem(newPassword); problem != "" {
		return apperrors.Validation(map[string][]string{"password": {problem}})
	}
	// Check the token cheaply, then hash OUTSIDE the transaction: the slow
	// hash must not hold a database connection and a row lock.
	if err := service.verificationTokenIsUsable(requestContext, email, PurposePasswordReset, verificationToken); err != nil {
		return err
	}
	newPasswordHash := service.toolkit.HashPassword(newPassword)
	var revokedSessionIDs []string
	err := database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		if err := service.consumeVerificationToken(databaseTransaction, email, PurposePasswordReset, verificationToken); err != nil {
			return err
		}
		var user models.User
		if err := database.LockForUpdate(databaseTransaction, &user, "email = ?", email); err != nil {
			return apperrors.NotFound("Account")
		}
		now := time.Now().UTC()
		if err := databaseTransaction.Model(&models.User{}).Where("id = ?", user.ID).
			Updates(map[string]any{"password_hash": newPasswordHash, "updated_at": now, "version": gorm.Expr("version + 1")}).Error; err != nil {
			return err
		}
		databaseTransaction.Model(&models.UserSession{}).Where("user_id = ? AND revoked_at IS NULL", user.ID).Pluck("id", &revokedSessionIDs)
		return databaseTransaction.Model(&models.UserSession{}).Where("user_id = ? AND revoked_at IS NULL", user.ID).
			Updates(map[string]any{"revoked_at": now, "revoked_reason": "password_reset"}).Error
	})
	if err == nil {
		service.forgetSessions(requestContext, revokedSessionIDs)
	}
	return err
}

// ChangePassword changes the password of a signed-in user.
func (service *Service) ChangePassword(requestContext context.Context, userID string, currentPassword string, newPassword string) error {
	if problem := PasswordProblem(newPassword); problem != "" {
		return apperrors.Validation(map[string][]string{"password": {problem}})
	}
	// Limit guesses at the current password per user (10 per 15 minutes),
	// counted atomically before the slow hash.
	attemptKey := "password-change-attempts:" + userID
	if !cache.TakeAttempt(requestContext, service.redis, attemptKey, 10, 15*time.Minute) {
		return apperrors.New(http.StatusTooManyRequests, "TOO_MANY_ATTEMPTS", "Too many attempts. Try again in 15 minutes.")
	}

	// Both slow hashes run OUTSIDE the transaction, so no database
	// connection or row lock is held while they work.
	var user models.User
	if err := service.database.WithContext(requestContext).Where("id = ?", userID).Take(&user).Error; err != nil {
		return err
	}
	if !service.toolkit.CheckPassword(user.PasswordHash, currentPassword) {
		return apperrors.New(http.StatusUnprocessableEntity, "WRONG_PASSWORD", "Your current password is not correct.")
	}
	cache.ForgetAttempts(requestContext, service.redis, attemptKey)
	newPasswordHash := service.toolkit.HashPassword(newPassword)

	return database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		// Lock -> re-read -> check -> write. If the password changed between
		// our check above and this lock (a reset, another phone), the hash
		// we checked against is no longer current: refuse with 409.
		var lockedUser models.User
		if err := database.LockForUpdate(databaseTransaction, &lockedUser, "id = ?", userID); err != nil {
			return err
		}
		if lockedUser.PasswordHash != user.PasswordHash {
			return apperrors.ErrStaleData
		}
		return databaseTransaction.Model(&models.User{}).Where("id = ?", userID).
			Updates(map[string]any{"password_hash": newPasswordHash, "updated_at": time.Now().UTC(), "version": gorm.Expr("version + 1")}).Error
	})
}

// createActiveDevice makes a phone the active device (inside a transaction).
func (service *Service) createActiveDevice(databaseTransaction *gorm.DB, userID string, input DeviceInput) (*models.UserDevice, error) {
	device, err := service.upsertDevice(databaseTransaction, userID, input, models.DeviceStatusActive)
	if err != nil && database.IsDuplicateEntry(err) {
		return nil, apperrors.New(http.StatusConflict, "DEVICE_CHANGED", "Another phone was just activated. Sign in again.")
	}
	return device, err
}

// upsertDevice creates the device row, or updates it if this user used this phone before.
func (service *Service) upsertDevice(databaseTransaction *gorm.DB, userID string, input DeviceInput, status string) (*models.UserDevice, error) {
	now := time.Now().UTC()
	name := strings.TrimSpace(input.Name)
	if name == "" {
		name = strings.TrimSpace(input.Model)
	}
	if name == "" {
		name = "Phone"
	}
	var device models.UserDevice
	err := databaseTransaction.Where("user_id = ? AND device_imei = ?", userID, input.Imei).Take(&device).Error
	if database.IsNotFound(err) {
		device = models.UserDevice{
			ID: ids.New(), UserID: userID, DeviceImei: input.Imei, Name: name,
			Platform: input.Platform, Model: input.Model, AppVersion: input.AppVersion,
			Status: status, LastSeenAt: &now, CreatedAt: now, UpdatedAt: now,
		}
		if status == models.DeviceStatusActive {
			device.ActivatedAt = &now
		}
		if err := databaseTransaction.Omit("ActiveUserID").Create(&device).Error; err != nil {
			return nil, err
		}
		return &device, nil
	}
	if err != nil {
		return nil, err
	}
	updates := map[string]any{"status": status, "name": name, "platform": input.Platform, "model": input.Model, "app_version": input.AppVersion, "last_seen_at": now, "updated_at": now}
	if status == models.DeviceStatusActive {
		updates["activated_at"] = now
	}
	if err := databaseTransaction.Model(&models.UserDevice{}).Where("id = ?", device.ID).Updates(updates).Error; err != nil {
		return nil, err
	}
	device.Status = status
	device.Name = name
	return &device, nil
}

// startSession creates a session plus a refresh token and an access token.
func (service *Service) startSession(databaseTransaction *gorm.DB, userID string, device *models.UserDevice, ipAddress string, userAgent string) (*TokenPair, error) {
	now := time.Now().UTC()
	session := models.UserSession{
		ID: ids.New(), UserID: userID, DeviceID: device.ID,
		IPAddress: ipAddress, UserAgent: truncate(userAgent, 255),
		LastUsedAt: now, ExpiresAt: now.Add(service.settings.Auth.RefreshTokenLifetime), CreatedAt: now,
	}
	if err := databaseTransaction.Create(&session).Error; err != nil {
		return nil, err
	}
	refreshToken, refreshExpiresAt, err := service.createRefreshToken(databaseTransaction, session.ID)
	if err != nil {
		return nil, err
	}
	accessToken, accessExpiresAt, err := service.toolkit.IssueAccessToken(security.AudienceMobile, userID, session.ID, device.ID, service.settings.Auth.AccessTokenLifetime)
	if err != nil {
		return nil, err
	}
	databaseTransaction.Model(&models.User{}).Where("id = ?", userID).Update("last_active_at", now)
	return &TokenPair{AccessToken: accessToken, AccessTokenExpiresAt: accessExpiresAt, RefreshToken: refreshToken, RefreshTokenExpiresAt: refreshExpiresAt, SessionID: session.ID}, nil
}

// createRefreshToken stores the hash of a new random refresh token.
func (service *Service) createRefreshToken(databaseTransaction *gorm.DB, sessionID string) (string, time.Time, error) {
	token := ids.RandomToken(32)
	expiresAt := time.Now().UTC().Add(service.settings.Auth.RefreshTokenLifetime)
	err := databaseTransaction.Create(&models.RefreshToken{
		ID: ids.New(), SessionID: sessionID, TokenHash: service.toolkit.HashCode(token),
		ExpiresAt: expiresAt, CreatedAt: time.Now().UTC(),
	}).Error
	return token, expiresAt, err
}

// ─── Session lookup for the middleware ───────────────────────────────

// SessionInfo is what the middleware needs to know about a session.
type SessionInfo struct {
	UserID     string `json:"user_id"`
	DeviceID   string `json:"device_id"`
	DeviceImei string `json:"device_imei"`
	IsActive   bool   `json:"is_active"` // false if revoked, expired or the device was replaced
	UserStatus string `json:"user_status"`
}

// sessionCacheKey is the Redis key for a cached session.
func sessionCacheKey(sessionID string) string { return "session:" + sessionID }

// LoadSession returns session info, from Redis when possible (cached for 60
// seconds) or from MySQL. Revoking a session deletes the cache entry, so a
// replaced phone is signed out immediately.
func (service *Service) LoadSession(requestContext context.Context, sessionID string) (*SessionInfo, error) {
	if cached, err := service.redis.Get(requestContext, sessionCacheKey(sessionID)).Bytes(); err == nil {
		var info SessionInfo
		if json.Unmarshal(cached, &info) == nil {
			return &info, nil
		}
	}

	var row struct {
		UserID       string
		DeviceID     string
		DeviceImei   string
		DeviceStatus string
		RevokedAt    *time.Time
		ExpiresAt    time.Time
		UserStatus   string
	}
	err := service.database.WithContext(requestContext).Raw(
		`SELECT sessions.user_id, sessions.device_id, devices.device_imei, devices.status AS device_status,
		        sessions.revoked_at, sessions.expires_at, users.status AS user_status
		   FROM user_sessions sessions
		   JOIN user_devices devices ON devices.id = sessions.device_id
		   JOIN users ON users.id = sessions.user_id
		  WHERE sessions.id = ?`, sessionID,
	).Scan(&row).Error
	if err != nil {
		return nil, err
	}
	if row.UserID == "" {
		return nil, errors.New("session not found")
	}
	info := SessionInfo{
		UserID:     row.UserID,
		DeviceID:   row.DeviceID,
		DeviceImei: row.DeviceImei,
		IsActive:   row.RevokedAt == nil && row.ExpiresAt.After(time.Now().UTC()) && row.DeviceStatus == models.DeviceStatusActive,
		UserStatus: row.UserStatus,
	}
	if encoded, err := json.Marshal(info); err == nil {
		service.redis.Set(requestContext, sessionCacheKey(sessionID), encoded, time.Minute)
	}
	return &info, nil
}

// forgetSessions deletes cached sessions so their next request re-reads MySQL.
func (service *Service) forgetSessions(requestContext context.Context, sessionIDs []string) {
	for _, sessionID := range sessionIDs {
		service.redis.Del(requestContext, sessionCacheKey(sessionID))
	}
}

// ForgetUserSessions clears every cached session of a user (used when an admin freezes them).
func (service *Service) ForgetUserSessions(requestContext context.Context, userID string) {
	var sessionIDs []string
	service.database.WithContext(requestContext).Model(&models.UserSession{}).Where("user_id = ? AND revoked_at IS NULL", userID).Pluck("id", &sessionIDs)
	service.forgetSessions(requestContext, sessionIDs)
}

// ─── Small helpers ───────────────────────────────────────────────────

// PasswordProblem returns "" for a good password, or what is wrong with it.
// Same rules as the app: 8+ characters, one capital letter, one special character.
func PasswordProblem(password string) string {
	if len(password) < 8 {
		return "Use at least 8 characters."
	}
	hasUpper, hasSpecial := false, false
	for _, character := range password {
		if unicode.IsUpper(character) {
			hasUpper = true
		}
		if !unicode.IsLetter(character) && !unicode.IsDigit(character) {
			hasSpecial = true
		}
	}
	if !hasUpper {
		return "Add at least one capital letter."
	}
	if !hasSpecial {
		return "Add at least one special character."
	}
	return ""
}

var tagCharacters = regexp.MustCompile(`[^a-z0-9_.]`)

// normaliseEmail trims and lower-cases an email.
func normaliseEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

// normaliseTag lower-cases a @tag and removes the "@" and odd characters.
func normaliseTag(tag string) string {
	return tagCharacters.ReplaceAllString(strings.ToLower(strings.TrimPrefix(strings.TrimSpace(tag), "@")), "")
}

// newReferralCode builds a code like "ADA-7KQ" (matches the app's format).
func newReferralCode(tag string) string {
	prefix := strings.ToUpper(regexp.MustCompile(`[^a-zA-Z0-9]`).ReplaceAllString(tag, ""))
	if len(prefix) > 6 {
		prefix = prefix[:6]
	}
	if len(prefix) < 2 {
		prefix = "TRIBE"
	}
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	suffix := []byte(ids.RandomToken(3))
	for index := range suffix {
		suffix[index] = alphabet[int(suffix[index])%len(alphabet)]
	}
	return prefix + "-" + string(suffix[:3])
}

// truncate cuts text to a maximum length.
func truncate(text string, maximumLength int) string {
	if len(text) <= maximumLength {
		return text
	}
	return text[:maximumLength]
}
