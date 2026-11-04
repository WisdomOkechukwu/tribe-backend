// Package testenv starts a complete Tribe API for integration tests.
//
// Each call to Start creates a brand-new MySQL database called
// tribe_core_test_<random>, runs every migration, seeds it, starts the real
// router in an in-memory HTTP server and drops the database when the test
// ends. Tests talk to it over HTTP exactly like the mobile app does.
//
// Run DB tests one package at a time (`go test -p 1 ./...` or `make test`):
// the MAMP MySQL server runs out of file handles when many ~100-table test
// databases are created at once.
package testenv

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"

	"github.com/tribe-africa/tribe-backend/app/bootstrap"
	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/app/services/shared"
	"github.com/tribe-africa/tribe-backend/config"
	"github.com/tribe-africa/tribe-backend/database"
	"github.com/tribe-africa/tribe-backend/database/seeders"
	"github.com/tribe-africa/tribe-backend/routes"
)

// DemoPin is the PIN every test user gets.
const DemoPin = "2580"

// Environment is one running API with its own database.
type Environment struct {
	App    *bootstrap.App
	Server *httptest.Server
	runID  string // makes emails unique, because Redis (code limits) is shared between runs
}

// Start creates the database, migrates, seeds and starts the API.
// Everything is cleaned up automatically when the test finishes.
func Start(test *testing.T) *Environment {
	test.Helper()
	if os.Getenv("SKIP_DATABASE_TESTS") != "" {
		test.Skip("SKIP_DATABASE_TESTS is set")
	}
	loadDotEnv()
	settings, err := config.Load()
	if err != nil {
		test.Fatalf("config: %v", err)
	}
	settings.App.Environment = "local" // tests read debug codes from responses
	settings.App.RateLimitsOff = true  // every test user signs up from 127.0.0.1
	settings.Database.Database = "tribe_core_test_" + randomHex(6)

	if err := database.CreateDatabaseIfMissing(settings.Database); err != nil {
		test.Fatalf("create test database: %v", err)
	}
	test.Cleanup(func() {
		if err := database.DropDatabase(settings.Database); err != nil {
			test.Logf("could not drop %s: %v", settings.Database.Database, err)
		}
	})

	application, err := bootstrap.New(settings)
	if err != nil {
		test.Fatalf("bootstrap: %v", err)
	}
	test.Cleanup(application.Close)
	if err := database.Migrate(application.Database); err != nil {
		test.Fatalf("migrate: %v", err)
	}
	if err := seeders.Run(context.Background(), application); err != nil {
		test.Fatalf("seed: %v", err)
	}

	gin.SetMode(gin.TestMode) // no route list printed for every test
	server := httptest.NewServer(routes.Build(application))
	test.Cleanup(server.Close)
	return &Environment{App: application, Server: server, runID: randomHex(4)}
}

// Response is a decoded API envelope.
type Response struct {
	Status  int
	Header  http.Header
	Success bool                `json:"success"`
	Message string              `json:"message"`
	Code    string              `json:"code"`
	Data    json.RawMessage     `json:"data"`
	Errors  map[string][]string `json:"errors"` // per-field messages (validation, retry_after)
}

// Decode reads the data part of the response into destination.
func (response *Response) Decode(test *testing.T, destination any) {
	test.Helper()
	if err := json.Unmarshal(response.Data, destination); err != nil {
		test.Fatalf("decode %s: %v", string(response.Data), err)
	}
}

// Client calls the API as one phone (mobile) or one admin.
type Client struct {
	environment *Environment
	Token       string
	DeviceImei  string
	Email       string // set by RegisterUser
}

// NewClient creates a client with no token yet.
func (environment *Environment) NewClient(deviceImei string) *Client {
	return &Client{environment: environment, DeviceImei: deviceImei}
}

