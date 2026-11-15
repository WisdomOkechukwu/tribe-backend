# Tribe backend: rules for Claude

Go 1.27 API behind two apps: `tribe-mobile` (Expo) and `tribe-admin` (Next.js). The code is written for a team that knows Laravel but is new to Go, so **clarity beats cleverness every time**. Read `docs/GO_FOR_BEGINNERS.md` for the tour, `docs/ACCOUNTING.md` for the books and `docs/API.md` for every endpoint.

## Folder map
| Folder | What lives there | Laravel equivalent |
|---|---|---|
| `cmd/server`, `cmd/worker`, `cmd/scheduler`, `cmd/artisan` | the four programs (plus `cmd/loadtest`, a load generator for disposable servers) | `artisan serve`, `queue:work`, `schedule:work`, `artisan` |
| `routes/` | `mobile.go` (`/api/mobile/v1`), `admin.go` (`/admin-api`), `system.go` (health, webhooks) | `routes/api.php` |
| `app/http/controllers/{mobile,admin,system}` | bind input, call ONE service, send the response | controllers |
| `app/http/middleware` | auth, single device, idempotency, permissions, rate limit, API trail | middleware |
| `app/http/requests` | input structs with `binding:` validation | FormRequest |
| `app/http/resources`, `responses` | output shapes and the JSON envelope | API Resources |
| `app/services/*` | all business logic (ledger, rates, swaps, transfers, products, backoffice, webhooks…) | services |
| `app/models` | GORM structs, one per table | Eloquent models |
| `app/jobs`, `app/console` | queue jobs (asynq) and cron (`schedule.go`) | jobs, `Kernel::schedule` |
| `app/events` | Redis pub/sub → SSE hubs (rates, support) | broadcasting |
| `database/migrations` | goose SQL migrations | migrations |
| `database/seeders` | lookup data, demo customers and admins | seeders |
| `tests/testenv`, `tests/integration` | real-database tests over HTTP | Feature tests |

## Naming and comments
- **Full words only.** Use `request`, `response`, `httpContext *gin.Context`, `databaseTransaction`, `walletBalance`, `transactionCount`. Never use `r`, `w`, `c`, `tx`, `ctx`, `res`, `req`, `mux` or `bal`.
  - The ONE allowed short name is `err`.
  - Loop variables are words too: `for _, account := range accounts`.
- Every exported function, type and constant has a plain-English comment above it that says **what it does and why**.
- Explain any non-obvious locking or SQL in a comment, written for someone who has never seen Go.
- No emojis in code or log lines.
- Use no generics or reflection tricks beyond what is already there. Write SQL that locks rows explicitly.

## Request flow (do not skip steps)
Route → Middleware → Controller → Request (validation) → Service → Model/DB → Resource → `responses.OK/Created/Error`.
Controllers never touch the database for business logic. Services never read `gin.Context`.

## Money rules (non-negotiable)
1. **Money is never `float64`.**
   - Use `pkg/money.Amount`: integer smallest units, stored as `DECIMAL(38,0)` and sent as strings in JSON.
   - Rates use `big.Rat`. Round only at the end:
     - fees round UP
     - payouts to users round DOWN
2. **Every balance change goes through `ledger.Post`** inside `database.WithTransaction`, with legs built by a rule in `app/services/ledger/rules.go`.
   - Never UPDATE a balance column; balances are computed by `ledger.BalanceOf`.
   - `journal_legs` is insert-only.
