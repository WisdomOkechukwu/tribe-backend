# Tribe backend

The Go API behind the Tribe mobile app (`tribe-mobile`) and the admin panel (`tribe-admin`). It has:

- a double-entry ledger (journal entries, legs and daily snapshots)
- single-phone sign-in
- idempotent money routes
- live rates over Server-Sent Events, with the NGN rail for conversions
- queues and cron
- two-person treasury approvals

| Read this | For |
|---|---|
| [`docs/GO_FOR_BEGINNERS.md`](docs/GO_FOR_BEGINNERS.md) | Go explained for Laravel developers, following one request through the code |
| [`docs/STUDY_GUIDE.md`](docs/STUDY_GUIDE.md) | The full study guide: the Go language, every layer, the ledger, safety patterns, jobs, tests, with questions to check yourself |
| [`docs/API.md`](docs/API.md) | Every endpoint, headers, errors, SSE and webhooks |
| [`docs/ACCOUNTING.md`](docs/ACCOUNTING.md) | The chart of accounts and every posting, with numbers |
| [`CLAUDE.md`](CLAUDE.md) | House rules: naming, money, locking, migrations, adding an endpoint |

## What you need
- **Go 1.27**
- **MySQL 5.7 or 8.4.** Locally this is MAMP's MySQL through its socket. Production uses 8.4.
- **Redis 7 or newer**, for the cache, locks, queue and live events.
- `jq`, `openssl` and `uuidgen`, for the curl tour.

## Run it

```bash
cp .env.example .env          # then check the DB_* and REDIS_* values
make migrate                  # creates the tribe_core database and its tables
make seed                     # currencies, providers, card designs, demo customers and admins
make dev                      # server + worker + scheduler; Ctrl+C stops all three
```

The API listens on `APP_PORT` (default 8080). Run a different port with `APP_PORT=8082 ./bin/server`.

In a second terminal:

```bash
make try                      # guided curl tour: sign-up, top-up, NGN-rail swap, replay, send,
                              # second phone, treasury approval, webhooks, live rates
```

| Program | Laravel equivalent | What it does |
|---|---|---|
| `bin/server` | `php artisan serve` | HTTP API (mobile, admin, webhooks, SSE) |
| `bin/worker` | `php artisan queue:work` | Queued jobs: emails, payouts, statements, push notifications |
| `bin/scheduler` | `php artisan schedule:work` | Cron (snapshots, cleanup, recurring bills, ledger check), the rate collector every 10 s, and the push outbox |
| `bin/artisan` | `php artisan` | `migrate`, `migrate:rollback`, `migrate:status`, `migrate:fresh`, `db:seed`, `ledger:snapshot`, `ledger:verify`, `admin:create`, `route:list` |

**Demo logins** (local only, password `Tribe@2026`):

- Admins: `super@`, `ops@`, `support@`, `compliance@`, `finance@` and `viewer@tribe.africa`.
- Customers: see `database/seeders/seeders.go`.

**Local conveniences** (only when `APP_ENV=local`):

- The API returns `debug_code` for email and new-phone codes, and the worker prints the "email" to its log.
- Customers get money from admins: `POST /admin-api/customers/{id}/adjustments` (credit or debit), approved by a super admin, limited by the vault's free funds (`GET /admin-api/free-funds`). The seeders still give demo customers test money.

## Connect the apps

| App | Setting |
|---|---|
| tribe-mobile | `EXPO_PUBLIC_API_URL=http://localhost:8080` (Android emulator: `http://10.0.2.2:8080`) |
| tribe-admin | `TRIBE_API_URL=http://localhost:8080`, and add the admin's origin to `CORS_ALLOWED_ORIGINS` |

## Tests

```bash
make vet                      # gofmt + go vet
make test-unit                # no database needed
make test                     # everything; each test package gets its own tribe_core_test_* database
```

The integration tests (`tests/integration`) check the promises that matter for money:

- 50 concurrent sends never overspend.
- Opposite transfers at the same moment never deadlock.
- One idempotency key sent 20 times posts once.
- A quote is used once.
- Two phones taking over at once leaves one active.
- Snapshots plus legs equal balances.
- Treasury funding needs a second admin.

Tests run one package at a time (`TEST_P=1`) because MAMP's MySQL runs out of file handles otherwise. CI (`.github/workflows/ci.yml`) runs the suite on **MySQL 5.7 and 8.4**.

## Safety rails
- The backend only touches the `tribe_core` database (and throw-away `tribe_core_test_*` ones). Any other name is refused, because the MAMP server also hosts unrelated apps.
- Every provider is a clearly labelled **simulator**, and production refuses to start with one.
- Webhook secrets come from `WEBHOOK_SECRET_<PROVIDER_SLUG>`.
- `TRUSTED_PROXIES` lists the load balancers allowed to set `X-Forwarded-For`. Leave it empty when there is none.

## Not built yet
- Real providers (FX, crypto, bills, gift cards, card issuing), plus email, SMS and push delivery and KYC vendors.
- Settlement, payroll and period close (placeholder tables only).
- Accountant sign-off on the gift-card and cashpoint postings.
