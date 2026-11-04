package integration

import (
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/tribe-africa/tribe-backend/tests/testenv"
)

// TestParallelWrongPinsAreCountedBeforeHashing fires 20 wrong PINs at once.
// Each PIN check is a slow 64 MB hash, so attempts must be counted BEFORE
// hashing: at most PIN_MAXIMUM_ATTEMPTS (5) may be checked, the rest must be
// refused as locked, and afterwards even the right PIN is refused.
func TestParallelWrongPinsAreCountedBeforeHashing(test *testing.T) {
	environment := testenv.Start(test)
	sender := environment.RegisterUser(test, "pinburst")
	environment.RegisterUser(test, "pinburstfriend")
	sender.TopUp(test, "USD", "20")
	maximumAttempts := int64(environment.App.Settings.Auth.PinMaximumAttempts)

	var wrongPin, locked atomic.Int64
	var waitGroup sync.WaitGroup
	for attempt := 0; attempt < 20; attempt++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			response := sender.Call(test, http.MethodPost, "/api/mobile/v1/sends", map[string]any{
				"recipient_kind": "tribe", "handle": "@pinburstfriend", "asset": "USD", "amount": "1", "pin": "9871",
			}, testenv.NewKey())
			switch response.Code {
			case "WRONG_PIN":
				wrongPin.Add(1)
			case "PIN_LOCKED":
				locked.Add(1)
			default:
				test.Errorf("unexpected answer: %d %s %s", response.Status, response.Code, response.Message)
			}
		}()
	}
	waitGroup.Wait()

	// The 5th wrong PIN answers PIN_LOCKED, so at most 4 can say WRONG_PIN.
	if wrongPin.Load() > maximumAttempts-1 {
		test.Errorf("%d PINs were hashed and answered WRONG_PIN, want at most %d", wrongPin.Load(), maximumAttempts-1)
	}
	if wrongPin.Load()+locked.Load() != 20 {
		test.Errorf("answers: %d wrong + %d locked, want 20 in total", wrongPin.Load(), locked.Load())
	}
	response := sender.Call(test, http.MethodPost, "/api/mobile/v1/sends", map[string]any{
		"recipient_kind": "tribe", "handle": "@pinburstfriend", "asset": "USD", "amount": "1", "pin": testenv.DemoPin,
	}, testenv.NewKey())
	if response.Code != "PIN_LOCKED" {
		test.Errorf("right PIN after the burst: %s, want PIN_LOCKED", response.Code)
	}
	if balance := sender.Balance(test, "USD"); balance != "20.00" {
		test.Errorf("balance = %s, want 20.00 (nothing may move)", balance)
	}
}

// TestParallelWrongPasswordsAreCountedBeforeHashing fires 25 wrong
// passwords for one email at once. Only 10 attempts per 15 minutes may reach
// the password hash; the rest must be refused straight away.
func TestParallelWrongPasswordsAreCountedBeforeHashing(test *testing.T) {
	environment := testenv.Start(test)
	user := environment.RegisterUser(test, "loginburst")
	phone := environment.NewClient("loginburst-phone")

	var invalid, tooMany atomic.Int64
	var waitGroup sync.WaitGroup
	for attempt := 0; attempt < 25; attempt++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			response := phone.Call(test, http.MethodPost, "/api/mobile/v1/auth/login", map[string]any{
				"email": user.Email, "password": "Wrong@2026", "device": map[string]string{"imei": "loginburst-phone"},
			}, testenv.NewKey())
			switch response.Code {
			case "INVALID_CREDENTIALS":
				invalid.Add(1)
			case "TOO_MANY_ATTEMPTS":
				tooMany.Add(1)
			default:
				test.Errorf("unexpected answer: %d %s %s", response.Status, response.Code, response.Message)
			}
		}()
	}
	waitGroup.Wait()

	if invalid.Load() != 10 || tooMany.Load() != 15 {
		test.Errorf("answers: %d wrong password + %d too many, want exactly 10 + 15", invalid.Load(), tooMany.Load())
	}
}
