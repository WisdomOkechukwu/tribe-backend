// Package apperrors defines the one error type our services return when
// something is the CALLER's fault (bad input, not enough money, wrong PIN).
//
// A service returns, for example:
//
//	return apperrors.New(http.StatusUnprocessableEntity, "INSUFFICIENT_FUNDS", "You don't have enough USD.")
//
// and the controller turns it into a JSON error response automatically.
// Any other error is treated as a server bug and returns a generic 500.
package apperrors

import (
	"errors"
	"net/http"
)

// AppError is an error with an HTTP status, a machine-readable code and a
// friendly message the app can show to the user.
type AppError struct {
	Status      int
	Code        string
	Message     string
	FieldErrors map[string][]string // per-field messages for forms (optional)
}

// Error makes AppError satisfy Go's built-in error interface.
func (appError *AppError) Error() string {
	return appError.Code + ": " + appError.Message
}

// New creates an AppError.
func New(status int, code string, message string) *AppError {
	return &AppError{Status: status, Code: code, Message: message}
}

// Validation creates a 422 error with messages for each invalid field.
func Validation(fieldErrors map[string][]string) *AppError {
	return &AppError{
		Status:      http.StatusUnprocessableEntity,
		Code:        "VALIDATION_FAILED",
		Message:     "Please check the highlighted fields.",
		FieldErrors: fieldErrors,
	}
}

// NotFound creates a 404 error, e.g. NotFound("Card").
func NotFound(thing string) *AppError {
	return New(http.StatusNotFound, "NOT_FOUND", thing+" not found.")
}

// As pulls an AppError out of any error (or returns nil if it is not one).
func As(err error) *AppError {
	var appError *AppError
	if errors.As(err, &appError) {
		return appError
	}
	return nil
}

// Commonly used errors.
var (
	ErrUnauthenticated   = New(http.StatusUnauthorized, "UNAUTHENTICATED", "Please sign in again.")
	ErrDeviceNotActive   = New(http.StatusUnauthorized, "DEVICE_NOT_ACTIVE", "You signed in on another phone. Sign in again to use Tribe here.")
	ErrForbidden         = New(http.StatusForbidden, "FORBIDDEN", "You don't have permission to do that.")
	ErrInsufficientFunds = New(http.StatusUnprocessableEntity, "INSUFFICIENT_FUNDS", "You don't have enough balance for this.")
	ErrAccountFrozen     = New(http.StatusForbidden, "ACCOUNT_FROZEN", "This account is frozen. Please contact support.")
	ErrStaleData         = New(http.StatusConflict, "STALE_DATA", "Someone else changed this a moment ago. Refresh and try again.")
	ErrQuoteExpired      = New(http.StatusConflict, "QUOTE_EXPIRED", "This price has expired. Please review the new price.")
	ErrWrongPin          = New(http.StatusUnprocessableEntity, "WRONG_PIN", "That PIN is not correct.")
	ErrPinLocked         = New(http.StatusTooManyRequests, "PIN_LOCKED", "Too many wrong PINs. Try again later.")
	ErrPinNotSet         = New(http.StatusUnprocessableEntity, "PIN_NOT_SET", "Create a payment PIN first.")
)
