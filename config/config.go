// Package config reads settings from the .env file and environment
// variables, exactly like Laravel's config() helper reads .env.
//
// Every setting has a sensible default for local development, so a fresh
// checkout works with only DB_* values filled in.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// Config holds every setting the application needs.
type Config struct {
	App      AppConfig
	Database DatabaseConfig
	Redis    RedisConfig
	Rates    RatesConfig
	Auth     AuthConfig
}

// AppConfig holds general application settings.
type AppConfig struct {
	Name           string
	Environment    string // "local", "testing" or "production"
	Key            string // secret used to sign tokens and encrypt data
	Port           string
	AllowedOrigins []string // websites allowed to call the API from a browser (the admin panel)
	TrustedProxies []string // load balancer addresses allowed to set X-Forwarded-For (empty = none)
	RateLimitsOff  bool     // tests and load tests only (RATE_LIMITS_OFF); refused in production
	TimeZone       *time.Location
	MemoryLimitMB  int // soft memory limit for the Go runtime (MEMORY_LIMIT_MB, 0 = none)

	RequestBodyMaxBytes    int64         // bigger request bodies are refused with 413 (REQUEST_BODY_MAX_BYTES)
	RequestBodyReadTimeout time.Duration // a body must arrive within this time (REQUEST_BODY_TIMEOUT_SECONDS)
	ApiLogRetention        time.Duration // api_request_logs older than this are deleted (API_LOG_RETENTION_DAYS)
}

// DatabaseConfig holds the MySQL connection settings (same names as Laravel).
type DatabaseConfig struct {
	Host     string
	Port     string
	Database string
	Username string
	Password string
	Socket   string // when set (MAMP), we connect through this file instead of host:port

	// Connection pool, per program (server, worker and scheduler each have
	// one). Keep MaxOpenConnections x 3 below the MySQL server's
	// max_connections, especially on a small managed MySQL.
	MaxOpenConnections int           // DB_MAX_OPEN_CONNECTIONS
	MaxIdleConnections int           // DB_MAX_IDLE_CONNECTIONS
	ConnectionLifetime time.Duration // DB_CONNECTION_LIFETIME_SECONDS: replace connections this old
	ConnectionIdleTime time.Duration // DB_CONNECTION_IDLE_SECONDS: close connections unused this long

	// Network timeouts, so a stalled MySQL on another server makes requests
	// fail instead of hang. Ignored for the MAMP socket connection.
	ConnectTimeout time.Duration // DB_CONNECT_TIMEOUT_SECONDS
	ReadTimeout    time.Duration // DB_READ_TIMEOUT_SECONDS: longest wait for one answer
	WriteTimeout   time.Duration // DB_WRITE_TIMEOUT_SECONDS
}

// RedisConfig holds the Redis connection settings (same names as Laravel).
type RedisConfig struct {
	Host     string
	Port     string
	Password string
}

// RatesConfig controls the live exchange rate collector.
type RatesConfig struct {
	RefreshEvery       time.Duration // how often the collector asks providers for new rates
	StaleAfter         time.Duration // rates older than this cannot be used for quotes
	QuoteLifetime      time.Duration // how long a price quote stays valid
	NairaPerDollarSeed string        // starting NGN/USD rate for the simulator
}

// AuthConfig controls tokens, codes and the PIN lock-out.
type AuthConfig struct {
	AccessTokenLifetime  time.Duration
	RefreshTokenLifetime time.Duration
	AdminSessionLifetime time.Duration
	CodeLifetime         time.Duration
	PinMaximumAttempts   int
	PinLockDuration      time.Duration
	HashingConcurrency   int // most argon2id hashes (64 MB each) running at once in one program
}

// IsProduction is true when APP_ENV=production.
func (configuration Config) IsProduction() bool {
	return configuration.App.Environment == "production"
}

// IsLocal is true when APP_ENV=local (developer machine).
func (configuration Config) IsLocal() bool {
	return configuration.App.Environment == "local"
}