3. **Every journal reference is stable across retries**, built from `middleware.BusinessReference(httpContext, "SWP")`, so a retry can never post twice.
4. **Lock order:** `ledger.Post` locks ledger accounts by ascending id. If you lock other rows (user, card, quote, funding), lock them BEFORE calling Post, and always in the same order everywhere.
5. Money routes re-check the device inside the transaction (`shared.LockActor`) and need a quote (when currencies differ) plus the PIN.
6. Write the inbox message (`shared.Notify` / `shared.NotifyTransaction`) inside the same transaction. The scheduler's push outbox sends the push.
7. Treasury funding is two-person: one admin requests, a **different** admin approves, and only approval posts.
   - Manual customer credits & debits (`customer_adjustments`) are two-person too, and only a **super admin** (every permission, `adminauth.IsSuperAdmin`) may approve. A credit is paid from the currency's **Tribe Funds** and must fit its balance; a debit must fit the wallet.
   - **The providers hold the money; the ledger mirrors them.** Only three kinds of account exist: provider vaults (`provider_vault`, one per provider per currency in `provider_assets`), customer balances, and **Tribe Funds** (`tribe_funds`, one per currency). Per currency, Tribe Funds = Σ provider vaults − Σ owed to customers; `assertBooksBalance` checks it in every integration test. Never add another company account.
   - Money leaves a provider vault when the provider is instructed (payouts, bills, gift cards); a refusal reverses or refunds it. Use the currency's home vault (`ledger.HomeVault`) unless the product has its own provider (`products.productVault`). Card money also moves to the card issuer's vault (`cardMoneyLegs`).
   - Every fee and conversion margin lands in Tribe Funds, tagged with `Leg.Source`. On every route that is not the NGN rail, pass `MarketReceived` (from the quote or price) so the margin is booked.
   - An invited customer's money flow calls `referrals.Prepare` BEFORE its transaction, sets `ReferralShare: invitation.ShareOf()` on the posting, and `referrals.Record` after it. Flows a provider must confirm pay the share when it confirms (`referrals.PayAccrued`) and cancel it on refusal (`referrals.Cancel`). See `docs/ACCOUNTING.md`.
8. New posting rules need a case in `app/services/ledger/rules_test.go` proving they balance per asset, and a note in `docs/ACCOUNTING.md`.
   - Gift-card and cashpoint postings are still **pending accountant sign-off**.

## Changing information safely
- Every POST/PATCH/PUT/DELETE that changes state uses the `idempotent` middleware. This applies to mobile AND admin.
- Edits follow lock → re-read → check → write: `database.LockForUpdate`, plus a `version` column check, which returns `apperrors.ErrStaleData` (409) when two people edit at once.
- Use `database.WithTransaction`. It runs at READ COMMITTED and retries deadlocks.

## Database (MySQL 5.7 locally, 8.4 in production)
- Migrations must run on BOTH versions. That rules out:
  - CHECK constraints
  - `SKIP LOCKED` / `NOWAIT`
  - CTEs or window functions
  - functional indexes
  - the `JSON_TABLE` function
- Use `DATETIME(6)` and `CHAR(26) ascii_bin` ULIDs.
- New migration = a new numbered file in `database/migrations` with `-- +goose Up` / `-- +goose Down`. Never edit a migration that has run.
- **Only ever touch the `tribe_core` database** (and throw-away `tribe_core_test_*`). The MAMP server also hosts unrelated apps (`tribe_live`, `tribe_server`, `dan_gen`, …).
  - Never read, write, migrate or drop them.
  - `database.CheckDatabaseName` enforces this. Do not loosen it.

## Providers
- Every provider is an interface in `app/services/providers` with a clearly labelled **simulator**. Production refuses simulators.
- Webhooks arrive at `POST /webhooks/:provider`. They are HMAC-signed and stored once, and they only re-trigger jobs that ask the provider for the truth.

## Adding an endpoint (checklist)
1. Request struct in `app/http/requests` with `binding:` rules.
2. Service method holding the logic: locks, `ledger.Post`, `NotifyTransaction`.
3. Controller method that is short: bind, call, respond.
4. Route in `routes/*.go` with the right group, `idempotent` for changes, and `can("permission")` for admin.
5. New admin permission? Add it to `adminauth.Catalog` and to the seeder presets.
6. Integration test in `tests/integration` if it moves money or has a race.
7. Document it in `docs/API.md`, and add a step to `scripts/try-the-api.sh` if it shows something new.

## Running things
- Node is not used here. Go is at `/usr/local/go`.
- MAMP MySQL socket: `/Applications/MAMP/tmp/mysql/mysql.sock`. Redis: `127.0.0.1:6379`.
- If Go's checksum database flakes, build with `GOFLAGS=-mod=mod`. The Makefile already does this.
- Commands:
  - `make migrate && make seed`
  - `make dev` (server, worker and scheduler)
  - `make try`
  - `make test`: one package at a time (`TEST_P=1`), because MAMP runs out of file handles otherwise
  - `make vet` must pass, including gofmt
  - `make loadtest-seed` / `make loadtest URL=...`: disposable servers only (`docs/LOAD_TESTING.md`)
- Local only: the API returns `debug_code` for email and device codes, and the worker prints emails to its log. Neither happens outside `APP_ENV=local`.

## Deferred on purpose
- Settlement, payroll and period close have placeholder tables only.
- Real providers (FX, crypto, bills, gift cards, card issuing), email/SMS/push delivery and KYC vendors are not built yet.
