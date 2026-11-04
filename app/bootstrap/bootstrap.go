// Package bootstrap builds the whole application once: it opens MySQL and
// Redis and creates every service. Think of it as Laravel's service
// container, but written out by hand so you can SEE what depends on what.
//
// The API server, the worker, the scheduler and artisan all call New().
package bootstrap

import (
	"runtime/debug"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"github.com/tribe-africa/tribe-backend/app/cache"
	"github.com/tribe-africa/tribe-backend/app/events"
	"github.com/tribe-africa/tribe-backend/app/jobs"
	"github.com/tribe-africa/tribe-backend/app/services/account"
	"github.com/tribe-africa/tribe-backend/app/services/adminauth"
	"github.com/tribe-africa/tribe-backend/app/services/apitrail"
	"github.com/tribe-africa/tribe-backend/app/services/auth"
	"github.com/tribe-africa/tribe-backend/app/services/backoffice"
	"github.com/tribe-africa/tribe-backend/app/services/idempotency"
	"github.com/tribe-africa/tribe-backend/app/services/ledger"
	"github.com/tribe-africa/tribe-backend/app/services/products"
	"github.com/tribe-africa/tribe-backend/app/services/providers"
	"github.com/tribe-africa/tribe-backend/app/services/rates"
	"github.com/tribe-africa/tribe-backend/app/services/security"
	"github.com/tribe-africa/tribe-backend/app/services/shared"
	"github.com/tribe-africa/tribe-backend/app/services/swaps"
	"github.com/tribe-africa/tribe-backend/app/services/transfers"
	"github.com/tribe-africa/tribe-backend/app/services/wallets"
	"github.com/tribe-africa/tribe-backend/app/services/webhooks"
	"github.com/tribe-africa/tribe-backend/config"
	"github.com/tribe-africa/tribe-backend/database"
)

// App holds every shared object. Controllers receive it and use its services.
type App struct {
	Settings   config.Config
	Database   *gorm.DB
	Redis      *redis.Client
	Toolkit    *security.Toolkit
	Dispatcher *jobs.Dispatcher
	Inspector  *asynq.Inspector // reads queue state for the admin Jobs page
	Providers  *providers.Registry
	RateHub    *events.Hub        // live rates for phones
	SupportHub *events.Hub        // live support inbox for admins
	Config     *cache.ConfigCache // shared copy of assets, providers, margins and fees

	Ledger      *ledger.Service
	Rates       *rates.Service
	Auth        *auth.Service
	AdminAuth   *adminauth.Service
	Idempotency *idempotency.Service
	Wallets     *wallets.Service
	Swaps       *swaps.Service
	Transfers   *transfers.Service
	Products    *products.Service
	Account     *account.Service
	Backoffice  *backoffice.Service
	Webhooks    *webhooks.Service
	ApiTrail    *apitrail.Writer // batched writer for api_request_logs
}

// New connects to MySQL and Redis and creates every service.
func New(settings config.Config) (*App, error) {
	if settings.App.MemoryLimitMB > 0 {
		// A soft limit: near it, Go's garbage collector works harder instead
		// of letting memory grow until the server kills the program.
		debug.SetMemoryLimit(int64(settings.App.MemoryLimitMB) * 1024 * 1024)
	}
	connection, err := database.Connect(settings.Database)
	if err != nil {
		return nil, err
	}
	redisClient, err := cache.Connect(settings.Redis)
	if err != nil {
		return nil, err
	}
	redisAddress := settings.Redis.Host + ":" + settings.Redis.Port

	application := &App{
		Settings:   settings,
		Database:   connection,
		Redis:      redisClient,
		Toolkit:    security.NewToolkit(settings.App.Key, settings.Auth.HashingConcurrency),
		Dispatcher: jobs.NewDispatcher(redisAddress, settings.Redis.Password),
		Inspector:  asynq.NewInspector(asynq.RedisClientOpt{Addr: redisAddress, Password: settings.Redis.Password}),
		Providers:  providers.NewRegistry(settings.IsProduction(), settings.Rates.NairaPerDollarSeed),
		RateHub:    events.NewRateHub(redisClient),
		SupportHub: events.NewSupportHub(redisClient),
		Config:     cache.NewConfigCache(connection, redisClient),
	}
	shared.UseConfigCache(application.Config)

	// Services, in dependency order (each one only uses services created above it).
	application.Ledger = ledger.NewService(connection, settings.App.TimeZone)
	application.Rates = rates.NewService(connection, redisClient, application.Providers, application.Config, settings)
	application.Auth = auth.NewService(connection, redisClient, application.Toolkit, application.Dispatcher, application.Ledger, settings)
	application.AdminAuth = adminauth.NewService(connection, redisClient, application.Toolkit, settings)
	application.Idempotency = idempotency.NewService(connection)
	application.Wallets = wallets.NewService(connection, application.Ledger, application.Rates, settings)
	application.Swaps = swaps.NewService(connection, application.Ledger, application.Rates, application.Auth, application.Dispatcher)
	application.Transfers = transfers.NewService(connection, application.Ledger, application.Rates, application.Auth, application.Providers, application.Dispatcher)
	application.Products = products.NewService(connection, application.Ledger, application.Rates, application.Auth, application.Providers, application.Dispatcher, application.Toolkit, settings)
	application.Account = account.NewService(connection, redisClient, application.Toolkit, application.Dispatcher, settings)
	application.Webhooks = webhooks.NewService(connection, application.Dispatcher, settings)
	application.ApiTrail = apitrail.NewWriter(connection)
	application.Backoffice = backoffice.NewService(connection, redisClient, application.Ledger, application.Rates, application.Auth, application.AdminAuth, settings)
	return application, nil
}

// Close disconnects everything (called when a program stops).
func (application *App) Close() {
	application.ApiTrail.Close() // save queued API trail rows before the database closes
	application.Dispatcher.Close()
	application.Inspector.Close()
	application.Redis.Close()
	if pool, err := application.Database.DB(); err == nil {
		pool.Close()
	}
}