// Load reads .env (if it exists) and then the real environment, which wins.
func Load() (Config, error) {
	// godotenv does not overwrite variables that are already set, so real
	// environment variables always win over the .env file.
	_ = godotenv.Load()

	lagos, err := time.LoadLocation("Africa/Lagos")
	if err != nil {
		// Some minimal servers have no time zone database; Lagos is always UTC+1.
		lagos = time.FixedZone("WAT", 60*60)
	}

	configuration := Config{
		App: AppConfig{
			Name:           readString("APP_NAME", "Tribe"),
			Environment:    readString("APP_ENV", "local"),
			Key:            readString("APP_KEY", ""),
			Port:           readString("APP_PORT", "8080"),
			AllowedOrigins: readList("CORS_ALLOWED_ORIGINS", "http://localhost:3000"),
			TrustedProxies: readList("TRUSTED_PROXIES", ""),
			RateLimitsOff:  readString("RATE_LIMITS_OFF", "false") == "true",
			TimeZone:       lagos,
			MemoryLimitMB:  readInteger("MEMORY_LIMIT_MB", 0),

			RequestBodyMaxBytes:    int64(readInteger("REQUEST_BODY_MAX_BYTES", 256*1024)),
			RequestBodyReadTimeout: readSeconds("REQUEST_BODY_TIMEOUT_SECONDS", 15),
			ApiLogRetention:        time.Duration(readInteger("API_LOG_RETENTION_DAYS", 90)) * 24 * time.Hour,
		},
		Database: DatabaseConfig{
			Host:     readString("DB_HOST", "127.0.0.1"),
			Port:     readString("DB_PORT", "3306"),
			Database: readString("DB_DATABASE", "tribe_core"),
			Username: readString("DB_USERNAME", "root"),
			Password: readString("DB_PASSWORD", ""),
			Socket:   readString("DB_SOCKET", ""),

			MaxOpenConnections: readInteger("DB_MAX_OPEN_CONNECTIONS", 25),
			MaxIdleConnections: readInteger("DB_MAX_IDLE_CONNECTIONS", 10),
			ConnectionLifetime: readSeconds("DB_CONNECTION_LIFETIME_SECONDS", 30*60),
			ConnectionIdleTime: readSeconds("DB_CONNECTION_IDLE_SECONDS", 5*60),
			ConnectTimeout:     readSeconds("DB_CONNECT_TIMEOUT_SECONDS", 5),
			ReadTimeout:        readSeconds("DB_READ_TIMEOUT_SECONDS", 60),
			WriteTimeout:       readSeconds("DB_WRITE_TIMEOUT_SECONDS", 30),
		},
		Redis: RedisConfig{
			Host:     readString("REDIS_HOST", "127.0.0.1"),
			Port:     readString("REDIS_PORT", "6379"),
			Password: readNullableString("REDIS_PASSWORD"),
		},
		Rates: RatesConfig{
			RefreshEvery:       readSeconds("RATE_REFRESH_SECONDS", 10),
			StaleAfter:         readSeconds("RATE_STALE_SECONDS", 120),
			QuoteLifetime:      readSeconds("QUOTE_LIFETIME_SECONDS", 30),
			NairaPerDollarSeed: readString("RATE_SIMULATOR_NGN_PER_USD", "1540"),
		},
		Auth: AuthConfig{
			AccessTokenLifetime:  readSeconds("ACCESS_TOKEN_SECONDS", 15*60),
			RefreshTokenLifetime: readSeconds("REFRESH_TOKEN_SECONDS", 30*24*60*60),
			AdminSessionLifetime: readSeconds("ADMIN_SESSION_SECONDS", 12*60*60),
			CodeLifetime:         readSeconds("VERIFICATION_CODE_SECONDS", 10*60),
			PinMaximumAttempts:   readInteger("PIN_MAXIMUM_ATTEMPTS", 5),
			PinLockDuration:      readSeconds("PIN_LOCK_SECONDS", 15*60),
			HashingConcurrency:   readInteger("HASHING_CONCURRENCY", 4),
		},
	}

	if configuration.App.RateLimitsOff && configuration.IsProduction() {
		return configuration, fmt.Errorf("RATE_LIMITS_OFF is only for tests and load tests; it is not allowed in production")
	}
	if len(configuration.App.Key) < 32 {
		if configuration.IsProduction() {
			return configuration, fmt.Errorf("APP_KEY must be at least 32 characters in production")
		}
		// A fixed development key so local tokens survive restarts. Never used in production.
		configuration.App.Key = "local-development-key-change-me-0123456789"
	}
	return configuration, nil
}

// readString returns the variable or the default when it is empty.
func readString(name string, defaultValue string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return defaultValue
	}
	return value
}

// readNullableString treats Laravel's "null" as an empty value.
func readNullableString(name string) string {
	value := readString(name, "")
	if strings.EqualFold(value, "null") {
		return ""
	}
	return value
}

// readInteger returns the variable as a number, or the default.
func readInteger(name string, defaultValue int) int {
	number, err := strconv.Atoi(readString(name, ""))
	if err != nil {
		return defaultValue
	}
	return number
}

// readSeconds returns the variable (a number of seconds) as a duration.
func readSeconds(name string, defaultSeconds int) time.Duration {
	return time.Duration(readInteger(name, defaultSeconds)) * time.Second
}

// readList splits a comma separated variable into a list.
func readList(name string, defaultValue string) []string {
	parts := strings.Split(readString(name, defaultValue), ",")
	list := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			list = append(list, trimmed)
		}
	}
	return list
}