// Call sends one request. idempotencyKey may be "" for reads.
// It is safe to call from many goroutines at once (the concurrency tests do).
func (client *Client) Call(test *testing.T, method string, path string, body any, idempotencyKey string) *Response {
	test.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			test.Fatalf("encode body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, client.environment.Server.URL+path, reader)
	if err != nil {
		test.Fatalf("new request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	if client.Token != "" {
		request.Header.Set("Authorization", "Bearer "+client.Token)
	}
	if client.DeviceImei != "" {
		request.Header.Set("X-Device-Imei", client.DeviceImei)
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	httpResponse, err := http.DefaultClient.Do(request)
	if err != nil {
		test.Errorf("%s %s: %v", method, path, err)
		return &Response{Status: 0}
	}
	defer httpResponse.Body.Close()
	raw, _ := io.ReadAll(httpResponse.Body)
	response := &Response{Status: httpResponse.StatusCode, Header: httpResponse.Header}
	if err := json.Unmarshal(raw, response); err != nil {
		test.Errorf("%s %s: response is not JSON: %s", method, path, string(raw))
	}
	return response
}

// NewKey returns a fresh Idempotency-Key.
func NewKey() string {
	return "test-" + randomHex(12)
}

// RegisterUser signs a new customer up on a new phone, creates their PIN and
// returns a client for that phone. tag must be unique within the test.
func (environment *Environment) RegisterUser(test *testing.T, tag string) *Client {
	test.Helper()
	client := environment.NewClient("phone-" + tag + "-" + randomHex(4))
	email := tag + "." + environment.runID + "@example.com"
	client.Email = email

	sent := client.Call(test, http.MethodPost, "/api/mobile/v1/auth/email/send-code", map[string]string{"email": email}, "")
	var codeData struct {
		DebugCode string `json:"debug_code"`
	}
	sent.Decode(test, &codeData)

	verified := client.Call(test, http.MethodPost, "/api/mobile/v1/auth/email/verify", map[string]string{"email": email, "code": codeData.DebugCode}, "")
	var tokenData struct {
		VerificationToken string `json:"verification_token"`
	}
	verified.Decode(test, &tokenData)

	registered := client.Call(test, http.MethodPost, "/api/mobile/v1/auth/register", map[string]any{
		"email": email, "verification_token": tokenData.VerificationToken, "password": "Tribe@2026",
		"first_name": "Test", "last_name": "User", "tag": tag, "country": "NG",
		"device": map[string]string{"imei": client.DeviceImei, "name": "Test phone", "platform": "ios"},
	}, "")
	if registered.Status != http.StatusCreated && registered.Status != http.StatusOK {
		test.Fatalf("register %s: %d %s %s", tag, registered.Status, registered.Code, registered.Message)
	}
	var registration struct {
		Tokens struct {
			AccessToken string `json:"access_token"`
		} `json:"tokens"`
	}
	registered.Decode(test, &registration)
	client.Token = registration.Tokens.AccessToken

	if pin := client.Call(test, http.MethodPost, "/api/mobile/v1/pin", map[string]string{"pin": DemoPin}, NewKey()); !pin.Success {
		test.Fatalf("create PIN: %s", pin.Message)
	}
	return client
}

// TopUp adds test money to the client's wallet. There is no HTTP route for
// test money any more, so it calls the wallets service directly (the same
// real ledger posting the seeders use).
func (client *Client) TopUp(test *testing.T, asset string, amount string) {
	test.Helper()
	application := client.environment.App
	var user models.User
	if err := application.Database.Where("email = ?", client.Email).Take(&user).Error; err != nil {
		test.Fatalf("top-up: find user %s: %v", client.Email, err)
	}
	if _, err := application.Wallets.TestTopUp(context.Background(), shared.Actor{UserID: user.ID}, asset, amount, "TOP-"+NewKey()); err != nil {
		test.Fatalf("top-up: %v", err)
	}
}

// Balance returns the client's balance of one asset, e.g. "100.00".
func (client *Client) Balance(test *testing.T, asset string) string {
	test.Helper()
	response := client.Call(test, http.MethodGet, "/api/mobile/v1/wallets", nil, "")
	var wallets struct {
		Balances map[string]string `json:"balances"` // asset code -> balance
	}
	response.Decode(test, &wallets)
	if balance, found := wallets.Balances[asset]; found {
		return balance
	}
	return "0"
}

// SignInAdmin returns a client signed in as a seeded admin (password Tribe@2026).
func (environment *Environment) SignInAdmin(test *testing.T, email string) *Client {
	test.Helper()
	client := environment.NewClient("")
	response := client.Call(test, http.MethodPost, "/admin-api/sessions", map[string]string{"email": email, "password": "Tribe@2026"}, "")
	var session struct {
		AccessToken string `json:"access_token"`
	}
	response.Decode(test, &session)
	if session.AccessToken == "" {
		test.Fatalf("admin sign-in %s failed: %s", email, response.Message)
	}
	client.Token = session.AccessToken
	return client
}

// loadDotEnv loads the project's .env (tests run inside package folders,
// so we walk up to the folder that holds go.mod). Real environment
// variables still win, which is how CI points tests at its own MySQL.
func loadDotEnv() {
	directory, err := os.Getwd()
	if err != nil {
		return
	}
	for directory != "/" {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			_ = godotenv.Load(filepath.Join(directory, ".env"))
			return
		}
		directory = filepath.Dir(directory)
	}
}

// randomHex returns n random bytes as lowercase hex.
func randomHex(byteCount int) string {
	buffer := make([]byte, byteCount)
	_, _ = rand.Read(buffer)
	return strings.ToLower(hex.EncodeToString(buffer))
}
