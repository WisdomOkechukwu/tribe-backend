// Package security holds small helpers for secrets:
//   - hashing passwords and PINs (argon2id: slow on purpose, so stolen
//     hashes are very hard to crack)
//   - hashing one-time codes and tokens (SHA-256 with our APP_KEY)
//   - encrypting data we must be able to read back (AES-256-GCM)
//   - signing and checking access tokens (JWT)
package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/argon2"
)

// Toolkit holds the application key and offers the helpers below.
type Toolkit struct {
	applicationKey []byte

	// hashingSlots limits how many argon2id hashes run at the same time.
	// Each hash needs 64 MB of memory, so 50 sign-ins or PIN checks at once
	// would need over 3 GB and could crash a small server. A buffered
	// channel works like a box of N tickets: a hash takes a ticket before it
	// starts (blocking while the box is empty) and puts it back when done.
	// The hash itself is NOT made weaker; requests just wait their turn.
	hashingSlots chan struct{}
}

// NewToolkit creates the helpers using APP_KEY. hashingConcurrency is the
// most argon2id hashes allowed at once (HASHING_CONCURRENCY, at least 1).
func NewToolkit(applicationKey string, hashingConcurrency int) *Toolkit {
	if hashingConcurrency < 1 {
		hashingConcurrency = 1
	}
	return &Toolkit{applicationKey: []byte(applicationKey), hashingSlots: make(chan struct{}, hashingConcurrency)}
}

// ─── Passwords and PINs ──────────────────────────────────────────────

// argon2id settings: 64 MB memory, 1 pass, 2 threads. Safe defaults (OWASP).
const (
	argonMemoryKilobytes = 64 * 1024
	argonIterations      = 1
	argonThreads         = 2
	argonKeyLength       = 32
)

// argonKey runs argon2id while holding one of the hashing slots, so memory
// use stays at most HASHING_CONCURRENCY x 64 MB however busy the server is.
func (toolkit *Toolkit) argonKey(secret string, salt []byte) []byte {
	toolkit.hashingSlots <- struct{}{}        // take a ticket (waits if none are free)
	defer func() { <-toolkit.hashingSlots }() // give it back, even on panic
	return argon2.IDKey([]byte(secret), salt, argonIterations, argonMemoryKilobytes, argonThreads, argonKeyLength)
}

// HashPassword turns a password (or PIN) into a string safe to store.
// Format: argon2id$<salt in base64>$<hash in base64>
func (toolkit *Toolkit) HashPassword(password string) string {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		panic(err)
	}
	hash := toolkit.argonKey(password, salt)
	return "argon2id$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(hash)
}

// CheckPassword returns true when the password matches the stored hash.
func (toolkit *Toolkit) CheckPassword(storedHash string, password string) bool {
	parts := strings.Split(storedHash, "$")
	if len(parts) != 3 || parts[0] != "argon2id" {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	expected, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	actual := toolkit.argonKey(password, salt)
	// ConstantTimeCompare takes the same time whether the first or last byte
	// differs, so attackers learn nothing from how long the check took.
	return subtle.ConstantTimeCompare(expected, actual) == 1
}

// DummyPasswordCheck burns the same time as a real check. We call it when an
// email does not exist, so attackers can't tell real emails by timing.
func (toolkit *Toolkit) DummyPasswordCheck(password string) {
	toolkit.argonKey(password, []byte("dummy-salt-value"))
}

// ─── Codes and tokens ────────────────────────────────────────────────

// HashCode hashes a short code or token with our key (HMAC-SHA256).
// Codes are short, so a plain SHA-256 could be brute-forced; mixing in the
// secret key prevents that.
func (toolkit *Toolkit) HashCode(code string) string {
	mac := hmac.New(sha256.New, toolkit.applicationKey)
	mac.Write([]byte(code))
	return hex.EncodeToString(mac.Sum(nil))
}

// CodesMatch compares a typed code with a stored hash in constant time.
func (toolkit *Toolkit) CodesMatch(storedHash string, typedCode string) bool {
	return hmac.Equal([]byte(storedHash), []byte(toolkit.HashCode(typedCode)))
}

// HashBody returns the SHA-256 of a request body (used by idempotency).
func HashBody(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// ─── Encryption ──────────────────────────────────────────────────────

// encryptionKey derives a 32-byte AES key from the application key.
func (toolkit *Toolkit) encryptionKey() []byte {
	sum := sha256.Sum256(append([]byte("tribe-encryption:"), toolkit.applicationKey...))
	return sum[:]
}

// Encrypt locks text with AES-256-GCM. The result starts with a random
// "nonce" so encrypting the same text twice gives different results.
func (toolkit *Toolkit) Encrypt(plainText string) ([]byte, error) {
	block, err := aes.NewCipher(toolkit.encryptionKey())
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, []byte(plainText), nil), nil
}

// Decrypt reverses Encrypt.
func (toolkit *Toolkit) Decrypt(cipherText []byte) (string, error) {
	block, err := aes.NewCipher(toolkit.encryptionKey())
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(cipherText) < gcm.NonceSize() {
		return "", errors.New("encrypted value is too short")
	}
	nonce, sealed := cipherText[:gcm.NonceSize()], cipherText[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, sealed, nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// ─── Access tokens (JWT) ─────────────────────────────────────────────

// Audiences: a mobile token can never be used on admin routes and the other way round.
const (
	AudienceMobile = "mobile"
	AudienceAdmin  = "admin"
)

// TokenClaims is what we put inside an access token.
type TokenClaims struct {
	SessionID string `json:"sid"`
	DeviceID  string `json:"did,omitempty"`
	jwt.RegisteredClaims
}

// IssueAccessToken creates a signed token that says "this subject (user or
// admin id) is signed in with this session until expiresAt".
func (toolkit *Toolkit) IssueAccessToken(audience string, subject string, sessionID string, deviceID string, lifetime time.Duration) (string, time.Time, error) {
	now := time.Now()
	expiresAt := now.Add(lifetime)
	claims := TokenClaims{
		SessionID: sessionID,
		DeviceID:  deviceID,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   subject,
			Audience:  jwt.ClaimStrings{audience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			Issuer:    "tribe",
		},
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(toolkit.tokenKey())
	return signed, expiresAt, err
}

// ParseAccessToken checks a token's signature, expiry and audience.
func (toolkit *Toolkit) ParseAccessToken(tokenText string, audience string) (*TokenClaims, error) {
	claims := &TokenClaims{}
	_, err := jwt.ParseWithClaims(tokenText, claims, func(token *jwt.Token) (any, error) {
		return toolkit.tokenKey(), nil
	},
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithAudience(audience),
		jwt.WithIssuer("tribe"),
	)
	if err != nil {
		return nil, fmt.Errorf("invalid token: %w", err)
	}
	return claims, nil
}

// tokenKey derives the token signing key from the application key.
func (toolkit *Toolkit) tokenKey() []byte {
	sum := sha256.Sum256(append([]byte("tribe-tokens:"), toolkit.applicationKey...))
	return sum[:]
}
