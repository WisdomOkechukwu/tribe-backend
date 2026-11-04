package auth

import (
	"context"
	"net/http"
	"regexp"
	"time"

	"gorm.io/gorm"

	"github.com/tribe-africa/tribe-backend/app/apperrors"
	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/database"
)

var fourDigits = regexp.MustCompile(`^[0-9]{4}$`)

// weakPins are refused when creating a PIN (same idea as the app).
var weakPins = map[string]bool{"0000": true, "1111": true, "1234": true, "4321": true, "2222": true, "9999": true, "1212": true}

// SetPin creates the payment PIN the first time.
func (service *Service) SetPin(requestContext context.Context, userID string, pin string) error {
	if !fourDigits.MatchString(pin) || weakPins[pin] {
		return apperrors.Validation(map[string][]string{"pin": {"Choose 4 digits that are not easy to guess."}})
	}
	err := service.database.WithContext(requestContext).Create(&models.UserPin{
		UserID: userID, PinHash: service.toolkit.HashPassword(pin), UpdatedAt: time.Now().UTC(),
	}).Error
	if database.IsDuplicateEntry(err) {
		return apperrors.New(http.StatusConflict, "PIN_EXISTS", "You already have a PIN. Use change PIN instead.")
	}
	return err
}

// ChangePin replaces the PIN after checking the current one.
func (service *Service) ChangePin(requestContext context.Context, userID string, currentPin string, newPin string) error {
	if err := service.VerifyPin(requestContext, userID, currentPin); err != nil {
		return err
	}
	if !fourDigits.MatchString(newPin) || weakPins[newPin] {
		return apperrors.Validation(map[string][]string{"new_pin": {"Choose 4 digits that are not easy to guess."}})
	}
	return service.database.WithContext(requestContext).Model(&models.UserPin{}).Where("user_id = ?", userID).
		Updates(map[string]any{"pin_hash": service.toolkit.HashPassword(newPin), "failed_attempts": 0, "locked_until": nil, "updated_at": time.Now().UTC()}).Error
}

// VerifyPin checks the PIN sent with a money request.
//
// The attempt is COUNTED before the slow hash, in one atomic UPDATE that
// only succeeds while the PIN is not locked and attempts are left:
//
//	UPDATE user_pins SET failed_attempts = failed_attempts + 1
//	 WHERE user_id = ? AND failed_attempts < 5 AND not locked
//
// MySQL runs that UPDATE one row-change at a time, so if 50 PINs arrive at
// once only the first 5 get a "ticket" and are hashed. (Checking the lock
// first and counting afterwards would let all 50 hash.) The others WAIT
// for a ticket, a few milliseconds at a time:
//   - if those 5 were wrong, the 5th locks the PIN and every waiting
//     request is refused as locked, without hashing;
//   - if they were right, each one resets the count to zero, which frees
//     tickets, so a burst of genuine payments all goes through.
//
// Call this BEFORE the money transaction: the failure count must be saved
// even though the money request itself is refused.
func (service *Service) VerifyPin(requestContext context.Context, userID string, pin string) error {
	var stored models.UserPin
	err := service.database.WithContext(requestContext).Where("user_id = ?", userID).Take(&stored).Error
	if database.IsNotFound(err) {
		return apperrors.ErrPinNotSet
	}
	if err != nil {
		return err
	}
	maximumAttempts := service.settings.Auth.PinMaximumAttempts

	// 1. Take a ticket: count this attempt now, before hashing.
	if err := service.takePinTicket(requestContext, userID, maximumAttempts); err != nil {
		return err
	}
	now := time.Now().UTC()

	// 2. The slow hash (also limited by the toolkit's hashing slots).
	if service.toolkit.CheckPassword(stored.PinHash, pin) {
		// Correct: forget the failures, including this attempt's ticket.
		return service.database.WithContext(requestContext).Model(&models.UserPin{}).Where("user_id = ?", userID).
			Updates(map[string]any{"failed_attempts": 0, "locked_until": nil}).Error
	}

	// 3. Wrong: the attempt is already counted. If that was the last one,
	// lock the PIN and start the count again from zero for after the lock.
	// The WHERE makes this a single atomic step, so it locks exactly once.
	locked := service.database.WithContext(requestContext).Exec(
		`UPDATE user_pins
		    SET locked_until = ?, failed_attempts = 0
		  WHERE user_id = ? AND failed_attempts >= ?`,
		now.Add(service.settings.Auth.PinLockDuration), userID, maximumAttempts)
	if locked.Error != nil {
		return locked.Error
	}
	if locked.RowsAffected > 0 {
		return apperrors.ErrPinLocked
	}
	return apperrors.ErrWrongPin
}

// pinTicketWait is the longest a PIN check waits for a free ticket.
const pinTicketWait = 5 * time.Second

// takePinTicket counts one PIN attempt before it is checked (see VerifyPin).
// It returns ErrPinLocked when the PIN is really locked, and waits while
// every ticket is taken by checks that are still running.
func (service *Service) takePinTicket(requestContext context.Context, userID string, maximumAttempts int) error {
	giveUpAt := time.Now().Add(pinTicketWait)
	for {
		now := time.Now().UTC()
		reserved := service.database.WithContext(requestContext).Exec(
			`UPDATE user_pins
			    SET failed_attempts = failed_attempts + 1
			  WHERE user_id = ?
			    AND failed_attempts < ?
			    AND (locked_until IS NULL OR locked_until <= ?)`, userID, maximumAttempts, now)
		if reserved.Error != nil {
			return reserved.Error
		}
		if reserved.RowsAffected == 1 {
			return nil // got a ticket
		}
		// No ticket: is the PIN locked, or are other checks still running?
		var stored models.UserPin
		if err := service.database.WithContext(requestContext).Where("user_id = ?", userID).Take(&stored).Error; err != nil {
			return err
		}
		if stored.LockedUntil != nil && stored.LockedUntil.After(now) {
			return apperrors.ErrPinLocked
		}
		if time.Now().After(giveUpAt) {
			return apperrors.New(http.StatusTooManyRequests, "PIN_BUSY", "Too many payments at once. Please try again.")
		}
		select {
		case <-requestContext.Done():
			return requestContext.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// HasPin tells the app whether the user has created a PIN.
func (service *Service) HasPin(query *gorm.DB, userID string) bool {
	var count int64
	query.Model(&models.UserPin{}).Where("user_id = ?", userID).Count(&count)
	return count > 0
}
