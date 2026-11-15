package cache

import (
	"context"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"github.com/tribe-africa/tribe-backend/app/models"
)

// configVersionKey is a Redis counter that goes up every time an admin
// changes a fee, a margin, a provider or a service switch (or the seeder
// changes assets).
const configVersionKey = "config:version"

// configCheckEvery is how often a program asks Redis whether the
// configuration changed. Changes are seen everywhere within about this long.
const configCheckEvery = time.Second

// configMaximumAge is the longest a copy is used if Redis cannot be reached
// to ask about changes. After that it is read from MySQL again anyway.
const configMaximumAge = 30 * time.Second

// configRefreshAnyway reloads even when the version did not change: a
// backstop in case a change was saved but bumping the counter failed.
const configRefreshAnyway = 5 * time.Minute

// ConfigSnapshot is one consistent copy of the small, rarely-changing
// tables every money request reads: assets, providers, rate margins,
// active fee rules and the service switches. Treat it as READ-ONLY: it is
// shared by every request.
//
// Deliberately NOT here: exchange_rate_latest (prices change every 10
// seconds and quotes check how old they are) and anything about balances,
// which always come from the ledger.
type ConfigSnapshot struct {
	Assets    map[string]models.Asset          // by code, enabled or not
	Providers map[string]models.Provider       // by id
	Margins   map[string]models.RateMargin     // by asset code
	Fees      map[string]models.TransactionFee // ACTIVE rules, by transaction type
	Services  map[string]models.Service        // on/off switches, by service code (no row = enabled)
}

// ConfigCache keeps one ConfigSnapshot per program (API server, worker,
// scheduler) and replaces it when the configuration changes.
//
// How a change reaches every program, reliably:
//  1. The admin edit commits in MySQL.
//  2. It then calls ConfigChanged, which adds 1 to config:version in Redis.
//  3. Every program compares that number with the one it loaded, at most
//     once a second, and reloads from MySQL when it differs.
//
// A plain counter (not a pub/sub message) cannot be "missed": a program
// that was busy or restarting still sees the new number next time it looks.
// If Redis is down, copies are simply reloaded every 30 seconds.
type ConfigCache struct {
	database *gorm.DB
	redis    *redis.Client

	// mutex protects the three fields below. A request holds it only while
	// it reads or swaps the pointer, or while ONE request reloads; others
	// wait for that reload instead of all hitting MySQL at once.
	mutex     sync.Mutex
	snapshot  *ConfigSnapshot
	version   string    // config:version when snapshot was loaded
	loadedAt  time.Time // when snapshot was read from MySQL
	checkedAt time.Time // when we last asked Redis for the version
}

// NewConfigCache creates an empty cache; the first Snapshot call fills it.
func NewConfigCache(database *gorm.DB, redisClient *redis.Client) *ConfigCache {
	return &ConfigCache{database: database, redis: redisClient}
}

// Snapshot returns the current configuration, reloading it from MySQL if
// it changed (or is older than 30 seconds and Redis cannot tell us).
func (configCache *ConfigCache) Snapshot(requestContext context.Context) (*ConfigSnapshot, error) {
	if requestContext == nil {
		requestContext = context.Background()
	}
	configCache.mutex.Lock()
	defer configCache.mutex.Unlock()

	now := time.Now()
	if configCache.snapshot != nil && now.Sub(configCache.checkedAt) < configCheckEvery {
		return configCache.snapshot, nil // checked less than a second ago
	}
	currentVersion, err := configCache.redis.Get(requestContext, configVersionKey).Result()
	redisAnswered := err == nil || err == redis.Nil
	if err == redis.Nil {
		currentVersion = "0"
	}
	configCache.checkedAt = now
	if configCache.snapshot != nil && redisAnswered && currentVersion == configCache.version && now.Sub(configCache.loadedAt) < configRefreshAnyway {
		return configCache.snapshot, nil // nothing changed
	}
	if configCache.snapshot != nil && !redisAnswered && now.Sub(configCache.loadedAt) < configMaximumAge {
		return configCache.snapshot, nil // Redis is down; the copy is still young
	}

	snapshot, err := loadConfigSnapshot(configCache.database.WithContext(requestContext))
	if err != nil {
		return nil, err
	}
	configCache.snapshot, configCache.version, configCache.loadedAt = snapshot, currentVersion, now
	return snapshot, nil
}

// ConfigChanged tells every program to reload the configuration. Call it
// AFTER the change has committed, so nobody reloads the old values.
func (configCache *ConfigCache) ConfigChanged(requestContext context.Context) {
	configCache.redis.Incr(requestContext, configVersionKey)
	// Also forget our own copy now, in case Redis is unreachable.
	configCache.mutex.Lock()
	configCache.snapshot = nil
	configCache.mutex.Unlock()
}

// loadConfigSnapshot reads the five configuration tables from MySQL.
func loadConfigSnapshot(query *gorm.DB) (*ConfigSnapshot, error) {
	snapshot := &ConfigSnapshot{
		Assets: map[string]models.Asset{}, Providers: map[string]models.Provider{},
		Margins: map[string]models.RateMargin{}, Fees: map[string]models.TransactionFee{},
		Services: map[string]models.Service{},
	}
	var assets []models.Asset
	if err := query.Find(&assets).Error; err != nil {
		return nil, err
	}
	for _, asset := range assets {
		snapshot.Assets[asset.Code] = asset
	}
	var providerRows []models.Provider
	if err := query.Find(&providerRows).Error; err != nil {
		return nil, err
	}
	for _, provider := range providerRows {
		snapshot.Providers[provider.ID] = provider
	}
	var margins []models.RateMargin
	if err := query.Find(&margins).Error; err != nil {
		return nil, err
	}
	for _, margin := range margins {
		snapshot.Margins[margin.AssetCode] = margin
	}
	var fees []models.TransactionFee
	if err := query.Where("active = ?", true).Find(&fees).Error; err != nil {
		return nil, err
	}
	for _, fee := range fees {
		snapshot.Fees[fee.TransactionType] = fee
	}
	var serviceRows []models.Service
	if err := query.Find(&serviceRows).Error; err != nil {
		return nil, err
	}
	for _, serviceRow := range serviceRows {
		snapshot.Services[serviceRow.Code] = serviceRow
	}
	return snapshot, nil
}
