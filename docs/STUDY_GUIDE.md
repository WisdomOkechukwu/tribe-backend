# The Tribe backend study guide: Go, explained for Laravel developers

This guide is for someone who knows PHP and Laravel well and has never written Go. Read it from top to bottom the first time. Afterwards, use it as a reference.

It goes deeper than [`GO_FOR_BEGINNERS.md`](GO_FOR_BEGINNERS.md), which is the short tour. This guide explains **why** the code is shaped the way it is, and it shows the Go language features you will meet on almost every line.

Every file named here exists. Keep the code open next to this page and follow along.

**How the guide is laid out:**

| Part | What you learn |
|---|---|
| [1. The big mental shift](#part-1-the-big-mental-shift-php-vs-go) | How a Go program runs, compared with how PHP runs |
| [2. The Go language, one idea at a time](#part-2-the-go-language-one-idea-at-a-time) | Every Go feature this project uses, each next to its PHP or Laravel equivalent |
| [3. The project layout](#part-3-the-project-layout) | Where everything lives, and the Laravel folder each one replaces |
| [4. Start-up: what happens when you run the server](#part-4-start-up-what-happens-when-you-run-the-server) | `main()`, config, the hand-written "service container" |
| [5. The life of a request](#part-5-the-life-of-a-request-layer-by-layer) | Routes, middleware, controllers, requests, services, models, resources, responses |
| [6. Worked example: a swap from tap to database](#part-6-worked-example-a-swap-from-tap-to-database) | One real request followed through every file |
| [7. The database layer](#part-7-the-database-layer-gorm-transactions-and-locks) | GORM compared with Eloquent, transactions, row locks, migrations |
| [8. Money and the ledger](#part-8-money-and-the-ledger) | Why there is no `balance` column, and how double-entry works here |
| [9. Making things safe to retry and safe to race](#part-9-making-things-safe-to-retry-and-safe-to-race) | Idempotency, quotes, version numbers, single device |
| [10. Work that happens outside the request](#part-10-work-that-happens-outside-the-request) | Queues, cron, the push outbox, live updates, webhooks |
| [11. Admin panel specifics](#part-11-admin-panel-specifics) | Permissions, audit log, two-person approval |
| [12. Tests](#part-12-tests) | Unit tests, integration tests, and how a test database is made |
| [13. Day-to-day commands](#part-13-day-to-day-commands) | Make targets compared with artisan and composer |
| [14. Adding an endpoint, step by step](#part-14-adding-an-endpoint-step-by-step) | A full worked example you can copy |
| [15. Cheat sheet and glossary](#part-15-cheat-sheet-and-glossary) | Laravel → Go lookup table and a glossary |
| [16. Check your understanding](#part-16-check-your-understanding) | Questions with answers |

---

## Part 1: The big mental shift (PHP vs Go)

Before any syntax, understand this one difference. It explains most of what looks strange in the code.

### PHP starts fresh for every request

With PHP-FPM, every HTTP request goes like this:

1. A PHP process is handed the request.
2. Laravel boots: it reads `.env`, builds the service container and registers providers.
3. Your controller runs.
4. The response is sent.
5. **Everything is thrown away.** The next request boots again from nothing.

Because of this, PHP code rarely worries about two requests sharing memory. They never do.

### Go starts once and keeps running

A Go program is **compiled** into one executable file (for example `bin/server`). When you start it:

1. `main()` runs **once**. It reads config, connects to MySQL and Redis, and creates every service.
2. It then listens on a port, **forever**.
3. Every incoming request is handled by a **goroutine** (a lightweight thread, explained in Part 2). Many requests run **at the same time inside the same process**, and they share the same services, the same database connection pool and the same Redis client.

What this means for you:

| Consequence | Where you see it |
|---|---|
| Expensive set-up (DB connections, services) happens once, not per request. | [`app/bootstrap/bootstrap.go`](../app/bootstrap/bootstrap.go) |
| Shared memory touched by many requests at once must be protected with a lock (a mutex). | `mutex sync.Mutex` in [`app/events/hub.go`](../app/events/hub.go) |
| Services must not store per-request data in their fields. They hold only shared things (the DB, Redis, other services). Per-request data travels as function arguments. | Every `Service` struct in `app/services/*` |
| You must **rebuild and restart** after changing code. There is no "save and refresh". | `make dev` builds first, then starts |
| Long-lived connections are cheap. One goroutine can hold an open HTTP response for minutes. | The live-rates stream in [`money_controller.go`](../app/http/controllers/mobile/money_controller.go) (`RatesStream`) |

### Compiled and typed

Go checks your code **before** it runs. If you call a method that does not exist, pass a `string` where an `int` is expected, or forget to use a variable you declared, the program **will not build**. PHP would only tell you when that line ran. Treat build errors as a free code review.

---

## Part 2: The Go language, one idea at a time

Each section shows a small piece of real project code, explains it, and gives the PHP equivalent.

### 2.1 Modules and packages (Composer and namespaces)

**`go.mod` is `composer.json`. `go.sum` is `composer.lock`.** Open [`go.mod`](../go.mod):

```go
module github.com/tribe-africa/tribe-backend

go 1.27.1

require (
	github.com/gin-gonic/gin v1.12.0   // the web framework (our "Laravel routing + request")
	gorm.io/gorm v1.31.2               // the ORM (our "Eloquent")
	github.com/hibiken/asynq v0.26.0   // the queue (our "Laravel queues")
	github.com/pressly/goose/v3 ...    // migrations
	github.com/redis/go-redis/v9 ...   // Redis client
	github.com/robfig/cron/v3 ...      // cron (our "schedule")
	...
)
```

The first line names **the module**: the root "namespace" of the whole project.

**One folder = one package.** Every `.go` file in `app/services/swaps/` starts with `package swaps`. All files in that folder share everything with each other, as if they were one big file. That is why `app/services/ledger/` is split into `posting.go`, `rules.go`, `balances.go` and so on: it is one package, organised into files for reading.

**Importing** uses the module path plus the folder:

```go
import (
	"context"   // from Go's standard library
	"time"

	"gorm.io/gorm"   // a third-party package

	"github.com/tribe-africa/tribe-backend/app/services/ledger"  // our own package
)
```

After that you write `ledger.Post(...)` or `ledger.Debit`. The last part of the path (`ledger`) is the name you type. There is no autoloader: if you use a package, you must import it, and if you import it, you must use it (otherwise it will not build). `gofmt` and most editors manage imports for you.

| PHP / Laravel | Go |
|---|---|
| `namespace App\Services\Swaps;` | `package swaps` (at the top of the file) |
| `use App\Services\Ledger\LedgerService;` | `import ".../app/services/ledger"` |
| `composer require x/y` | `go get x/y` |
| `vendor/` folder | the Go module cache (outside the project) |

### 2.2 Public and private: capital letters

Go has no `public` or `private` keywords. **A name that starts with a capital letter can be used from other packages. A lowercase name is private to its package.**

From [`app/services/ledger/posting.go`](../app/services/ledger/posting.go):

```go
func (service *Service) Post(...)       // Post: capital P, other packages can call ledger.Post
func validateLegs(legs []Leg) error     // validateLegs: lowercase, only the ledger package can call it
```

The same rule applies to struct fields, constants and types. In [`app/jobs/dispatcher.go`](../app/jobs/dispatcher.go), the `Dispatcher` has a lowercase field `client`, so nobody outside the `jobs` package can touch the Redis client directly. They must go through methods like `SendEmailCode`.

### 2.3 Variables, `:=` and zero values

```go
var transactionCount int64           // declared, starts at 0
transaction := &models.Transaction{} // declared AND assigned; Go works out the type
transaction, err := service.Swap(...) // two values at once
```

- `:=` means "create a new variable and work out its type from the right-hand side". Use it inside functions.
- `var name Type` creates a variable set to its **zero value**.

**Zero values** replace PHP's `null` for basic types. Every type has a default:

| Type | Zero value |
|---|---|
| `int`, `int64`, `uint64` | `0` |
| `string` | `""` (empty string, never null) |
| `bool` | `false` |
| pointer (`*User`), map, slice, interface, channel | `nil` |
| a struct | every field set to its own zero value |

So a plain `string` can never be "missing". When a database column is **nullable**, the model uses a **pointer** instead, as in [`app/models/users.go`](../app/models/users.go):

```go
Phone           *string    // NULL in MySQL becomes nil here
PhoneVerifiedAt *time.Time // nil means "not verified yet"
```

Read `*string` as "a string, or nothing". To read it, check for nil first:

```go
if user.Phone != nil {
	fmt.Println(*user.Phone)   // *user.Phone means "the string the pointer points at"
}
```

### 2.4 Structs (classes with only properties)

A struct is a named group of fields. There is no inheritance and no constructor keyword.

```go
// From app/services/shared/shared.go
type Actor struct {
	UserID   string
	DeviceID string
}

actor := shared.Actor{UserID: "01J...", DeviceID: "01K..."}
fmt.Println(actor.UserID)
```

PHP equivalent:

```php
final class Actor {
    public function __construct(public string $userId, public string $deviceId) {}
}
```

**Struct tags** are the text in backticks after a field. Libraries read them. You will see three kinds:

```go
type SwapRequest struct {
	QuoteID string `json:"quote_id" binding:"required"`
	Pin     string `json:"pin" binding:"required,len=4,numeric"`
}
```

| Tag | Read by | Meaning |
|---|---|---|
| `json:"quote_id"` | Go's JSON encoder | the key name in JSON |
| `json:"-"` | Go's JSON encoder | never put this field in JSON (used for `PasswordHash`) |
| `binding:"required,len=4"` | Gin's validator | validation rules, like a FormRequest's `rules()` |
| `gorm:"primaryKey"` | GORM | database details for this column |

### 2.5 Functions, multiple return values and methods

A function can return **more than one value**. This is used everywhere for "the result, plus an error":

```go
func (service *Service) Swap(requestContext context.Context, actor shared.Actor, input SwapInput) (*models.Transaction, error)
//   ^^^^^^^^^^^^^^^^^^^ receiver          ^^^ parameters, each "name Type"                        ^^^ two return values
```

The part before the name, `(service *Service)`, is the **receiver**. It makes `Swap` a **method** on `Service`. Inside, `service` plays the role of PHP's `$this`. This project always names the receiver after the type in full (`service`, `controller`, `hub`), never `s` or `c`.

PHP equivalent:

```php
class SwapService {
    public function swap(Actor $actor, SwapInput $input): Transaction { ... }
}
```

**Constructors are ordinary functions** named `NewSomething` by convention. From [`app/services/swaps/service.go`](../app/services/swaps/service.go):

```go
func NewService(connection *gorm.DB, ledgerService *ledger.Service, ratesService *rates.Service, authService *auth.Service, dispatcher *jobs.Dispatcher) *Service {
	return &Service{database: connection, ledger: ledgerService, rates: ratesService, auth: authService, dispatcher: dispatcher}
}
```

This is **constructor injection**, written by hand. Laravel's container would do this for you from type hints. In Go you call `NewService(...)` yourself, in [`bootstrap.go`](../app/bootstrap/bootstrap.go).

### 2.6 Pointers: `&` and `*`

In PHP, objects are always passed by handle, so a function can change the object you gave it. In Go, **structs are copied** when you pass them, unless you pass a **pointer**.

- `&value` means "the address of value" (make a pointer to it).
- `*Type` in a type means "a pointer to a Type".
- `*pointer` in an expression means "the value it points at".

You meet pointers in three situations:

1. **Services return pointers** to avoid copying and so callers can tell "nothing" (`nil`) from "something": `func Swap(...) (*models.Transaction, error)`.
2. **Filling a variable from the database**: you pass the address so GORM can write into it:

   ```go
   var quote models.Quote
   databaseTransaction.Where("id = ?", quoteID).Take(&quote)   // &quote: "fill this in"
   ```

   Compare with Eloquent, which *returns* a new object: `$quote = Quote::find($id);`.
3. **Nullable fields**, as shown in 2.3.

Methods are usually declared on the pointer (`func (service *Service)`), so they work on the one shared service and never on a copy.

### 2.7 Errors instead of exceptions

Go has no `throw`, `try` or `catch` for normal failures. **A function that can fail returns an `error` as its last value. The caller checks it at once.**

```go
transaction, err := controller.application.Swaps.Swap(httpContext.Request.Context(), actor, input)
if err != nil {
	responses.Error(httpContext, err)   // turn it into a JSON error
	return                              // stop here
}
// from this line on, transaction is safe to use
```

You will see `if err != nil { return ..., err }` hundreds of times. That is normal Go: every failure is visible right where it can happen. Errors bubble up by being **returned**, one layer at a time, until someone handles them. In this project that "someone" is the controller, which calls `responses.Error`.

**`err` is the only short variable name this codebase allows.** Every Go programmer recognises it.

**Our own error type.** [`app/apperrors/apperrors.go`](../app/apperrors/apperrors.go) defines `AppError`, which is the equivalent of throwing an `HttpException` with a status in Laravel:

```go
type AppError struct {
	Status      int                 // HTTP status, e.g. 422
	Code        string              // machine-readable, e.g. "INSUFFICIENT_FUNDS"
	Message     string              // human-readable, shown in the app
	FieldErrors map[string][]string // per-field messages, like Laravel's $errors bag
}
```

There are ready-made ones such as `apperrors.ErrInsufficientFunds`, `apperrors.ErrStaleData` and `apperrors.NotFound("Card")`.

**How does a struct count as an `error`?** `error` is a built-in **interface** with one method, `Error() string`. `AppError` has that method, so it *is* an error (see 2.9 for interfaces).

**`errors.Is` and `errors.As`** look inside an error that has been wrapped:

```go
errors.Is(err, gorm.ErrRecordNotFound)   // "is this (or something it wraps) that exact error?"
errors.As(err, &appError)                // "is this (or something it wraps) an *AppError? If so, put it in appError"
```

`responses.Error` uses `apperrors.As` to decide: an `AppError` keeps its status and message; anything else is a bug, gets logged, and becomes a generic 500 so internal details never leak.

**Wrapping** adds context while keeping the original: `fmt.Errorf("cannot reach MySQL: %w", err)`. The `%w` is what lets `errors.Is` and `errors.As` see through it.

**`panic`** exists, but it is for "the program is broken", not for business failures. The project panics only at start-up for impossible configuration (for example a bad `TRUSTED_PROXIES` list in [`routes/routes.go`](../routes/routes.go)). `gin.Recovery()` turns any panic during a request into a 500 so the server keeps running.

### 2.8 Slices and maps (PHP arrays, split in two)

PHP's `array` does two jobs. Go has two separate types.

**Slice: an ordered list.** `[]Leg` means "a list of `Leg`".

```go
legs := []Leg{
	{AccountID: companyVault, Side: Debit, Amount: amount},
	{AccountID: userWallet, Side: Credit, Amount: amount},
}
legs = append(legs, Leg{...})   // like $legs[] = ...
len(legs)                       // like count($legs)
```

**Map: key → value.** `map[string]int` means "keys are strings, values are ints".

```go
decimals := map[string]int{"USD": 2, "BTC": 8}
decimals["NGN"] = 2
places, found := decimals["EUR"]   // found is false if the key is missing
```

Reading a missing key gives the zero value, so use the two-value form when "missing" matters.

**Looping** uses `range`:

```go
for _, account := range accounts {        // _ throws away the index
	accountsByID[account.ID] = account
}
for index, leg := range legs { ... }      // index and value
for key, value := range someMap { ... }   // key and value (order is random!)
```

The underscore `_` is the **blank identifier**: "I must receive this value, but I do not want it."

**`any`** means "a value of any type" (like an untyped PHP variable). `map[string]any` is the closest thing to a PHP associative array of mixed values, and `gin.H` is just a short name for `map[string]any`.

### 2.9 Interfaces (implemented without saying so)

An interface lists methods. **Any type that has those methods satisfies the interface automatically.** There is no `implements` keyword.

From [`app/services/providers/providers.go`](../app/services/providers/providers.go):

```go
type PayoutProvider interface {
	SendPayout(requestContext context.Context, reference string, assetCode string, amount string, destination map[string]string) (PayoutResult, error)
}
```

The simulator in [`simulator.go`](../app/services/providers/simulator.go) has a `SendPayout` method with that exact signature, so it is a `PayoutProvider`. When a real bank provider is built, it will have the same method and will drop straight in. The transfers service only ever talks to `PayoutProvider`, so it never needs to change.

This is the same idea as binding an interface to an implementation in a Laravel service provider, except the "binding" happens in the provider registry rather than in a container.

### 2.10 `defer` (like `finally`)

`defer` schedules a call to run **when the surrounding function returns**, however it returns.

```go
application, err := bootstrap.New(settings)
...
defer application.Close()   // runs when main() ends, even after an early return
```

```go
hub.mutex.Lock()
defer hub.mutex.Unlock()    // always unlock, even if the code below returns early
```

Deferred calls run in reverse order (last deferred, first run).

### 2.11 Closures and "functions that return functions"

Functions are values in Go, like PHP closures. This project uses that heavily for **middleware**, which needs configuration up front and then runs per request.

From [`app/http/middleware/authentication.go`](../app/http/middleware/authentication.go):

```go
func RequirePermission(permission string) gin.HandlerFunc {
	return func(httpContext *gin.Context) {          // this inner function runs on every request
		admin := CurrentAdmin(httpContext)
		if admin == nil || !admin.Can(permission) {  // it "remembers" permission from the outer call
			responses.Error(httpContext, apperrors.ErrForbidden)
			return
		}
		httpContext.Next()
	}
}
```

- `RequirePermission("kyc.decide")` runs **once**, when routes are built, and returns a function.
- That returned function runs on **every request**.

It is the Go version of Laravel's `->middleware('can:kyc.decide')`, where `kyc.decide` is the middleware parameter.

The same trick builds controller actions that take a parameter. In [`routes/mobile.go`](../routes/mobile.go):

```go
signedIn.POST("/kyc/tier-1", idempotent, profileController.SubmitKyc(1))
signedIn.POST("/kyc/tier-2", idempotent, profileController.SubmitKyc(2))
```

`SubmitKyc(1)` returns a handler that knows its tier is 1.

### 2.12 `context.Context` ("is this request still alive?")

Almost every service method takes `requestContext context.Context` as its first argument. A context carries two things:

1. **Cancellation.** If the phone disconnects, or the server is shutting down, the context is cancelled. Database queries and Redis calls that were given this context stop early instead of wasting work.
2. **Deadlines.** A context can say "give up after 15 seconds".

Where it comes from:

- In a controller: `httpContext.Request.Context()` (the HTTP request's context).
- In `main()`: `signal.NotifyContext(...)` gives a context that is cancelled when you press Ctrl+C.
- In jobs: the queue library hands one to each job.

You pass it down; you rarely create one. PHP has no direct equivalent, because a PHP process simply dies when the request ends.

**Do not confuse the two "contexts":**

| Name in this code | Type | What it is |
|---|---|---|
| `httpContext` | `*gin.Context` | Gin's request **and** response object, like Laravel's `Request` plus `response()`. Only controllers and middleware see it. |
| `requestContext` | `context.Context` | The "still alive?" signal. Services receive this, never `httpContext`. |

That split is a house rule: **services never read `gin.Context`**. It keeps business logic independent of HTTP, so the worker and scheduler can call the same services.

### 2.13 Goroutines, channels and `select`

A **goroutine** is a function started with the word `go`. It runs at the same time as everything else.

```go
go application.RateHub.Run(stopContext)   // from cmd/server/main.go: listen to Redis in the background
```

Think of it as dispatching a job that runs immediately in the same process, with almost no cost. The HTTP server already runs every request in its own goroutine, so you rarely start one yourself.

A **channel** is a typed pipe between goroutines. `chan []byte` carries byte slices.

```go
connection := make(chan []byte, 1)   // a pipe that can hold 1 message
connection <- event                  // send into the pipe
payload := <-connection              // receive from the pipe (waits until something arrives)
```

**`select`** waits on several channels and runs whichever is ready first. Here is the live-rates loop from [`money_controller.go`](../app/http/controllers/mobile/money_controller.go), with comments:

```go
for {                                                   // loop forever
	select {
	case <-httpContext.Request.Context().Done():       // the phone went away
		return
	case payload := <-connection:                      // the hub sent new rates
		writeEvent("rates.updated", event.Version, payload)
	case <-heartbeat.C:                                // 15 seconds passed
		fmt.Fprint(writer, ": heartbeat\n\n")
	}
}
```

A `select` with a `default:` case never waits. [`hub.go`](../app/events/hub.go) uses that in `broadcast` so one slow phone can never hold up the others.

**`sync.Mutex`** is a lock for memory shared between goroutines. The hub's list of subscribers is touched by many requests at once, so every read or write is wrapped in `hub.mutex.Lock()` / `Unlock()`. PHP never needed this because PHP requests never share memory.

**Where you will actually meet concurrency in this project:**

| Place | Why |
|---|---|
| [`cmd/server/main.go`](../cmd/server/main.go) | runs the two event hubs and the HTTP server in goroutines |
| [`cmd/scheduler/main.go`](../cmd/scheduler/main.go) | runs the rate collector and the push outbox in goroutines |
| [`app/events/hub.go`](../app/events/hub.go) | channels and a mutex for live updates |
| [`app/http/middleware/common.go`](../app/http/middleware/common.go) | `RequestLogger` saves the API log in a goroutine so the user does not wait |
| [`tests/integration/concurrency_test.go`](../tests/integration/concurrency_test.go) | fires 50 requests at once to prove money is safe |

Everything else is plain top-to-bottom code.

### 2.14 Constants and "enums"

Go has no `enum` keyword. The project uses named string constants instead:

```go
// From app/models/users.go
const (
	DeviceStatusPending  = "pending"
	DeviceStatusActive   = "active"
	DeviceStatusReplaced = "replaced"
	DeviceStatusRevoked  = "revoked"
)
```

Write `models.DeviceStatusActive`, not `"active"`, so a typo fails the build instead of silently matching nothing.

### 2.15 Formatting and checks are not optional

- `gofmt` formats every file in one standard style. There is no style debate in Go. `make fmt` formats; `make vet` fails if anything is unformatted.
- `go vet` finds likely bugs (wrong `Printf` arguments, unreachable code, and so on).
- An unused variable or import is a **build error**, not a warning.

---

## Part 3: The project layout

Go does not force a folder structure. This project copies Laravel's on purpose so the team feels at home.

```
tribe-backend/
├── cmd/                     the four programs (each has its own main())
│   ├── server/main.go       php artisan serve
│   ├── worker/main.go       php artisan queue:work
│   ├── scheduler/main.go    php artisan schedule:work
│   └── artisan/main.go      php artisan (migrate, db:seed, ledger:verify, ...)
├── config/config.go         config/*.php + .env reading
├── routes/                  routes/api.php
│   ├── routes.go            builds the router, global middleware
│   ├── mobile.go            /api/mobile/v1/...
│   ├── admin.go             /admin-api/...
│   └── system.go            /health/..., /webhooks/:provider
├── app/
│   ├── bootstrap/           the hand-written service container
│   ├── http/
│   │   ├── controllers/     mobile/, admin/, system/
│   │   ├── middleware/      auth, single device, idempotency, rate limit, API trail
│   │   ├── requests/        FormRequests (input structs + binding rules)
│   │   ├── resources/       API Resources (output shapes)
│   │   └── responses/       the JSON envelope (success / error)
│   ├── services/            ALL business logic, one package per area
│   ├── models/              Eloquent models (GORM structs)
│   ├── apperrors/           HttpException-style errors
│   ├── jobs/                queued jobs (dispatcher + handlers/)
│   ├── console/             Kernel::schedule (cron)
│   ├── events/              broadcasting (Redis pub/sub → Server-Sent Events)
│   └── cache/               Redis connection and locks
├── database/
│   ├── connection.go        DB connection + the "tribe_core only" guard
│   ├── locking.go           WithTransaction, LockForUpdate, LockForShare
│   ├── migrate.go           runs migrations
│   ├── migrations/          numbered .sql files (goose)
│   └── seeders/             DatabaseSeeder
├── pkg/
│   ├── money/               integer money (never float)
│   └── ids/                 ULID ids and random codes
├── tests/
│   ├── testenv/             boots a full API on a throw-away database
│   └── integration/         Feature tests over HTTP
├── scripts/try-the-api.sh   a guided curl tour
└── docs/                    you are here
```

**Why `cmd/`?** A Laravel app has one entry point (`public/index.php`) plus `artisan`. This project has four separate programs that share all the code under `app/`. Each folder under `cmd/` holds one `package main` with one `func main()`, and `make build` turns each into a file in `bin/`.

**Why `pkg/`?** By Go convention, `pkg/` holds small, general-purpose code that knows nothing about Tribe's business. `money` and `ids` could be used in any project.

---

## Part 4: Start-up: what happens when you run the server

Open [`cmd/server/main.go`](../cmd/server/main.go). This is the whole start-up, in order.

### Step 1: load configuration

```go
settings, err := config.Load()
```

[`config/config.go`](../config/config.go) is Laravel's `.env` plus `config/*.php` in one file.

- `godotenv.Load()` reads `.env`. Real environment variables win over the file, exactly like Laravel.
- Each value is read with a default: `readString("APP_PORT", "8080")` is like `env('APP_PORT', '8080')`.
- Durations are turned into Go's `time.Duration`: `readSeconds("QUOTE_LIFETIME_SECONDS", 30)`.
- The result is one typed `Config` struct. Instead of `config('rates.quote_lifetime')` with a string key, you write `settings.Rates.QuoteLifetime`, and a typo fails the build.

There is one safety check: in production, `APP_KEY` must be at least 32 characters, or the program refuses to start.

### Step 2: build the application ("the service container")

```go
application, err := bootstrap.New(settings)
defer application.Close()
```

Open [`app/bootstrap/bootstrap.go`](../app/bootstrap/bootstrap.go). `App` is a struct with one field per shared object:

```go
type App struct {
	Settings   config.Config
	Database   *gorm.DB
	Redis      *redis.Client
	Dispatcher *jobs.Dispatcher
	...
	Ledger     *ledger.Service
	Swaps      *swaps.Service
	Transfers  *transfers.Service
	...
}
```

`New` connects to MySQL and Redis, then creates each service **in dependency order**:

```go
application.Ledger = ledger.NewService(connection, settings.App.TimeZone)
application.Rates  = rates.NewService(connection, redisClient, application.Providers, settings)
application.Auth   = auth.NewService(connection, redisClient, application.Toolkit, application.Dispatcher, application.Ledger, settings)
...
application.Swaps  = swaps.NewService(connection, application.Ledger, application.Rates, application.Auth, application.Dispatcher)
```

Read it as a dependency graph: Swaps needs Ledger, Rates and Auth, so they are created first.

**Compared with Laravel:** Laravel's container builds objects for you by reading type hints, and you rarely see the wiring. Here the wiring is written out by hand, so you can open one file and see exactly what depends on what. There is no magic, and a missing dependency is a build error.

All four programs (server, worker, scheduler, artisan) call `bootstrap.New`, so they all share the same services. A payout job in the worker calls the very same `application.Transfers.ProcessPayout` that the code is tested with.

### Step 3: start background listeners

```go
stopContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
go application.RateHub.Run(stopContext)
go application.SupportHub.Run(stopContext)
```

- `stopContext` is cancelled when you press Ctrl+C or the server is told to stop.
- Each hub runs in its own goroutine and listens to a Redis channel (Part 10).

### Step 4: build the router and listen

```go
server := &http.Server{
	Addr:    ":" + settings.App.Port,
	Handler: routes.Build(application),
	...
}
go func() { server.ListenAndServe() }()
```

`routes.Build` (in [`routes/routes.go`](../routes/routes.go)) creates the Gin router, adds global middleware and registers the three route files.

### Step 5: wait, then shut down gracefully

```go
<-stopContext.Done()                       // block here until Ctrl+C
server.Shutdown(shutdownContext)           // stop accepting, let running requests finish (max 15 s)
```

`<-stopContext.Done()` is "wait until this channel closes". The `main` function just sits on this line while goroutines handle requests. When it moves on, the server stops taking new requests and gives running ones 15 seconds to finish, so a payment in progress is not cut in half.

---

## Part 5: The life of a request, layer by layer

The house rule from `CLAUDE.md`:

```
Route → Middleware → Controller → Request (validation) → Service → Model/DB → Resource → responses.OK/Created/Error
```

Each layer has one job. This part explains each one.

### 5.1 Routes

[`routes/routes.go`](../routes/routes.go) builds the router:

```go
router := gin.New()
router.Use(gin.Recovery(), middleware.RequestID(), middleware.Cors(...))  // global middleware, every request
registerSystemRoutes(router, application)
registerMobileRoutes(router, application)
registerAdminRoutes(router, application)
```

**Route groups** work like `Route::prefix()->middleware()->group()`. From [`routes/mobile.go`](../routes/mobile.go):

```go
api := router.Group("/api/mobile/v1", middleware.RequestLogger(application, "user"))
idempotent := middleware.RequireIdempotencyKey(application, "user")

public := api.Group("/auth", middleware.RateLimit(application, "mobile-auth", 30))
public.POST("/login", authController.Login)

signedIn := api.Group("", middleware.AuthenticateUser(application), middleware.EnsureSingleDevice(application), middleware.RateLimit(application, "mobile", 240))
signedIn.POST("/swaps", idempotent, moneyController.Swap)
```

How to read the last line, left to right:

1. It is in the `signedIn` group, so the group's middleware runs first: `RequestLogger` (from `api`), then `AuthenticateUser`, `EnsureSingleDevice` and `RateLimit`.
2. Then the route's own middleware: `idempotent`.
3. Then the last function, `moneyController.Swap`, is the controller action.

Laravel equivalent:

```php
Route::prefix('api/mobile/v1')->middleware('api-trail:user')->group(function () {
    Route::middleware(['auth:mobile', 'single-device', 'throttle:240,1'])->group(function () {
        Route::post('/swaps', [MoneyController::class, 'swap'])->middleware('idempotent');
    });
});
```

**Route parameters** use a colon: `/cards/:id/fund`. Read them with `httpContext.Param("id")`, like `$request->route('id')`. Query strings use `httpContext.Query("page")`.

**Two URL styles** on purpose:

| App | Prefix | Style | Why |
|---|---|---|---|
| Mobile | `/api/mobile/v1` | versioned, kebab-case | old app versions stay installed on phones for months |
| Admin | `/admin-api` | unversioned REST | the admin panel and API always deploy together |

Run `make routes` to print every route, like `php artisan route:list`.

### 5.2 Middleware

A middleware is a `gin.HandlerFunc`: a function that receives `httpContext *gin.Context`. It does one of two things:

- calls `httpContext.Next()` to continue down the chain (Laravel's `return $next($request);`), or
- calls `responses.Error(...)`, which **aborts**, so nothing after it runs.

Code written **after** `httpContext.Next()` runs on the way back out, after the controller has finished. That is how `RequestLogger` measures time and how `RequireIdempotencyKey` saves the response.

**Passing data to later layers.** Middleware stores values on the request with `httpContext.Set(key, value)`, and later code reads them with `httpContext.GetString(key)`. Like `$request->attributes->set()`. Helper functions wrap this so controllers never use raw keys:

```go
middleware.CurrentActor(httpContext)   // {UserID, DeviceID}, set by AuthenticateUser
middleware.CurrentAdmin(httpContext)   // *adminauth.Admin, set by AuthenticateAdmin
```

**The middleware in this project:**

| Middleware | File | What it does | Laravel equivalent |
|---|---|---|---|
| `gin.Recovery` | (Gin) | turns a panic into a 500 | the exception handler |
| `RequestID` | [`common.go`](../app/http/middleware/common.go) | gives each request an id, sent back as `X-Request-Id` | (custom) |
| `Cors` | [`common.go`](../app/http/middleware/common.go) | lets the admin website call the API | `HandleCors` |
| `RequestLogger` | [`common.go`](../app/http/middleware/common.go) | saves every call to `api_request_logs` with secrets hidden (the admin "API trail") | Telescope-like |
| `RateLimit` | [`common.go`](../app/http/middleware/common.go) | N requests per minute per IP, counted in Redis | `throttle:N,1` |
| `AuthenticateUser` | [`authentication.go`](../app/http/middleware/authentication.go) | checks the mobile JWT | `auth:sanctum` |
| `EnsureSingleDevice` | [`authentication.go`](../app/http/middleware/authentication.go) | checks this phone is the user's **active** phone | (custom) |
| `AuthenticateAdmin` | [`authentication.go`](../app/http/middleware/authentication.go) | checks the admin JWT and loads the admin | `auth:admin` |
| `RequirePermission` (`can`) | [`authentication.go`](../app/http/middleware/authentication.go) | the admin must have a permission | `can:...` |
| `RequireIdempotencyKey` | [`idempotency.go`](../app/http/middleware/idempotency.go) | makes a change safe to retry (Part 9) | (custom) |

### 5.3 Controllers

Controllers are deliberately boring. Every action follows the same recipe: **bind, call one service, respond.**

```go
// Swap  POST /swaps
func (controller *MoneyController) Swap(httpContext *gin.Context) {
	// 1. Bind and validate the body.
	var input requests.SwapRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	// 2. Call ONE service method. Pass plain values, never httpContext.
	transaction, err := controller.application.Swaps.Swap(
		httpContext.Request.Context(),
		middleware.CurrentActor(httpContext),
		swaps.SwapInput{QuoteID: input.QuoteID, Pin: input.Pin, Reference: middleware.BusinessReference(httpContext, "SWP")},
	)
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	// 3. Shape the output and send it.
	responses.Created(httpContext, "Swap complete.", resources.Transaction(*transaction, controller.decimals(httpContext)))
}
```

A controller is a struct holding the `*bootstrap.App`, created once in the route file with `mobile.NewMoneyController(application)`. That is how it reaches the services: `controller.application.Swaps`.

Notice that the action **does not return anything**. In Laravel you `return response()->json(...)`. In Gin you *write* the response onto `httpContext` (via `responses.Created`) and then `return` to end the function.

Controller files:

| File | Covers |
|---|---|
| [`mobile/auth_controller.go`](../app/http/controllers/mobile/auth_controller.go) | sign-up, sign-in, new phone, refresh, password reset |
| [`mobile/profile_controller.go`](../app/http/controllers/mobile/profile_controller.go) | profile, PIN, phone, KYC, devices |
| [`mobile/money_controller.go`](../app/http/controllers/mobile/money_controller.go) | wallets, rates, quotes, swaps, sends, activity |
| [`mobile/products_controller.go`](../app/http/controllers/mobile/products_controller.go) | bills, gift cards, cashpoints, cards, referrals, inbox, support |
| [`admin/admin_controller.go`](../app/http/controllers/admin/admin_controller.go) | every admin route |
| [`system/webhook_controller.go`](../app/http/controllers/system/webhook_controller.go) | provider webhooks |

**A few read-only actions query the database directly** (for example `Assets` lists enabled currencies). That is allowed for simple lookups. **Business logic** (anything that checks rules, locks rows or moves money) always lives in a service.

### 5.4 Requests (validation)

[`app/http/requests/requests.go`](../app/http/requests/requests.go) holds one struct per request body, with rules in `binding:` tags:

```go
type QuoteRequest struct {
	TransactionType string `json:"transaction_type" binding:"required,oneof=swap send"`
	FromAsset       string `json:"from_asset" binding:"required,max=10"`
	ToAsset         string `json:"to_asset" binding:"required,max=10"`
	Amount          string `json:"amount" binding:"max=40"`
	ReceiveAmount   string `json:"receive_amount" binding:"max=40"`
}
```

| Go rule | Laravel rule |
|---|---|
| `required` | `required` |
| `email` | `email` |
| `min=4`, `max=191` | `min:4`, `max:191` (string length) |
| `len=4` | `size:4` |
| `oneof=swap send` | `in:swap,send` |
| `numeric` | `digits` (only 0–9) |

`requests.Bind(httpContext, &input)`:

1. reads the JSON body into `input` (`&input` so it can fill it in),
2. runs the `binding` rules,
3. on failure returns a **422** `AppError` with per-field messages, the same shape as Laravel's validation errors:

```json
{"success": false, "message": "Please check the highlighted fields.", "code": "VALIDATION_FAILED",
 "errors": {"pin": ["Must be exactly 4 characters."]}}
```

**Amounts are strings**, never numbers (`Amount string`). JSON numbers are floats in most languages, and floats lose precision. The service parses the string into exact integer money (Part 8).

Rules that need the database (for example "is this tag taken?") are checked in the **service**, not in the request struct.

### 5.5 Services (all business logic)

Services live in `app/services/<area>/`. Each package has a `Service` struct, a `NewService` constructor, and methods.

| Package | Responsibility |
|---|---|
| [`ledger`](../app/services/ledger) | the books: accounts, posting rules, `Post`, balances, snapshots, verification |
| [`rates`](../app/services/rates) | live rates, pricing, quotes, the NGN rail route |
| [`swaps`](../app/services/swaps) | converting between a user's own wallets |
| [`transfers`](../app/services/transfers) | sending to Tribe users, banks and crypto wallets; payouts |
| [`wallets`](../app/services/wallets) | listing and opening wallets, receive details, test top-up |
| [`products`](../app/services/products) | bills, cards, gift cards, cashpoints, referrals |
| [`auth`](../app/services/auth) | sign-up, sign-in, devices, sessions, PINs |
| [`adminauth`](../app/services/adminauth) | admin sign-in, permissions, team, audit log |
| [`account`](../app/services/account) | profile, KYC, inbox, push outbox |
| [`backoffice`](../app/services/backoffice) | admin pages: customers, finance, operations |
| [`idempotency`](../app/services/idempotency) | remembering requests so retries are safe |
| [`webhooks`](../app/services/webhooks) | verifying and storing provider webhooks |
| [`providers`](../app/services/providers) | interfaces for outside companies, plus simulators |
| [`security`](../app/services/security) | tokens, hashing, encryption |
| [`shared`](../app/services/shared) | small helpers every money service uses |

Rules for services:

- They receive `requestContext context.Context` and plain values. **Never** `gin.Context`.
- They return `(result, error)`. Caller mistakes are `apperrors.AppError`; anything else is treated as a bug.
- They own transactions, locks, ledger posting and inbox messages.

### 5.6 Models

[`app/models/`](../app/models) holds one struct per table:

```go
// User is a Tribe customer (table: users).
type User struct {
	ID           string `gorm:"primaryKey" json:"id"`
	Email        string `json:"email"`
	PasswordHash string `json:"-"`
	...
	Version      int       `json:"version"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}
```

How GORM maps it (by convention, like Eloquent):

- struct `User` → table `users` (plural, snake_case)
- field `PasswordHash` → column `password_hash`
- when the guess is wrong, a `TableName()` method sets it: `func (ExchangeRateLatest) TableName() string { return "exchange_rate_latest" }`

**Differences from Eloquent that matter:**

| Eloquent | Here |
|---|---|
| A model is an active object: `$user->save()`, `$user->wallets` | A model is **only data**. All queries go through a `*gorm.DB` (see Part 7). |
| Relationships (`hasMany`, lazy loading) | **Not used.** You query related tables yourself, explicitly. No hidden N+1 queries. |
| `$casts` | Go types do it: `time.Time`, `bool`, `datatypes.JSON`. |
| `$hidden` | `json:"-"`, and more importantly **resources** decide the output. |
| Auto-increment ids | Mostly **ULID strings** (`CHAR(26)`), made by `ids.New()`. Ledger tables use numeric ids. |

Generated columns are marked read-only with `gorm:"<-:false"`, so Go never tries to write them (see `ActiveUserID` on `UserDevice`).

### 5.7 Resources

[`app/http/resources/resources.go`](../app/http/resources/resources.go) turns models into the JSON the apps receive, like Laravel API Resources.

```go
type TransactionResource struct {
	ID     string `json:"id"`
	Amount string `json:"amount"`   // "12.50": always a decimal STRING
	...
}

func Transaction(transaction models.Transaction, decimals map[string]int) TransactionResource { ... }
```

Why a separate layer:

- **Secrets cannot leak by accident.** A new column on `users` does not appear in the API until someone adds it to the resource.
- **Money is formatted once.** The database stores `1250` (cents); the resource sends `"12.50"`, using each asset's decimal places.

### 5.8 Responses (the JSON envelope)

[`app/http/responses/responses.go`](../app/http/responses/responses.go) makes every response the same shape:

```json
{"success": true,  "message": "Swap complete.", "data": {...}, "meta": {...}}
{"success": false, "message": "You don't have enough balance for this.", "code": "INSUFFICIENT_FUNDS"}
```

| Function | Status | Use for |
|---|---|---|
| `responses.OK` | 200 | reads, simple updates |
| `responses.OKWithMeta` | 200 | lists with pagination in `meta` |
| `responses.Created` | 201 | something was created |
| `responses.Accepted` | 202 | started; it finishes later (e.g. a payout) |
| `responses.Error` | from the error | any failure |

`responses.Error` is the project's exception handler:

- `AppError` → its own status, code and message (plus field errors).
- Anything else → log the real error, send a generic **500 `SERVER_ERROR`**. Internal details never reach the user.

---

## Part 6: Worked example: a swap from tap to database

This follows one real request through every file. The user wants to swap **$20 into British pounds**.

### Request 1: get a price (a quote)

```
POST /api/mobile/v1/quotes
Authorization: Bearer <access token>
X-Device-Imei: 3f9a...

{"transaction_type":"swap","from_asset":"USD","to_asset":"GBP","amount":"20"}
```

1. **Route** (`routes/mobile.go`): `signedIn.POST("/quotes", moneyController.CreateQuote)`. No `idempotent` here: creating a quote changes no money, and asking twice just gives two quotes.
2. **Middleware**: logger → token check → single-device check → rate limit.
3. **Controller** `CreateQuote` binds a `QuoteRequest` and calls `Rates.CreateQuote`.
4. **Service** [`app/services/rates/quotes.go`](../app/services/rates/quotes.go):
   - `PriceConversion` works out the price. [`router.go`](../app/services/rates/router.go) decides the **route**: one direct hop, or two hops through Naira (the "NGN rail"). [`pricing.go`](../app/services/rates/pricing.go) applies margins and the fee using `big.Rat` (exact fractions), rounding fees up and payouts down.
   - It **saves** a `quotes` row with `expires_at` = now + 30 seconds.
5. **Response**: the quote id, amounts as strings, the route, and `expires_at`.

The app shows "You pay $20.00, you get £15.03" with a countdown.

### Request 2: confirm the swap

```
POST /api/mobile/v1/swaps
Authorization: Bearer <access token>
X-Device-Imei: 3f9a...
Idempotency-Key: 6c1e2b9a-...        ← a new UUID, made when the user tapped "Confirm"

{"quote_id":"01JC...","pin":"2580"}
```

#### Step 1: route and middleware

`signedIn.POST("/swaps", idempotent, moneyController.Swap)`.

After the usual checks, `RequireIdempotencyKey` ([`idempotency.go`](../app/http/middleware/idempotency.go)) runs:

1. The key must look valid, or the request gets 400 `IDEMPOTENCY_KEY_REQUIRED`.
2. A Redis `SETNX` "front guard" rejects a second copy arriving while the first is still running (a double tap) with 409 `REQUEST_IN_PROGRESS`.
3. `Idempotency.Begin` inserts a row in `idempotency_keys` (UNIQUE per user + key).
   - If this key was **already completed**, the saved response is sent back with header `Idempotent-Replay: true` and **the controller never runs**.
   - Otherwise the new row's id is stored on the request.
4. It wraps the response writer in a **recorder**, so it can save a copy of whatever the controller sends.
5. `httpContext.Next()` runs the controller.

#### Step 2: controller

`MoneyController.Swap` binds `SwapRequest` and calls the service with:

- `middleware.CurrentActor(httpContext)`: the user id and device id from the token.
- `middleware.BusinessReference(httpContext, "SWP")`: `"SWP-<idempotency row id>"`. **This is the same on every retry of this request**, which matters in Step 4.

#### Step 3: service, before the transaction

Open [`app/services/swaps/service.go`](../app/services/swaps/service.go) and read `Swap`.

```go
if err := service.auth.VerifyPin(requestContext, actor.UserID, input.Pin); err != nil {
	return nil, err
}
```

The PIN is checked **outside** the database transaction on purpose. A wrong PIN increments a counter. If that happened inside a transaction that then rolled back, the wrong attempt would be forgotten and someone could guess forever. After 5 wrong tries the PIN locks for 15 minutes (see [`pins.go`](../app/services/auth/pins.go)).

```go
sourceWallet, err := service.ledger.EnsureUserAccount(requestContext, actor.UserID, ledger.CodeUserPrimary, preview.FromAsset)
targetWallet, err := service.ledger.EnsureUserAccount(requestContext, actor.UserID, ledger.CodeUserPrimary, preview.ToAsset)
```

This makes sure the user has a ledger account for USD and for GBP, creating one if needed. It also runs before the transaction, so the money transaction stays short.

#### Step 4: service, inside ONE transaction

```go
err = database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
	...
})
```

Everything inside the function is **all or nothing**: if it returns an error, every change is undone. Inside, in order:

| # | Code | What it does | Why |
|---|---|---|---|
| a | `shared.LockActor(databaseTransaction, actor)` | Re-reads the user and device rows with a **shared lock**. Fails if the user is frozen or this phone is no longer active. | The middleware checked this a few milliseconds ago, but a device switch could have happened since. The shared lock blocks a device switch until this payment finishes. |
| b | `service.rates.ConsumeQuote(...)` | `UPDATE quotes SET used_at = now WHERE id = ? AND used_at IS NULL AND expires_at > now`. Must change exactly 1 row. | A quote can be used **once**, and only before it expires. Two simultaneous confirms: MySQL lets one change the row; the other gets `QUOTE_EXPIRED`. |
| c | `service.ledger.PostConversion(...)` | Builds the journal legs and calls `ledger.Post`. | **This is where money moves.** It locks the ledger accounts, checks the balance, and inserts the legs (Part 8). Reference is `"SWP-<id>:POST"`. |
| d | `shared.RecordTransaction(...)` | Inserts the `transactions` row the user sees in Activity. | |
| e | `SaveSwapLegs(...)` | Inserts one `swap_legs` row per hop (USD→NGN, NGN→GBP). | Shows the exact route and rates later. |
| f | `shared.Notify(...)` | Inserts an `inbox_messages` row: "Swap complete". | Inside the transaction, so a message exists **only if the money really moved**. |

If any step returns an error (for example `INSUFFICIENT_FUNDS` from `Post`), the function returns it, MySQL rolls back, the quote stays unused, and no money moves.

If MySQL reports a **deadlock**, `WithTransaction` waits a few milliseconds and runs the whole function again (up to 3 times).

#### Step 5: ledger posting

`ledger.Post` in [`posting.go`](../app/services/ledger/posting.go):

1. Validates the legs (at least two, every amount above zero).
2. Inserts the journal entry header. `reference` is UNIQUE. If `"SWP-<id>:POST"` already exists, it returns the old entry and posts nothing new. **A retry can never move money twice.**
3. Locks every ledger account in the legs with `SELECT ... FOR UPDATE`, **in ascending id order** (to avoid deadlocks).
4. Checks that debits equal credits **for each currency**.
5. For each user wallet that goes down, computes the balance and refuses with `INSUFFICIENT_FUNDS` if it would go below zero.
6. Inserts the legs. Legs are never updated or deleted.

#### Step 6: response, and saving it

The controller sends `201 Created` with the transaction resource. On the way back out, `RequireIdempotencyKey` saves the status and body against the key. If the phone lost signal and retries with the same key, it gets this exact response again.

(If the response was a 5xx, the key is **released** instead of saved, so a retry runs the work again. That is safe because of the journal reference in step 5.)

#### Step 7: after the response

A few seconds later, the scheduler's **push outbox** finds the new inbox message and queues a push notification job (Part 10). The worker sends it.

#### The whole picture

```
Phone ──POST /swaps──▶ Gin router
                       ├─ RequestID, Cors                      (global)
                       ├─ RequestLogger                        (api group)
                       ├─ AuthenticateUser, EnsureSingleDevice,
                       │  RateLimit                            (signedIn group)
                       ├─ RequireIdempotencyKey                (route)
                       │     └─ replay? ──yes──▶ saved response
                       └─ MoneyController.Swap
                             ├─ requests.Bind(SwapRequest)
                             └─ Swaps.Swap
                                   ├─ VerifyPin                        (own query)
                                   ├─ EnsureUserAccount ×2             (own queries)
                                   └─ WithTransaction ┐
                                        LockActor     │ all or
                                        ConsumeQuote  │ nothing
                                        ledger.Post   │
                                        RecordTransaction
                                        SaveSwapLegs  │
                                        Notify        ┘
                       ◀── 201 + saved under the Idempotency-Key
Scheduler (3 s later) ── inbox row ──▶ push job ──▶ Worker sends push
```

---

## Part 7: The database layer: GORM, transactions and locks

### 7.1 Connecting

[`database/connection.go`](../database/connection.go):

- `Connect` opens a **connection pool** (up to 25 open connections) shared by every request. In PHP each request opens its own connection; here connections are reused.
- Every time is stored in UTC with microseconds. MySQL is forced into the same strict `sql_mode` on 5.7 and 8.4.
- **`CheckDatabaseName`** refuses any database except `tribe_core` and `tribe_core_test_*`. The local MAMP server hosts other apps' databases, and this guard makes it impossible to touch them by mistake. Do not loosen it.

### 7.2 GORM compared with Eloquent and the query builder

GORM queries are a chain of calls on a `*gorm.DB`, finishing with a call that runs the SQL. You **pass a pointer** for GORM to fill, and you **read `.Error`** at the end.

| Laravel | GORM in this project |
|---|---|
| `Quote::where('id', $id)->firstOrFail()` | `database.Where("id = ?", id).Take(&quote).Error` |
| `Asset::where('enabled', true)->orderBy('display_order')->get()` | `database.Where("enabled = ?", true).Order("display_order").Find(&assets).Error` |
| `Transaction::create([...])` | `database.Create(&transaction).Error` |
| `User::where('id', $id)->update([...])` | `database.Model(&models.User{}).Where("id = ?", id).Updates(map[string]any{...}).Error` |
| `->count()` | `.Count(&transactionCount)` |
| `->lockForUpdate()` | `database.LockForUpdate(...)` (our helper) |
| `->sharedLock()` | `database.LockForShare(...)` (our helper) |
| `DB::statement($sql, $bindings)` | `.Exec(sql, arguments...)` |
| `ModelNotFoundException` | `database.IsNotFound(err)` |
| affected rows | `result.RowsAffected` |

Notes:

- `Take` gets one row with no ordering; `Find` gets a list (or zero rows without an error).
- `?` placeholders are always used. Never build SQL by gluing strings with user input.
- **Every query is given the context**: `service.database.WithContext(requestContext)`, so a cancelled request stops its queries.
- Inside a transaction you use `databaseTransaction` (the `*gorm.DB` given to your function), **not** `service.database`. Using `service.database` inside the transaction function would run that query on a different connection, outside the transaction. This is the most common beginner mistake.

### 7.3 Transactions: `database.WithTransaction`

[`database/locking.go`](../database/locking.go):

```go
err := database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
	// every query here uses databaseTransaction
	return nil   // nil = COMMIT, an error = ROLLBACK
})
```

It is `DB::transaction(function () { ... })` with two extras:

1. **Isolation level READ COMMITTED.** Each `SELECT` sees the latest committed data. Locked rows stay locked until the end.
2. **Automatic retry on deadlock (MySQL 1213) or lock wait timeout (1205).** It waits 20–80 ms with some randomness and runs the whole function again, up to 3 times.

Because the function **may run more than once**, do not put anything inside it that cannot be repeated, such as calling a provider's API or queueing a job. Do that before or after.

### 7.4 Row locks, in plain words

When two requests try to change the same thing at the same moment, you get a **race condition**. Example: a wallet has $10, and two $8 payments arrive together. Both read "$10", both think it is enough, and the wallet ends at −$6.

The cure is a **row lock**: "I am working on this row; anyone else who wants to lock it must wait until I commit."

| Helper | SQL | Who else can lock the row | Used for |
|---|---|---|---|
| `database.LockForUpdate` | `SELECT ... FOR UPDATE` | nobody: they wait | rows you are about to change (cards, quotes, admin edits, ledger accounts in `Post`) |
| `database.LockForShare` | `SELECT ... LOCK IN SHARE MODE` | other **shared** lockers can; **exclusive** lockers wait | "this must not change while I work" (the user and device in `LockActor`) |

`LOCK IN SHARE MODE` is written out by hand because MySQL 5.7 does not understand the newer `FOR SHARE`.

**Lock order prevents deadlocks.** If request 1 locks Alice then Bob, and request 2 locks Bob then Alice, each holds one lock and waits for the other forever. MySQL detects this, kills one and reports a deadlock. The rule: **always lock in the same order.** `ledger.Post` locks accounts by ascending id. If a service locks other rows (a card, a quote, a funding request), it does so **before** calling `Post`, in the same order everywhere.

### 7.5 Unique indexes as safety nets

Several "do it only once" promises are enforced by a **UNIQUE index**, so the database itself refuses a duplicate even if two requests race:

| Table / column | Promise |
|---|---|
| `journal_entries.reference` | a business event posts once |
| `journal_entries.reverses_entry_id` | an entry is reversed at most once |
| `idempotency_keys` (actor, key) | a request key is remembered once |
| `user_devices.active_user_id` | one active phone per user |
| `scheduled_job_runs` (job, window) | a cron job runs once per window |
| webhooks (provider, event id) | a provider event is handled once |

`database.IsDuplicateEntry(err)` recognises MySQL error 1062, so code can say "already done, that's fine".

### 7.6 Migrations

Migrations are plain `.sql` files in [`database/migrations/`](../database/migrations), run by **goose**:

```sql
-- +goose Up
CREATE TABLE users ( ... );

-- +goose Down
DROP TABLE users;
```

- Files run in number order (`00001_...`, `00002_...`), each once. Goose records which have run, like Laravel's `migrations` table.
- [`embed.go`](../database/migrations/embed.go) uses `//go:embed *.sql` to **build the SQL files into the program**, so a server binary can migrate without the source folder next to it.
- There is no schema builder (`Schema::create`). You write SQL directly.

Rules (from `CLAUDE.md`), because the SQL must run on **both MySQL 5.7 and 8.4**:

- No CHECK constraints, no `SKIP LOCKED`/`NOWAIT`, no CTEs or window functions, no functional indexes, no `JSON_TABLE`.
- Ids are `CHAR(26) CHARACTER SET ascii COLLATE ascii_bin` (ULIDs); times are `DATETIME(6)`.
- **Never edit a migration that has run.** Add a new numbered file.

| Laravel | Here |
|---|---|
| `php artisan make:migration` | create `database/migrations/000NN_describe_it.sql` by hand |
| `php artisan migrate` | `make migrate` |
| `php artisan migrate:rollback` | `make rollback` |
| `php artisan migrate:status` | `go run ./cmd/artisan migrate:status` |
| `php artisan migrate:fresh --seed` | `make fresh` (only on a `tribe_core_test_*` database) |

### 7.7 Seeders

[`database/seeders/seeders.go`](../database/seeders/seeders.go) runs a list of steps in order: providers and assets, company ledger accounts, pricing, catalogues, card designs, first rates, the admin team, demo customers. `insertMissing` skips rows that already exist, so seeding twice is safe. Run with `make seed`.

---

## Part 8: Money and the ledger

This is the most important part of the codebase. Read [`ACCOUNTING.md`](ACCOUNTING.md) after this for every posting with numbers.

### 8.1 Money is never a float

```php
var_dump(0.1 + 0.2 == 0.3); // false in PHP
```

The same is true in Go. Floats cannot store most decimals exactly, so money would drift. [`pkg/money/money.go`](../pkg/money/money.go) stores every amount as a **whole number of the smallest unit**:

| Asset | Smallest unit | `$12.50` / `₦12.50` / `0.5 BTC` is stored as |
|---|---|---|
| USD | cent | `1250` |
| NGN | kobo | `1250` |
| BTC | satoshi | `50000000` |

The type is `money.Amount`, which wraps `big.Int` (an integer of any size).

| Function | Example | Use when |
|---|---|---|
| `money.ParseDecimal(text, places)` | `ParseDecimal("12.50", 2)` → `1250` | reading what a person typed |
| `money.ParseUnits(text)` | `ParseUnits("1250")` → `1250` | reading a database column |
| `amount.UnitsString()` | `"1250"` | writing to a `DECIMAL(38,0)` column |
| `amount.Decimal(places)` | `"12.50"` | sending to the app |
| `Add`, `Subtract`, `Negate`, `IsNegative`, `IsPositive`, `IsZero` | | arithmetic and checks |

The flow is always: **text in → `Amount` for all maths → text out.** Database columns are `DECIMAL(38,0)`. JSON always carries strings.

**Exchange rates** are fractions, so they use `big.Rat` (an exact fraction, like 1540/1). Rounding happens only at the very end: **fees round up, payouts round down**, so rounding never costs Tribe money.

### 8.2 Double-entry in five minutes

Forget "a balance column that goes up and down". The ledger is a **journal**: a list of entries that is only ever added to.

- A **journal entry** is one business event ("Ada swapped $20 to £15.03").
- Each entry has two or more **legs**. Each leg **debits** or **credits** one **ledger account**.
- **For each currency in the entry, total debits = total credits.** Money is never created or lost; it only moves.

Every account has a **category**, and the category decides which side makes it go up (see the table at the top of [`accounts.go`](../app/services/ledger/accounts.go)):

| Category | Goes up with | Examples |
|---|---|---|
| asset | debit | Provider vaults (what each provider holds for Tribe) |
| liability | credit | **User wallets** (money Tribe owes the user) |
| equity | credit | Tribe Funds (Tribe's own share: fees, margins, top-ups) |
| expense | debit | Provider Expense |
| equity | credit | Treasury Funding (owners' money) |

The key point: **a user's wallet is a liability for Tribe**. When Ada receives money we **credit** her account; when she spends we **debit** it.

**Example: Ada sends Bola $5 with a $0.10 fee** (`InternalTransferLegs` in [`rules.go`](../app/services/ledger/rules.go)):

| Account | Debit | Credit |
|---|---|---|
| Ada's USD wallet | 5.10 | |
| Bola's USD wallet | | 5.00 |
| Tribe Funds (USD) | | 0.10 |
| **Total** | **5.10** | **5.10** |

### 8.3 Posting rules

[`app/services/ledger/rules.go`](../app/services/ledger/rules.go) holds small **pure functions**: given account ids and amounts, they return the legs. They never touch the database, so they are easy to read and test.

```go
func DepositLegs(companyVault uint64, userWallet uint64, amount money.Amount) []Leg {
	return []Leg{
		{AccountID: companyVault, Side: Debit, Amount: amount, Memo: "Money received"},
		{AccountID: userWallet, Side: Credit, Amount: amount, Memo: "Deposit"},
	}
}
```

Rules include `DepositLegs`, `TreasuryFundingLegs`, `InternalTransferLegs`, `PayoutLegs`, `PayoutFailedLegs`, `VaultTransferLegs`, `ReferralRewardLegs`, `FeeLegs`, `MoveLegs` and, in [`conversion.go`](../app/services/ledger/conversion.go), `ConversionLegs` for swaps.

[`rules_test.go`](../app/services/ledger/rules_test.go) proves **every rule balances per currency**. A new rule needs a case there **and** a note in `ACCOUNTING.md`.

### 8.4 `ledger.Post`: the only door money goes through

**Every balance change goes through `ledger.Post`, inside `database.WithTransaction`.** No code ever writes a balance.

The steps are listed in Part 6, Step 5. Two outcomes to remember:

- Same reference posted twice → the second call returns the first entry with `AlreadyPosted: true`. Nothing new is written.
- A user wallet would go negative → `apperrors.ErrInsufficientFunds`, and the whole transaction rolls back.

**Undoing** an entry never deletes anything. `ledger.Reverse` posts the exact opposite legs, with reference `<original>:REVERSAL`.

### 8.5 Where does a balance come from?

There is **no balance column**. A balance is calculated by `ledger.BalanceOf` in [`balances.go`](../app/services/ledger/balances.go):

```
balance = the latest daily snapshot's closing balance
        + every leg posted AFTER that snapshot
```

**Example:**

- Last night's snapshot for Ada's USD wallet: closing balance `$40.00`, last leg counted was #1000.
- Today: leg #1003 credits `$5.00`; leg #1010 debits `$2.50`.
- Balance = 40.00 + 5.00 − 2.50 = **$42.50**.

Every night at **00:05 Lagos time**, the scheduler writes a new snapshot for every account ([`snapshots.go`](../app/services/ledger/snapshots.go)), so the "legs after" part stays short and fast. At **00:30**, `ledger:verify` checks that every entry balances and every snapshot equals the full sum of its legs. You can run that yourself with `make verify`.

### 8.6 Account codes

Ledger accounts are found by **code + asset** (and user, for user accounts). The codes are constants in [`accounts.go`](../app/services/ledger/accounts.go):

| Code | Belongs to | Meaning |
|---|---|---|
| `user_primary` | a user | the user's wallet for one asset |
| `user_card` | a user | balance loaded on one virtual card |
| `user_referral` | a user | referral earnings waiting to be withdrawn |
| `user_cashpoint` | a user | Naira set aside for the user's unredeemed cashpoint codes |
| `provider_vault` | a provider | what that provider holds for Tribe in one currency |
| `tribe_funds` | Tribe | Tribe's own share of the money at the providers: fees, margins, top-ups; credits and referral rewards are paid from it |

`ledger.EnsureUserAccount` creates a user account the first time it is needed. `ledger.CompanyAccount` finds a company account inside a transaction.

---

## Part 9: Making things safe to retry and safe to race

Phones lose signal, users double-tap, two admins edit the same row, and two phones sign in at once. This part explains each guard.

### 9.1 Idempotency: four layers

**The problem:** the user taps "Send", the signal drops, and the app does not know if the money went. It must be able to retry **without paying twice**.

**The contract:** the app makes a new UUID when the user taps, sends it as `Idempotency-Key`, and **reuses the same key** for every retry of that tap.

| Layer | Where | Stops |
|---|---|---|
| 1. Redis `SETNX` front guard (30 s) | [`middleware/idempotency.go`](../app/http/middleware/idempotency.go) | a second copy arriving **while the first is still running** → 409 `REQUEST_IN_PROGRESS` |
| 2. `idempotency_keys` row, UNIQUE(actor, key) | [`services/idempotency/service.go`](../app/services/idempotency/service.go) | a retry **after** the first finished → the saved response is replayed |
| 3. Journal reference built from the key, UNIQUE | `ledger.Post` | any bug that slips past 1 and 2 → the money still moves once |
| 4. Single-use quotes | `rates.ConsumeQuote` | the same price being used twice |

Extra rules in layer 2:

- Same key, **different body** → 422 `IDEMPOTENCY_KEY_REUSED` (that is a bug in the app).
- A 5xx response is **not** saved, so a retry runs the work again (layer 3 keeps it safe).
- The hourly cleanup deletes keys older than 7 days.

**Every POST/PATCH/PUT/DELETE that changes state uses the `idempotent` middleware**, in mobile and admin. And every money service builds its journal reference from `middleware.BusinessReference(httpContext, "PREFIX")`, never from a fresh random id. A fresh id would be different on each retry and would defeat layer 3.

### 9.2 Lock → re-read → check → write (and the `version` column)

When an admin edits something, two admins might edit it at once. Without care, the second save silently overwrites the first.

The pattern, from `SetCustomerStatus` in [`backoffice/customers.go`](../app/services/backoffice/customers.go):

```go
err := database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
	// 1. LOCK the row and RE-READ it (nobody else can change it until we finish).
	if err := database.LockForUpdate(databaseTransaction, &before, "id = ?", customerID); err != nil {
		return apperrors.NotFound("Customer")
	}
	// 2. CHECK the version the admin was looking at is still the current one.
	if version != 0 && version != before.Version {
		return apperrors.ErrStaleData                       // 409: "Someone else changed this"
	}
	// 3. CHECK business rules against the fresh row.
	if before.Status == "closed" { return apperrors.New(...) }
	// 4. WRITE, and bump the version.
	return databaseTransaction.Model(&models.User{}).Where("id = ?", customerID).
		Updates(map[string]any{"status": status, "version": before.Version + 1, ...}).Error
})
```

The admin panel sends back the `version` it loaded. If the row is now at a newer version, the admin sees `409 STALE_DATA` and refreshes. This is **optimistic locking**, plus a real row lock so the check and the write cannot be separated.

### 9.3 One active phone per user, enforced by the database

Tribe allows one active phone per account. This is enforced in three places:

1. **The database.** `user_devices.active_user_id` is a **generated column** that equals `user_id` only while the device's status is `active`, and is NULL otherwise. It has a UNIQUE index. MySQL allows many NULLs but only one copy of each user id, so **two active phones for one user is impossible**, even if two sign-ins race.
2. **Every signed-in request.** `EnsureSingleDevice` checks the session is still valid, its phone is still active, and the `X-Device-Imei` header matches. A replaced phone gets `401 DEVICE_NOT_ACTIVE`.
3. **Inside every money transaction.** `shared.LockActor` re-checks the device with a shared lock. A device switch needs an exclusive lock on the same rows, so it waits for in-flight payments to finish, and a payment cannot sneak through mid-switch.

Signing in on a new phone emails a code. Verifying it, in one transaction, marks the old phone `replaced`, revokes its sessions and activates the new one.

### 9.4 The PIN

Every money route needs the 4-digit PIN. `auth.VerifyPin` ([`pins.go`](../app/services/auth/pins.go)) compares it with a hash. Wrong attempts are counted with one atomic `UPDATE ... SET failed_attempts = failed_attempts + 1`, and after `PIN_MAXIMUM_ATTEMPTS` (5) the PIN locks for `PIN_LOCK_SECONDS` (15 minutes). As explained in Part 6, this runs **outside** the money transaction so failed attempts are never rolled back.

---

## Part 10: Work that happens outside the request

### 10.1 Queues (the worker)

Some work must not make the user wait: emails, provider payouts, statements, push notifications. Like Laravel queues, the API puts a small **job** into Redis and returns; a separate program, the **worker**, runs it.

The library is **asynq**. Two files:

**[`app/jobs/dispatcher.go`](../app/jobs/dispatcher.go)**: the "dispatch" side, used by the API.

```go
const TypeProcessPayout = "payouts:process"                  // the job name

type ProcessPayoutPayload struct {                            // the job's data (like a job class's properties)
	TransactionID string `json:"transaction_id"`
}

func (dispatcher *Dispatcher) ProcessPayout(requestContext context.Context, transactionID string) {
	dispatcher.enqueue(requestContext, TypeProcessPayout, ProcessPayoutPayload{TransactionID: transactionID},
		asynq.Queue(QueueCritical), asynq.MaxRetry(10), asynq.TaskID("payout:"+transactionID))
}
```

- `asynq.Queue(...)`: which queue (`critical`, `default`, `low`), like `->onQueue()`.
- `asynq.MaxRetry(10)`: like `$tries`. Retries wait longer each time.
- `asynq.TaskID(...)`: a unique id, so queueing the same payout twice is ignored, like `ShouldBeUnique`.

**[`app/jobs/handlers/handlers.go`](../app/jobs/handlers/handlers.go)**: the "handle" side, run by the worker.

```go
jobRouter.HandleFunc(jobs.TypeProcessPayout, processPayout(application))  // job name → handler
```

Each handler decodes the JSON payload and calls a service. Return `nil` for success, an error to retry, or wrap with `asynq.SkipRetry` for "never retry this".

**[`cmd/worker/main.go`](../cmd/worker/main.go)** runs 10 jobs at once and picks from queues in a 6 : 3 : 1 ratio (critical : default : low).

**The golden rule: every job must be safe to run twice.** Redis may deliver a job again after a crash. So handlers check the current state first. `Transfers.ProcessPayout` returns immediately if the transaction is no longer `pending`, and re-checks inside a locked transaction before posting.

| Laravel | Here |
|---|---|
| `SendPayout::dispatch($id)` | `application.Dispatcher.ProcessPayout(requestContext, id)` |
| job class `handle()` | a handler function in `handlers.go` |
| `php artisan queue:work` | `bin/worker` |
| `failed_jobs` table, Horizon | asynq's Redis state, shown on the admin **Jobs** page (via `application.Inspector`) |

**Example: sending to a bank.** The request posts `PayoutLegs` (wallet → the provider vault that pays it out), saves a `pending` transaction, queues `ProcessPayout`, and returns. The worker asks the provider. On success it just completes the transaction. On a refusal, `PayoutFailedLegs` puts the money back in the vault and the user's wallet.

### 10.2 Cron (the scheduler)

[`app/console/schedule.go`](../app/console/schedule.go) is `Kernel::schedule()`. Each job has a name, a cron expression, a **window key** and a function.

**Cron expressions here have six fields, with seconds first:**

```
"0 5 0 * * *"   = second 0, minute 5, hour 0  → 00:05:00 every day
"0 0 * * * *"   = second 0, minute 0          → every hour on the hour
```

| Time (Lagos) | Job | Does |
|---|---|---|
| 00:05 | `ledger:daily-snapshot` | writes yesterday's closing balance for every account |
| 00:30 | `ledger:verify` | proves the books balance |
| every hour | `cleanup:hourly` | deletes old idempotency keys and expired quotes |
| 07:00 | `bills:recurring` | pays recurring bills that are due |

**The window key** names the period a run belongs to (for example `"2026-11-02"` for a daily job). Each run is recorded in `scheduled_job_runs` with UNIQUE(job, window). If two scheduler processes are running, both try to insert the row, only one succeeds, and only that one runs the job. Laravel's `->onOneServer()` does the same thing.

**[`cmd/scheduler/main.go`](../cmd/scheduler/main.go)** also runs two loops in goroutines:

- **The rate collector**, every 10 seconds. Only one scheduler may collect, so it takes a Redis lock (`cache.TryLock`), keeps extending it, and stops if it ever loses it. This is called **leader election**.
- **The push outbox**, every 3 seconds (next section).

### 10.3 The push outbox

Services write the inbox message **inside** their money transaction (`shared.Notify` / `shared.NotifyTransaction`). They do **not** queue the push themselves. Why?

- If they queued it before the commit and the transaction then rolled back, the user would get a push for money that never moved.
- If they queued it after the commit and the process crashed in between, the push would be lost.

Instead, `Account.PushPendingInboxMessages` ([`push_outbox.go`](../app/services/account/push_outbox.go)) runs every 3 seconds:

1. Find inbox messages with `pushed_at IS NULL`.
2. Queue a push job for each. The job id is the message id, so the same message is never pushed twice.
3. Set `pushed_at`.

This is the **transactional outbox pattern**: write "something to send" in the same transaction as the change, and let a separate loop send it.

### 10.4 Live updates (Server-Sent Events)

Phones show live rates. Admins see the support inbox update live. Both use **Server-Sent Events (SSE)**: one long HTTP response the server keeps writing to.

```
event: rates.updated
id: 13
data: {"version":13,"rates":{"BTC":{"usd":"67000.00","ngn":"103180000.00"}, ...}}

: heartbeat
```

How the rates reach every phone:

1. The scheduler's collector fetches rates, saves them, and **publishes one message** on the Redis channel `rates.updated`.
2. Every API server runs one `Hub` goroutine ([`app/events/hub.go`](../app/events/hub.go)) subscribed to that channel.
3. Each phone with an open `/rates/stream` connection has its own Go **channel** registered with the hub. The hub copies every message into every channel.
4. `MoneyController.RatesStream` loops with `select`, writing each event to the HTTP response and a heartbeat every 15 seconds.

A slow phone keeps only the **newest** rates event (buffer size 1), so it can never slow the others down. The support hub keeps up to 32 events, because each names a different thread.

This replaces Laravel broadcasting with Pusher or Reverb. Redis pub/sub plays the part of the broadcaster, and SSE plays the part of the WebSocket.

### 10.5 Webhooks

Providers call `POST /webhooks/:provider` to say things like "your payout was delivered". [`app/services/webhooks/service.go`](../app/services/webhooks/service.go) applies three checks:

1. **Signature.** The provider signs the raw body with a shared secret (HMAC-SHA256, from `WEBHOOK_SECRET_<PROVIDER_SLUG>`). A wrong signature is rejected before anything is read.
2. **Once only.** Each event is stored with UNIQUE(provider, event id); a repeat is ignored.
3. **Never trust the body for money.** A payout event only **re-queues the payout job**, and that job **asks the provider** for the real status before posting anything.

### 10.6 Providers and simulators

Every outside company (rates, payouts, bills, gift cards, card issuing) is an **interface** in [`providers.go`](../app/services/providers/providers.go). Today each has a clearly labelled **simulator** in [`simulator.go`](../app/services/providers/simulator.go) that returns placeholder answers. **Production refuses to start with a simulator.** When a real provider is chosen, you write one adapter that satisfies the interface, and the services do not change.

---

## Part 11: Admin panel specifics

The admin API is at `/admin-api`, registered in [`routes/admin.go`](../routes/admin.go).

### 11.1 Permissions

```go
can := middleware.RequirePermission
signedIn.GET("/customers", can("customers.view"), controller.Customers)
signedIn.PATCH("/customers/:id/status", can("customers.freeze"), idempotent, controller.SetCustomerStatus)
```

`can := middleware.RequirePermission` just gives the function a short local name so routes read like Laravel's `can:` middleware.

Every permission is listed in `adminauth.Catalog` in [`permissions.go`](../app/services/adminauth/permissions.go), with a key, a label, a group and a risk level (`view`, `action`, `sensitive`). Pages need a `*.view` key; buttons need action keys. A new permission must be added to the catalog **and** to the seeder presets.

### 11.2 The audit log

Admin changes call `AdminAuth.Audit(...)` with the action, the target, a description, and "before" and "after" values. The **Audit log** page shows them.

### 11.3 Two-person treasury funding

Putting company money into the vault is a big deal, so it needs two different admins:

1. Admin A calls `POST /treasury-fundings` (permission `inventory.topup`). This creates a **request**. No money moves.
2. Admin B calls `POST /treasury-fundings/:id/approve` (permission `inventory.approve`). `ApproveTreasuryFunding` in [`wallets/service.go`](../app/services/wallets/service.go) locks the request, checks it is still pending and was raised by **someone else**, and only then posts `TreasuryFundingLegs`. **Only approval moves money.**

---

## Part 12: Tests

### 12.1 How Go tests work

- Test files end in `_test.go` and sit next to the code they test (or in `tests/`).
- A test is a function named `TestSomething(test *testing.T)`. The project names the parameter `test`, not the usual `t`.
- `test.Errorf(...)` marks a failure and continues; `test.Fatalf(...)` marks a failure and stops this test.
- `go test ./...` runs every test in every package. There is no PHPUnit config file.

### 12.2 Table-driven tests

Go code often tests many cases with one loop over a table. From [`rules_test.go`](../app/services/ledger/rules_test.go):

```go
cases := []struct {
	name     string
	legs     []Leg
	accounts map[uint64]models.LedgerAccount
}{
	{"deposit", DepositLegs(1, 2, amount), usdOnly},
	{"internal transfer with fee", InternalTransferLegs(1, 2, 3, amount, fee), usdOnly},
	...
}
for _, testCase := range cases {
	test.Run(testCase.name, func(test *testing.T) {          // a named sub-test, like a PHPUnit data provider
		if err := checkBalancedPerAsset(testCase.legs, testCase.accounts); err != nil {
			test.Fatalf("does not balance: %v", err)
		}
	})
}
```

`[]struct{...}{...}` is an **anonymous struct** type made on the spot: a list of rows, each with a name and inputs. It is Go's version of a PHPUnit `@dataProvider`.

Because this file is in `package ledger` (the same package), it can call the private `checkBalancedPerAsset`.

### 12.3 Integration tests (Laravel Feature tests)

[`tests/testenv/testenv.go`](../tests/testenv/testenv.go) gives every test package a complete, real API:

1. Creates a brand-new MySQL database named `tribe_core_test_<random>`.
2. Runs every migration and the seeders.
3. Starts the real router in an in-memory HTTP server (`httptest.NewServer`).
4. Drops the database when the test ends (`test.Cleanup`).

Tests then talk to it **over HTTP, exactly like the phone does**:

```go
environment := testenv.Start(test)
sender := environment.RegisterUser(test, "sender50")
sender.TopUp(test, "USD", "100")
response := sender.Call(test, http.MethodPost, "/api/mobile/v1/sends", map[string]any{...}, testenv.NewKey())
```

[`tests/integration/concurrency_test.go`](../tests/integration/concurrency_test.go) proves the money promises under real concurrency. For example, `TestConcurrentSendsNeverOverspend` starts 50 goroutines that each send $3 from a $100 wallet **at the same moment**. Exactly 33 must succeed and the wallet must end at exactly `$1.00`.

```go
var waitGroup sync.WaitGroup
for attempt := 0; attempt < 50; attempt++ {
	waitGroup.Add(1)           // "one more goroutine to wait for"
	go func() {
		defer waitGroup.Done() // "this one has finished"
		...
	}()
}
waitGroup.Wait()               // block until all 50 have finished
```

`sync.WaitGroup` waits for a group of goroutines. `atomic.Int64` is a counter that many goroutines can increase safely.

Other integration tests check: opposite transfers never deadlock, one idempotency key sent 20 times posts once, a quote is used once, two phones taking over at once leaves one active, snapshots plus legs equal balances, and treasury funding needs a second admin.

**Add an integration test for anything that moves money or could race.**

### 12.4 Running tests

| Command | Runs |
|---|---|
| `make test-unit` | tests that need no database (`SKIP_DATABASE_TESTS=1`) |
| `make test` | everything, one package at a time (`TEST_P=1`), because MAMP's MySQL runs out of file handles if many 100-table databases are created at once |
| `go test ./app/services/ledger/ -run TestEveryRuleBalances -v` | one test, with output |

CI ([`.github/workflows/ci.yml`](../.github/workflows/ci.yml)) runs the suite on **MySQL 5.7 and 8.4**.

---

## Part 13: Day-to-day commands

The [`Makefile`](../Makefile) is the project's `composer run` scripts. `make help` lists them.

| Command | Does | Laravel equivalent |
|---|---|---|
| `make migrate` | creates `tribe_core` if missing, runs new migrations | `php artisan migrate` |
| `make seed` | lookup data, demo customers and admins | `php artisan db:seed` |
| `make fresh` | drop, migrate, seed. Only works when `DB_DATABASE` is a `tribe_core_test_*` database: dropping `tribe_core` itself is refused on purpose | `php artisan migrate:fresh --seed` |
| `make dev` | builds, then runs server + worker + scheduler; Ctrl+C stops all three | `serve` + `queue:work` + `schedule:work` |
| `make try` | guided curl tour of the API | — |
| `make test` / `make test-unit` | tests | `php artisan test` |
| `make vet` | `go vet` + gofmt check (must pass) | Pint / PHPStan |
| `make fmt` | format every file | `pint` |
| `make routes` | list routes | `php artisan route:list` |
| `make verify` | check the books | — |
| `make snapshot DATE=2026-10-23` | write snapshots for a day | — |

`go run ./cmd/artisan admin:create <email> <name> <password>` creates a super admin.

**Local conveniences** (only when `APP_ENV=local`): responses include `debug_code` for email and device codes, the worker prints "emails" to its log. Demo customers get test money from the seeders; there is no test top-up route any more (admins use manual credits).

**If you change Go code, restart.** Stop `make dev` with Ctrl+C and run it again; it rebuilds first.

---

## Part 14: Adding an endpoint, step by step

This walks through the checklist in `CLAUDE.md` with a small, made-up example: **"let a user rename a beneficiary"** (`PATCH /api/mobile/v1/beneficiaries/:id`). This endpoint does not exist yet; it is only a practice example. It changes data but moves no money, so it uses the lock → re-read → write pattern.

### 1. Request struct

In [`app/http/requests/requests.go`](../app/http/requests/requests.go):

```go
// RenameBeneficiaryRequest changes the name a user sees for a saved recipient.
type RenameBeneficiaryRequest struct {
	Name string `json:"name" binding:"required,min=1,max=100"`
}
```

### 2. Service method

In the right service package (here, `transfers`, which owns beneficiaries). The real `models.Beneficiary` has a `Name` field but **no `version` column**, so this example only locks and re-reads. If two people could edit the same row (an admin page, for example), you would first add a `version` column in a new migration and check it, exactly like `SetCustomerStatus` in Part 9.2.

```go
// RenameBeneficiary changes a saved recipient's display name. It locks the
// row first, so a rename and a delete arriving together cannot interleave.
func (service *Service) RenameBeneficiary(requestContext context.Context, userID string, beneficiaryID string, name string) (*models.Beneficiary, error) {
	var beneficiary models.Beneficiary
	err := database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		// Lock and re-read. "user_id = ?" makes sure users can only touch their own rows.
		if err := database.LockForUpdate(databaseTransaction, &beneficiary, "id = ? AND user_id = ?", beneficiaryID, userID); err != nil {
			return apperrors.NotFound("Beneficiary")
		}
		beneficiary.Name = name
		return databaseTransaction.Model(&models.Beneficiary{}).Where("id = ?", beneficiaryID).
			Update("name", name).Error
	})
	if err != nil {
		return nil, err
	}
	return &beneficiary, nil
}
```

If the endpoint **moved money**, the service would also: verify the PIN before the transaction, call `shared.LockActor` first inside it, lock other rows before posting, build legs with a rule from `rules.go`, call `ledger.Post` with a reference from `BusinessReference`, record the transaction, and call `shared.NotifyTransaction`.

### 3. Controller method

Short: bind, call, respond.

```go
// RenameBeneficiary  PATCH /beneficiaries/:id
func (controller *MoneyController) RenameBeneficiary(httpContext *gin.Context) {
	var input requests.RenameBeneficiaryRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	beneficiary, err := controller.application.Transfers.RenameBeneficiary(httpContext.Request.Context(),
		middleware.CurrentActor(httpContext).UserID, httpContext.Param("id"), input.Name)
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Beneficiary renamed.", beneficiary) // a real endpoint would shape this with a resource
}
```

### 4. Route

In [`routes/mobile.go`](../routes/mobile.go), in the right group, with `idempotent` because it changes state:

```go
signedIn.PATCH("/beneficiaries/:id", idempotent, moneyController.RenameBeneficiary)
```

For an admin route, also add `can("some.permission")`.

### 5. New admin permission?

Add it to `adminauth.Catalog` and to the seeder presets.

### 6. Integration test

If it moves money or could race, add a test in `tests/integration` using `testenv`.

### 7. Documentation

Add it to [`API.md`](API.md), and add a step to `scripts/try-the-api.sh` if it shows something new.

### 8. Check

```bash
make vet && make test
```

### "Where does this code belong?"

Ask one question:

| If the code is… | It goes in… |
|---|---|
| reading HTTP input or writing HTTP output | a controller (plus request / resource) |
| a business rule, a check, a lock, a transaction | a service |
| deciding which accounts to debit and credit | a posting rule in `ledger/rules.go` |
| a table's shape | a migration + a model |
| something that runs later or slowly | a job |
| something that runs on a clock | `app/console/schedule.go` |

---

## Part 15: Cheat sheet and glossary

### Laravel → this project

| Laravel | Tribe backend (Go) | File |
|---|---|---|
| `public/index.php` | `cmd/server/main.go` | [link](../cmd/server/main.go) |
| `.env` + `config()` | `config.Load()` → `settings.App.Port` | [link](../config/config.go) |
| Service container / providers | `bootstrap.New` | [link](../app/bootstrap/bootstrap.go) |
| `routes/api.php` | `routes/mobile.go`, `admin.go`, `system.go` | [link](../routes) |
| `Route::middleware()->group()` | `router.Group(path, middleware...)` | |
| `$request->input('x')` | `requests.Bind(httpContext, &input)` then `input.X` | [link](../app/http/requests/requests.go) |
| `$request->route('id')` | `httpContext.Param("id")` | |
| `$request->query('page')` | `httpContext.Query("page")` | |
| `$request->header('X')` | `httpContext.GetHeader("X")` | |
| `auth()->user()` | `middleware.CurrentActor(httpContext)` / `CurrentAdmin` | [link](../app/http/middleware/authentication.go) |
| FormRequest `rules()` | `binding:"..."` struct tags | |
| API Resource | `resources.Transaction(...)` | [link](../app/http/resources/resources.go) |
| `response()->json()` | `responses.OK/Created/Error` | [link](../app/http/responses/responses.go) |
| `abort(422, ...)` / HttpException | `return apperrors.New(422, "CODE", "message")` | [link](../app/apperrors/apperrors.go) |
| Eloquent model | struct in `app/models` | [link](../app/models) |
| `Model::find($id)` | `.Where("id = ?", id).Take(&model)` | |
| `DB::transaction(fn)` | `database.WithTransaction(ctx, db, fn)` | [link](../database/locking.go) |
| `->lockForUpdate()` | `database.LockForUpdate(...)` | |
| `->sharedLock()` | `database.LockForShare(...)` | |
| Migration class | `database/migrations/000NN_name.sql` | [link](../database/migrations) |
| `DatabaseSeeder` | `seeders.Run` | [link](../database/seeders/seeders.go) |
| `Job::dispatch()` | `application.Dispatcher.Something(...)` | [link](../app/jobs/dispatcher.go) |
| Job `handle()` | handler in `app/jobs/handlers` | [link](../app/jobs/handlers/handlers.go) |
| `Kernel::schedule` | `console.Jobs` | [link](../app/console/schedule.go) |
| `->onOneServer()` | UNIQUE(job, window) in `scheduled_job_runs` | |
| Broadcasting | Redis pub/sub + SSE `Hub` | [link](../app/events/hub.go) |
| `Cache::lock()` | `cache.TryLock` | [link](../app/cache/redis.go) |
| `RateLimiter` / `throttle` | `middleware.RateLimit` | [link](../app/http/middleware/common.go) |
| Gate / `can:` | `can("permission")` + `adminauth.Catalog` | [link](../app/services/adminauth/permissions.go) |
| Feature test | `tests/integration` + `testenv` | [link](../tests/testenv/testenv.go) |
| `php artisan` | `go run ./cmd/artisan` | [link](../cmd/artisan/main.go) |

### Go syntax at a glance

| Go | Means | PHP |
|---|---|---|
| `x := 5` | new variable | `$x = 5;` |
| `var x int` | new variable, zero value | `$x = 0;` |
| `func Name(a string) (int, error)` | function with two return values | `function name(string $a): int` (+ throws) |
| `func (service *Service) Do()` | method on `Service` | `public function do()` |
| `type User struct { Name string }` | a data type | a class with properties |
| `&value` | pointer to value | (objects are already handles) |
| `*string` | string or nil | `?string` |
| `[]string` | list | `array` (list) |
| `map[string]int` | dictionary | `array` (associative) |
| `any` | any type | `mixed` |
| `for _, item := range items {}` | loop | `foreach ($items as $item) {}` |
| `if err != nil { return err }` | stop on failure | `throw` (implicitly) |
| `defer f()` | run f when this function ends | `finally { f(); }` |
| `go f()` | run f in the background, now | — |
| `<-channel` | receive from a channel | — |
| `Name` vs `name` | exported vs package-private | `public` vs `private` |

### Glossary

| Term | Meaning |
|---|---|
| **Actor** | the signed-in user and their phone (`shared.Actor`) |
| **Business reference** | `PREFIX-<idempotency row id>`; the same on every retry; used as the journal reference |
| **Channel** | a typed pipe between goroutines |
| **Context** (`context.Context`) | the "is this request still alive?" signal passed into every service call |
| **Deadlock** | two transactions each waiting for a lock the other holds; prevented by locking in ascending order, and retried by `WithTransaction` |
| **Goroutine** | a function running at the same time as others in the same process |
| **Idempotent** | safe to repeat: doing it twice has the same effect as once |
| **Journal entry / leg** | one business event / one debit or credit line inside it |
| **Ledger account** | a bucket of money for one owner and one asset (a user's USD wallet, the company's NGN vault) |
| **NGN rail** | converting through Naira (USD → NGN → GBP), because providers price that way |
| **Outbox** | write "to be sent" in the same transaction; a separate loop sends it |
| **Quote** | a saved price, valid for 30 seconds, usable once |
| **Snapshot** | the closing balance of an account at the end of a day |
| **SSE** | Server-Sent Events: a long HTTP response the server keeps writing to |
| **ULID** | 26-character unique id that sorts by creation time |
| **Version column** | a counter bumped on every edit; a mismatch means someone else edited first (`409 STALE_DATA`) |

---

## Part 16: Check your understanding

Try to answer each question before opening it.

<details>
<summary>1. A Go service stores the current user in a field (<code>service.currentUser = user</code>) at the start of a request. What goes wrong?</summary>

The service is created **once** and shared by every request, and requests run at the same time. Request B would overwrite request A's user in the middle of A's work. Per-request data must be passed as function arguments (like `actor shared.Actor`), never stored on a shared service.
</details>

<details>
<summary>2. Why does <code>Swap</code> check the PIN outside <code>database.WithTransaction</code>?</summary>

A wrong PIN increments `failed_attempts`. If that happened inside a transaction that then rolled back (because the PIN was wrong and the function returned an error), the counter increase would be undone, and someone could guess PINs forever.
</details>

<details>
<summary>3. Inside a <code>WithTransaction</code> function, someone writes <code>service.database.Create(&row)</code>. What is wrong?</summary>

`service.database` is the connection pool, not the transaction. The insert runs on a different connection, outside the transaction: it is not rolled back on failure, and it cannot see uncommitted changes. Use `databaseTransaction.Create(&row)`.
</details>

<details>
<summary>4. The phone sends the same swap twice because of a timeout. List everything that stops the money moving twice.</summary>

1. If the first is still running: the Redis front guard returns 409 `REQUEST_IN_PROGRESS`.
2. If the first finished: the `idempotency_keys` row replays the saved response; the controller does not run.
3. If somehow both reach the service: the quote can only be consumed once (`QUOTE_EXPIRED` for the second), and the journal reference `SWP-<id>:POST` is UNIQUE, so `ledger.Post` returns the existing entry.
</details>

<details>
<summary>5. Why is there no <code>balance</code> column on wallets?</summary>

A balance column can be changed by mistake, can race, and leaves no history. Here the balance is **derived** from an insert-only journal (snapshot + legs since). Every cent can be traced to an entry, `ledger:verify` can prove the books are consistent, and concurrency is handled by locking the ledger account rows in `Post`.
</details>

<details>
<summary>6. Why must you never use <code>float64</code> for an amount?</summary>

Floats cannot store most decimal fractions exactly (`0.1 + 0.2 != 0.3`), so totals drift. Amounts are integers of the smallest unit (`money.Amount`), rates are exact fractions (`big.Rat`), and rounding happens once at the end: fees up, payouts down.
</details>

<details>
<summary>7. Request 1 sends Alice → Bob and request 2 sends Bob → Alice at the same moment. Why don't they deadlock?</summary>

`ledger.Post` locks the accounts in **ascending id order**. Both requests try to lock the lower id first, so one waits for the other at the first lock instead of each holding one and waiting for the other. `TestOppositeTransfersDoNotDeadlock` proves it.
</details>

<details>
<summary>8. Why do services write the inbox message inside the money transaction, but never queue the push directly?</summary>

Writing inside the transaction means the message exists only if the money moved. Queueing directly could send a push for a rolled-back payment, or lose the push if the process crashed after commit. The scheduler's outbox reads committed messages and queues each push once (job id = message id).
</details>

<details>
<summary>9. Two admins open the same customer and both click "Freeze". What happens?</summary>

Both send `version: 3`. The first locks the row, sees version 3, writes with version 4 and commits. The second was waiting on the lock; it then re-reads, sees version 4, and gets `409 STALE_DATA`.
</details>

<details>
<summary>10. A job handler calls a provider and then posts to the ledger. The worker crashes after the provider call but before the post. What protects the money?</summary>

The job is delivered again. The payout was posted once, when it started, so nothing is lost or doubled. The handler checks the transaction is still `pending` and calls the provider again with the **same reference** (a real provider adapter must treat a repeated reference as the same payout). It then finishes inside a transaction that **locks** the transaction row and re-checks it is still pending. A refund's journal reference (`<reference>:REFUND`) is UNIQUE, so even a duplicate cannot move money twice.
</details>

<details>
<summary>11. You added a column to <code>users</code>. Why doesn't it show in <code>GET /profile</code>?</summary>

The API returns `resources.UserResource`, not the model. Add the field to the resource on purpose. That is what stops secrets leaking by accident.
</details>

<details>
<summary>12. Where would you put "users under KYC tier 2 can't send more than $500 a day"?</summary>

In the service (for example `transfers`), as a business rule checked before posting. Not in the request struct (it needs the database) and not in the controller (controllers hold no business logic).
</details>

---

**Next steps:** run `make dev` and `make try`, and watch the three program logs while the tour runs. Then read [`ACCOUNTING.md`](ACCOUNTING.md) for every posting with numbers, and [`API.md`](API.md) for every endpoint.
